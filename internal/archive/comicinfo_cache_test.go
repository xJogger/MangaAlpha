package archive

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/cache"
	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
)

func TestReaderCachesComicInfoAndSkipsParsedVersion(t *testing.T) {
	const xml = `<ComicInfo><Title>Embedded title</Title><Summary>Embedded summary</Summary>
<Number>2</Number><Year>2025</Year><Month>2</Month><Day>1</Day><ISBN>9784088820001</ISBN>
<Writer>Embedded writer</Writer><Tags>Embedded tag</Tags><Web>https://example.com/embedded</Web>
<Series>Embedded series</Series><Publisher>Embedded publisher</Publisher><SeriesGroup>Group</SeriesGroup>
<StoryArc>Arc</StoryArc><StoryArcNumber>3</StoryArcNumber></ComicInfo>`
	store, service, book, filename := comicInfoFixture(t, xml)
	ctx := t.Context()
	title, summary, number, release, isbn := "Locked title", "Locked summary", "9", "2021-01-01", "Locked ISBN"
	locked, sort := true, 9.5
	authors := []database.Author{{Name: "Locked author", Role: "writer"}}
	tags := []string{"Locked tag"}
	links := []database.Link{{Label: "Locked link", URL: "https://example.com/locked"}}
	if err := store.PatchBookMetadata(ctx, book.ID, database.BookMetadataPatch{
		Title: &title, Summary: &summary, Number: &number, NumberSort: &sort, ReleaseDate: &release, ISBN: &isbn,
		Authors: &authors, Tags: &tags, Links: &links, TitleLock: &locked, SummaryLock: &locked, NumberLock: &locked,
		NumberSortLock: &locked, ReleaseDateLock: &locked, ISBNLock: &locked, AuthorsLock: &locked, TagsLock: &locked, LinksLock: &locked,
	}); err != nil {
		t.Fatal(err)
	}
	series, err := store.SeriesByID(ctx, book.SeriesID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.PatchSeriesMetadata(ctx, series, database.SeriesMetadataPatch{Summary: &summary, TitleLock: &locked}); err != nil {
		t.Fatal(err)
	}
	if _, err := service.ReadPage(ctx, book, 1); err != nil {
		t.Fatal(err)
	}
	meta := waitComicInfo(t, store, book.ID, ComicInfoParsed)
	if meta.ComicInfoVersion != BookVersion(book) || meta.Title != title || meta.Summary != summary ||
		meta.Number != number || meta.NumberSort == nil || *meta.NumberSort != sort || meta.ReleaseDate != release || meta.ISBN != isbn ||
		!reflect.DeepEqual(meta.Authors, authors) || !reflect.DeepEqual(meta.Tags, tags) || !reflect.DeepEqual(meta.Links, links) {
		t.Fatalf("ComicInfo modified locked values: %+v", meta)
	}
	if meta.SeriesGroup != "Group" || meta.StoryArc != "Arc" || meta.StoryArcNumber != "3" {
		t.Fatalf("ComicInfo grouping fields missing: %+v", meta)
	}
	seriesMeta, err := store.SeriesMetadataByID(ctx, series)
	if err != nil || seriesMeta.Publisher != "Embedded publisher" || seriesMeta.Summary != summary || seriesMeta.Title != series.Name {
		t.Fatalf("unexpected series backfill: %+v, %v", seriesMeta, err)
	}
	if err := os.Remove(filename); err != nil {
		t.Fatal(err)
	}
	if got, err := service.ExtractBookComicInfo(ctx, book); err != nil || got != ComicInfoParsed {
		t.Fatalf("parsed version reopened archive: %q %v", got, err)
	}
	if _, _, err := service.ExtractBookComicInfoWithOptions(ctx, book, true); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("force did not reopen archive: %v", err)
	}
}

