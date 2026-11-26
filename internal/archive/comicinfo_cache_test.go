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