func TestComicInfoAbsentAndSizeLimit(t *testing.T) {
	t.Run("absent", func(t *testing.T) {
		store, service, book, filename := comicInfoFixture(t, "")
		pages, err := service.ListPages(t.Context(), book)
		if err != nil || len(pages) != 1 {
			t.Fatalf("pages=%v err=%v", pages, err)
		}
		waitComicInfo(t, store, book.ID, ComicInfoAbsent)
		if err := os.Remove(filename); err != nil {
			t.Fatal(err)
		}
		if status, err := service.ExtractBookComicInfo(t.Context(), book); err != nil || status != ComicInfoAbsent {
			t.Fatalf("absent version was not skipped: status=%q err=%v", status, err)
		}
	})
	t.Run("oversize", func(t *testing.T) {
		store, service, book, _ := comicInfoFixture(t, "<ComicInfo>"+strings.Repeat("X", maxComicInfoSize)+"</ComicInfo>")
		if _, err := service.ExtractBookComicInfo(t.Context(), book); !errors.Is(err, ErrPageTooLarge) {
			t.Fatalf("oversized embedded metadata was accepted: %v", err)
		}
		if _, present, err := store.BookMetadataByID(t.Context(), book.ID); err != nil || present {
			t.Fatalf("oversized metadata affected cache: found=%v err=%v", present, err)
		}
	})
}

func TestComicInfoEncryptedRetryAndSolidRAR(t *testing.T) {
	t.Run("encrypted zip", func(t *testing.T) {
		store, service, book, file := comicInfoFixture(t, "")
		const password = "fixture-zip-password"
		data := buildEncryptedTestZIP(t, map[string][]byte{
			"01.png":             []byte("fixture page"),
			"meta/ComicInfo.xml": []byte("<ComicInfo><Title>Unlocked volume</Title></ComicInfo>"),
		}, password, "aes256-ae2")
		book = replaceComicInfoArchive(t, store, book, file, book.Name, data)
		if _, err := service.ListPages(t.Context(), book); !errors.Is(err, ErrArchivePasswordRequired) {
			t.Fatalf("locked archive reported %v", err)
		}
		waitComicInfo(t, store, book.ID, ComicInfoSkippedEncrypted)
		if _, err := service.VerifyAndBindBookPassword(t.Context(), book, password); err != nil {
			t.Fatal(err)
		}
		if _, err := service.ReadPage(t.Context(), book, 1); err != nil {
			t.Fatal(err)
		}
		meta := waitComicInfo(t, store, book.ID, ComicInfoParsed)
		if meta.Title != "Unlocked volume" {
			t.Fatalf("unexpected unlocked metadata: %+v", meta)
		}
	})
	t.Run("solid rar", func(t *testing.T) {
		store, service, book, file := comicInfoFixture(t, "")
		data := readRARFixture(t, "test_read_format_rar5_multiple_files_solid.rar")
		book = replaceComicInfoArchive(t, store, book, file, "book.cbr", data)
		if _, err := service.ListPages(t.Context(), book); !errors.Is(err, ErrSolidRAR) {
			t.Fatalf("solid RAR reported %v", err)
		}
		waitComicInfo(t, store, book.ID, ComicInfoSkippedSolid)
		if err := os.Remove(file); err != nil {
			t.Fatal(err)
		}
		if status, err := service.ExtractBookComicInfo(t.Context(), book); err != nil || status != ComicInfoSkippedSolid {
			t.Fatalf("solid RAR reopened: %s %v", status, err)
		}
	})
}

func replaceComicInfoArchive(t *testing.T, store *database.Store, book database.Book, file, name string, data []byte) database.Book {
	t.Helper()
	if err := os.WriteFile(file, data, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`UPDATE books SET name=?,size=? WHERE id=?`, name, len(data), book.ID); err != nil {
		t.Fatal(err)
	}
	book, err := store.BookByID(t.Context(), book.ID)
	if err != nil {
		t.Fatal(err)
	}
	return book
}

func comicInfoFixture(t *testing.T, xml string) (*database.Store, *Service, database.Book, string) {
	t.Helper()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "catalog.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	ctx := context.Background()
	if err := store.UpsertLibrary(ctx, database.Library{ID: "library", Name: "Local", ProviderType: database.ProviderLocal, RootPath: dir, Enabled: true}); err != nil {
		t.Fatal(err)
	}
	var buffer bytes.Buffer
	archive := zip.NewWriter(&buffer)
	page, err := archive.Create("01.png")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := page.Write([]byte("fixture page")); err != nil {
		t.Fatal(err)
	}
	if xml != "" {
		entry, err := archive.Create(`metadata\nested/cOmIcInFo.XmL`)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := entry.Write([]byte(xml)); err != nil {
			t.Fatal(err)
		}
	}
	if err := archive.Close(); err != nil {
		t.Fatal(err)
	}
	filename := filepath.Join(dir, "book.cbz")
	if err := os.WriteFile(filename, buffer.Bytes(), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().Exec(`
INSERT INTO series(id,library_id,cid,name,relative_path,books_count,created_at,updated_at,seen_scan_id)
VALUES('series','library','series','Catalog title','',1,'','','scan');
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES('book','series','library','book.cbz','','book.cbz',?,'',1,'','','scan')`, buffer.Len()); err != nil {
		t.Fatal(err)
	}
	book, err := store.BookByID(ctx, "book")
	if err != nil {
		t.Fatal(err)
	}
	cm, err := cache.New(store, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, cm, slog.Default())
	service.SetComicInfoParser(func(reader io.Reader) (database.BookScrapedMetadata, database.SeriesComicInfoBackfill, error) {
		meta, err := extmeta.ParseComicInfoXML(reader)
		if err != nil {
			return database.BookScrapedMetadata{}, database.SeriesComicInfoBackfill{}, err
		}
		var authors []database.Author
		for _, author := range meta.DetailedAuthors {
			authors = append(authors, database.Author{Name: author.Name, Role: author.Role})
		}
		var links []database.Link
		for _, link := range meta.Links {
			links = append(links, database.Link{Label: link.Label, URL: link.URL})
		}
		return database.BookScrapedMetadata{
			Title: meta.Title, Summary: meta.Summary, Number: meta.Number, NumberSort: meta.NumberSort,
			ReleaseDate: meta.ReleaseDate, ISBN: meta.ISBN, Authors: authors, Tags: meta.Tags, Links: links,
			SeriesGroup: meta.SeriesGroup, StoryArc: meta.StoryArc, StoryArcNumber: meta.StoryArcNumber,
		}, database.SeriesComicInfoBackfill{Title: meta.Series, Publisher: meta.Publisher, Summary: meta.Summary}, nil
	})
	t.Cleanup(service.Close)
	return store, service, book, filename
}

func waitComicInfo(t *testing.T, store *database.Store, id, status string) database.BookMetadata {
	t.Helper()
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		meta, _, err := store.BookMetadataByID(t.Context(), id)
		if err != nil {
			t.Fatal(err)
		}
		if meta.ComicInfoStatus == status {
			return meta
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("book %s did not reach ComicInfo status %s", id, status)
	return database.BookMetadata{}
}

func TestUnionComicInfoWriteThroughAndShortcutSync(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	store, err := database.Open(filepath.Join(dir, "union_comicinfo.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	libDir := filepath.Join(dir, "phys")
	if err := os.MkdirAll(libDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertLibrary(ctx, database.Library{
		ID: "lib_phys", Name: "Physical", ProviderType: database.ProviderLocal,
		RootCID: "local:" + libDir, RootPath: libDir, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}

	buildCBZ := func(name, xml string) (string, int64) {
		t.Helper()
		var buf bytes.Buffer
		zw := zip.NewWriter(&buf)
		p, err := zw.Create("01.png")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := p.Write([]byte("fixture page")); err != nil {
			t.Fatal(err)
		}
		if xml != "" {
			ci, err := zw.Create("ComicInfo.xml")
			if err != nil {
				t.Fatal(err)
			}
			if _, err := ci.Write([]byte(xml)); err != nil {
				t.Fatal(err)
			}
		}
		if err := zw.Close(); err != nil {
			t.Fatal(err)
		}
		rel := filepath.Join("SeriesA", name)
		full := filepath.Join(libDir, rel)
		if err := os.MkdirAll(filepath.Dir(full), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, buf.Bytes(), 0o600); err != nil {
			t.Fatal(err)
		}
		return rel, int64(buf.Len())
	}

	const comicXML = `<ComicInfo><Title>Union Embedded Vol</Title><Summary>Embedded volume summary</Summary>
<Number>2</Number><Year>2025</Year><Month>4</Month><Day>9</Day><ISBN>9784088820002</ISBN>
<Writer>Author One</Writer><Tags>TagOne</Tags><Web>https://example.test/vol</Web>
<Series>Union Embedded Series</Series><Publisher>Union Pub</Publisher><SeriesGroup>GroupA</SeriesGroup></ComicInfo>`

	f1, s1 := buildCBZ("01.cbz", comicXML)
	f2, s2 := buildCBZ("02.cbz", comicXML)
	f3, s3 := buildCBZ("03.cbz", comicXML)
	f4, s4 := buildCBZ("04.cbz", "")
	f5, s5 := buildCBZ("05.cbz", comicXML)

	now := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO series(id,library_id,cid,name,relative_path,one_shot,created_at,updated_at,seen_scan_id)
VALUES('s_phys','lib_phys','SeriesA','SeriesA','PhysRoot/SeriesA',0,?,?,'sc1')`, now, now); err != nil {
		t.Fatal(err)
	}
	for i, row := range []struct {
		id, rel string
		name    string
		size    int64
	}{
		{"b_p1", f1, "01.cbz", s1},
		{"b_p2", f2, "02.cbz", s2},
		{"b_p3", f3, "03.cbz", s3},
		{"b_p4", f4, "04.cbz", s4},
		{"b_p5", f5, "05.cbz", s5},
	} {
		if _, err := store.DB().ExecContext(ctx, `
INSERT INTO books(id,series_id,library_id,file_id,parent_cid,name,size,pick_code,number_sort,created_at,updated_at,seen_scan_id)
VALUES(?,'s_phys','lib_phys',?,'SeriesA',?,?,'',?,?,?,'sc1')`,
			row.id, row.rel, row.name, row.size, float64(i+1), now, now); err != nil {
			t.Fatal(err)
		}
	}

	cm, err := cache.New(store, filepath.Join(dir, "cache"))
	if err != nil {
		t.Fatal(err)
	}
	service := NewService(store, nil, cm, slog.Default())
	service.SetComicInfoParser(func(reader io.Reader) (database.BookScrapedMetadata, database.SeriesComicInfoBackfill, error) {
		meta, err := extmeta.ParseComicInfoXML(reader)
		if err != nil {
			return database.BookScrapedMetadata{}, database.SeriesComicInfoBackfill{}, err
		}
		var authors []database.Author
		for _, author := range meta.DetailedAuthors {
			authors = append(authors, database.Author{Name: author.Name, Role: author.Role})
		}
		var links []database.Link
		for _, link := range meta.Links {
			links = append(links, database.Link{Label: link.Label, URL: link.URL})
		}
		return database.BookScrapedMetadata{
			Title: meta.Title, Summary: meta.Summary, Number: meta.Number, NumberSort: meta.NumberSort,
			ReleaseDate: meta.ReleaseDate, ISBN: meta.ISBN, Authors: authors, Tags: meta.Tags, Links: links,
			SeriesGroup: meta.SeriesGroup,
		}, database.SeriesComicInfoBackfill{Title: meta.Series, Publisher: meta.Publisher, Summary: meta.Summary}, nil
	})
	t.Cleanup(service.Close)

	// Pre-extract b_p5 directly BEFORE union creation so physical b_p5 already has cached ComicInfo.
	if st, err := service.ExtractBookComicInfo(ctx, func() database.Book {
		b, _ := store.BookByID(ctx, "b_p5")
		return b
	}()); err != nil || st != ComicInfoParsed {
		t.Fatalf("pre-extract b_p5 = %q, %v", st, err)
	}

	unionLib, _, err := store.CreateUnionLibrary(ctx, "UnionLib", true, []string{"lib_phys"})
	if err != nil {
		t.Fatal(err)
	}
	uSeriesList, _, err := store.SeriesPage(ctx, database.SeriesQuery{LibraryIDs: []string{unionLib.ID}, Size: 10})
	if err != nil || len(uSeriesList) != 1 {
		t.Fatalf("union series = %+v, err=%v", uSeriesList, err)
	}
	uSeries := uSeriesList[0]
	uBooks, _, err := store.BooksPage(ctx, database.BookQuery{SeriesID: uSeries.ID, Unpaged: true})
	if err != nil || len(uBooks) != 5 {
		t.Fatalf("union books = %+v, err=%v", uBooks, err)
	}
	byName := make(map[string]database.Book, len(uBooks))
	for _, ub := range uBooks {
		byName[ub.Name] = ub
	}

	// Lock Title on 02.cbz and set manual MatchOrigin on 03.cbz.
	locked := true
	lockedTitle := "Locked Union Vol"
	if err := store.PatchBookMetadata(ctx, byName["02.cbz"].ID, database.BookMetadataPatch{
		Title: &lockedTitle, TitleLock: &locked,
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertBookScrapedMetadata(ctx, byName["03.cbz"].ID, database.BookScrapedMetadata{
		Title: "Manual Union Vol", MatchOrigin: "manual",
	}); err != nil {
		t.Fatal(err)
	}

	// 1. Reading 01.cbz via union book triggers async ComicInfo write-through to union book & series.
	if _, err := service.ReadPage(ctx, byName["01.cbz"], 1); err != nil {
		t.Fatal(err)
	}
	uMeta1 := waitComicInfo(t, store, byName["01.cbz"].ID, ComicInfoParsed)
	p1, _ := store.BookByIDAny(ctx, "b_p1")
	if uMeta1.Title != "Union Embedded Vol" || uMeta1.Summary != "Embedded volume summary" ||
		uMeta1.ComicInfoVersion != BookVersion(p1) || uMeta1.SeriesGroup != "GroupA" {
		t.Fatalf("union 01.cbz write-through = %+v", uMeta1)
	}
	uSeriesMeta, err := store.SeriesMetadataByID(ctx, uSeries)
	if err != nil || uSeriesMeta.Publisher != "Union Pub" || uSeriesMeta.Title != "Union Embedded Series" {
		t.Fatalf("union series backfill = %+v, %v", uSeriesMeta, err)
	}

	// 2. 02.cbz keeps locked Title while filling unlocked Summary; 03.cbz keeps manual metadata while recording parsed status.
	if st, err := service.ExtractBookComicInfo(ctx, byName["02.cbz"]); err != nil || st != ComicInfoParsed {
		t.Fatalf("extract 02.cbz = %q, %v", st, err)
	}
	uMeta2, _, err := store.BookMetadataByID(ctx, byName["02.cbz"].ID)
	if err != nil || uMeta2.Title != lockedTitle || uMeta2.Summary != "Embedded volume summary" || uMeta2.ComicInfoStatus != ComicInfoParsed {
		t.Fatalf("union 02.cbz lock protection = %+v, %v", uMeta2, err)
	}
	if st, err := service.ExtractBookComicInfo(ctx, byName["03.cbz"]); err != nil || st != ComicInfoParsed {
		t.Fatalf("extract 03.cbz = %q, %v", st, err)
	}
	uMeta3, _, err := store.BookMetadataByID(ctx, byName["03.cbz"].ID)
	if err != nil || uMeta3.Title != "Manual Union Vol" || uMeta3.Summary != "" || uMeta3.ComicInfoStatus != ComicInfoParsed {
		t.Fatalf("union 03.cbz manual protection = %+v, %v", uMeta3, err)
	}

	// 3. Sentinel write-through for 04.cbz without ComicInfo.xml.
	if _, err := service.ListPages(ctx, byName["04.cbz"]); err != nil {
		t.Fatal(err)
	}
	uMeta4 := waitComicInfo(t, store, byName["04.cbz"].ID, ComicInfoAbsent)
	p4, _ := store.BookByIDAny(ctx, "b_p4")
	if uMeta4.ComicInfoVersion != BookVersion(p4) {
		t.Fatalf("union 04.cbz sentinel version = %q, want %q", uMeta4.ComicInfoVersion, BookVersion(p4))
	}

	// 4. Shortcut sync: reset logical 05.cbz metadata AND delete physical 05.cbz file on disk!
	if _, err := store.DB().ExecContext(ctx, `DELETE FROM book_metadata WHERE book_id=?`, byName["05.cbz"].ID); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(libDir, f5)); err != nil {
		t.Fatal(err)
	}
	status5, changed5, err := service.ExtractBookComicInfoWithOptions(ctx, byName["05.cbz"], false)
	if err != nil || status5 != ComicInfoParsed || !changed5 {
		t.Fatalf("shortcut sync reopened archive or failed: status=%q changed=%v err=%v", status5, changed5, err)
	}
	uMeta5, ok5, err := store.BookMetadataByID(ctx, byName["05.cbz"].ID)
	p5, _ := store.BookByIDAny(ctx, "b_p5")
	if err != nil || !ok5 || uMeta5.Title != "Union Embedded Vol" || uMeta5.ComicInfoStatus != ComicInfoParsed || uMeta5.ComicInfoVersion != BookVersion(p5) {
		t.Fatalf("shortcut synced metadata = %+v, ok=%v err=%v", uMeta5, ok5, err)
	}
	// Second shortcut call sees logical book already synced -> changed=false without touching disk.
	status5b, changed5b, err := service.ExtractBookComicInfoWithOptions(ctx, byName["05.cbz"], false)
	if err != nil || status5b != ComicInfoParsed || changed5b {
		t.Fatalf("repeated shortcut sync should be no-op: status=%q changed=%v err=%v", status5b, changed5b, err)
	}
}
