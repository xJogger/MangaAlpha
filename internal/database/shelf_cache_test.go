package database

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestBooksPageCountCache(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "book-count.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"a", "b", "c"} {
		if err := s.UpsertLibrary(ctx, Library{ID: id, Name: id, RootCID: id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	seedV33Book(t, s, "a", "regular", "regular-1")
	seedV33Book(t, s, "a", "regular", "regular-2")
	seedV33Book(t, s, "b", "adult", "adult-1")
	seedV33Book(t, s, "b", "single", "single-1")
	seedV33Book(t, s, "c", "denied", "denied-1")
	execV33(t, s, "INSERT INTO series_metadata(series_id,is_nsfw,updated_at) VALUES('adult',1,'')")

	newUser := func(name string, hidden []string, show bool) User {
		t.Helper()
		u, err := s.CreateUser(ctx, CreateUserInput{
			Username: name, Password: "test-password", AllowedLibraryIDs: []string{"a", "b"}, AllowNSFW: true,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := s.SetUserPreferences(ctx, u.ID, &show, &hidden); err != nil {
			t.Fatal(err)
		}
		u, err = s.UserByID(ctx, u.ID)
		if err != nil {
			t.Fatal(err)
		}
		return u
	}
	hiddenB := newUser("hidden-b", []string{"b"}, false)
	hiddenA := newUser("hidden-a", []string{"a"}, false)
	showAdult := newUser("show-adult", []string{"a"}, true)

	check := func(ctx context.Context, q BookQuery, want int64, wantItems int) {
		t.Helper()
		items, total, err := s.BooksPage(ctx, q)
		if err != nil || total != want || len(items) != wantItems {
			t.Fatalf("user %s query %+v: total=%d items=%d err=%v, want %d/%d",
				UserIDFromContext(ctx), q, total, len(items), err, want, wantItems)
		}
	}
	bCtx := WithUserScope(ctx, hiddenB.Scope())
	aCtx := WithUserScope(ctx, hiddenA.Scope())
	adultCtx := WithUserScope(ctx, showAdult.Scope())
	check(bCtx, BookQuery{Size: 1}, 2, 1)
	check(bCtx, BookQuery{Size: 1}, 2, 1)

	// A sentinel in the warmed entry proves the next page reads the cached count.
	const key = `books-count:[["a"],true,null]`
	s.shelf.mu.Lock()
	entry, found := s.shelf.items[key]
	if found {
		s.shelf.items[key] = shelfCacheEntry{entry.revision, int64(13)}
	}
	s.shelf.mu.Unlock()
	if !found {
		t.Fatal("BooksPage did not cache the resolved count")
	}
	check(bCtx, BookQuery{Size: 2, Page: 1, Sort: "name,desc"}, 13, 0)
	s.shelf.mu.Lock()
	s.shelf.items[key] = entry
	s.shelf.mu.Unlock()

	check(aCtx, BookQuery{}, 1, 1)
	check(adultCtx, BookQuery{}, 2, 2)
	check(bCtx, BookQuery{LibraryIDs: []string{"b", "c"}}, 1, 1)
	check(bCtx, BookQuery{LibraryIDs: []string{"c"}}, 0, 0)
	check(WithUserScope(ctx, UserScope{UserID: "no-libraries"}), BookQuery{}, 0, 0)
	check(adultCtx, BookQuery{LibraryIDs: []string{"b", "a", "b"}}, 4, 4)
	check(adultCtx, BookQuery{LibraryIDs: []string{"a", "b"}}, 4, 4)
	yes, no := true, false
	onlyOneShot := BookQuery{OneShot: &yes}
	regularOnly := BookQuery{OneShot: &no}
	check(aCtx, onlyOneShot, 0, 0)
	check(aCtx, regularOnly, 1, 1)

	seedV33Book(t, s, "a", "regular", "regular-3")
	check(bCtx, BookQuery{}, 3, 3)
	execV33(t, s, "DELETE FROM books WHERE id='regular-3'")
	check(bCtx, BookQuery{}, 2, 2)
	execV33(t, s, "UPDATE series_metadata SET is_nsfw=0 WHERE series_id='adult'")
	check(aCtx, BookQuery{}, 2, 2)
	execV33(t, s, "UPDATE series_metadata SET is_nsfw=1 WHERE series_id='adult'")
	check(aCtx, BookQuery{}, 1, 1)
	if err := s.SetSeriesOneShotOverride(ctx, "single", true); err != nil {
		t.Fatal(err)
	}
	check(aCtx, onlyOneShot, 1, 1)
	check(aCtx, regularOnly, 0, 0)
	seedV33Book(t, s, "b", "single", "single-2")
	check(aCtx, onlyOneShot, 0, 0)
	check(aCtx, regularOnly, 2, 2)
	execV33(t, s, "DELETE FROM books WHERE id='single-2'")
	check(aCtx, onlyOneShot, 1, 1)
	if err := s.SetSeriesOneShotOverride(ctx, "single", false); err != nil {
		t.Fatal(err)
	}
	check(aCtx, onlyOneShot, 0, 0)

	// A reader bypasses both lookup and population while a bulk revision is active.
	check(bCtx, BookQuery{}, 2, 2)
	execV33(t, s, "UPDATE shelf_revision SET bulk_mode=1 WHERE id=1")
	execV33(t, s, "DELETE FROM books WHERE id='regular-1'")
	check(bCtx, BookQuery{}, 1, 1)
	s.shelf.mu.Lock()
	cachedDuringBulk := s.shelf.items[key].value
	s.shelf.mu.Unlock()
	if cachedDuringBulk != int64(2) {
		t.Fatalf("bulk read replaced cached total with %v", cachedDuringBulk)
	}
	execV33(t, s, "UPDATE shelf_revision SET bulk_mode=0,catalog=catalog+1 WHERE id=1")
	check(bCtx, BookQuery{}, 1, 1)
	canceled, cancel := context.WithCancel(bCtx)
	cancel()
	if _, _, err := s.BooksPage(canceled, BookQuery{}); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled BooksPage: %v", err)
	}
}

func TestUserShowNSFWShelfRevisionMigration(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "preferences.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ClaimAdmin(ctx, "owner", "test-password"); err != nil {
		t.Fatal(err)
	}
	user, err := s.CreateUser(ctx, CreateUserInput{Username: "reader", Password: "test-password", AllowNSFW: true})
	if err != nil {
		t.Fatal(err)
	}
	execV33(t, s, "DROP TRIGGER fk115_shelf_users_show_nsfw_update")
	for range 2 {
		if err := applyMigrations(s.db); err != nil {
			t.Fatal(err)
		}
	}
	revision := func() int64 {
		t.Helper()
		rev, err := s.shelfRevision(ctx, false)
		if err != nil {
			t.Fatal(err)
		}
		return rev.catalog
	}
	before := revision()
	if _, err := s.SetUserShowNSFW(ctx, user.ID, true); err != nil {
		t.Fatal(err)
	}
	if after := revision(); after != before+1 {
		t.Fatalf("NSFW preference revision %d -> %d", before, after)
	}
}

func TestShelfCacheGroupingRankAndCatalogVisibility(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "shelf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	a := Library{ID: "local", Name: "Local", ProviderType: ProviderLocal, RootPath: "/fixture", Enabled: true}
	b := Library{ID: "remote", Name: "Remote", RootCID: "fixture", Enabled: true}
	for _, lib := range []Library{a, b} {
		if err := s.UpsertLibrary(ctx, lib); err != nil {
			t.Fatal(err)
		}
	}
	insertCatalogSeries(t, s, a.ID, "a-anchor", "Anchor", []catalogBookTime{{modified: "2023-01-01T00:00:00Z"}})
	insertCatalogSeries(t, s, b.ID, "b-edition", "Edition", []catalogBookTime{{modified: "2025-01-01T00:00:00Z"}})
	insertCatalogSeries(t, s, a.ID, "c-ranked", "Ranked", []catalogBookTime{{modified: "2024-01-01T00:00:00Z"}})
	insertCatalogSeries(t, s, a.ID, "d-unknown", "Unknown", []catalogBookTime{{modified: "2022-01-01T00:00:00Z"}})
	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := s.DB().Exec(sql, args...); err != nil {
			t.Fatal(err)
		}
	}
	exec(`INSERT INTO series_metadata(series_id,bgm_subject_id,bgm_rank,updated_at)
VALUES ('a-anchor',100,NULL,''),('b-edition',100,1,''),('c-ranked',200,20,'')`)
	check := func(q SeriesQuery, total int64, want ...string) {
		t.Helper()
		items, n, err := s.SeriesPage(ctx, q)
		got := make([]string, 0, len(items))
		for _, item := range items {
			got = append(got, item.ID)
		}
		if err != nil || n != total || !reflect.DeepEqual(got, want) {
			t.Fatalf("query %+v: total %d items %v err %v; want total %d, items %v", q, n, got, err, total, want)
		}
	}
	q := SeriesQuery{Size: 36, Sort: "bgmRank,asc", GroupBangumi: true}
	check(q, 3, "a-anchor", "c-ranked", "d-unknown")
	check(q, 3, "a-anchor", "c-ranked", "d-unknown") // cache hit
	date := SeriesQuery{Size: 36, Sort: "lastModifiedDate,desc", GroupBangumi: true}
	check(date, 3, "a-anchor", "c-ranked", "d-unknown")
	check(SeriesQuery{Size: 1, Page: 1, Sort: q.Sort, GroupBangumi: true}, 3, "c-ranked")
	check(SeriesQuery{Size: 36, Sort: q.Sort}, 4, "b-edition", "c-ranked", "a-anchor", "d-unknown")
	check(SeriesQuery{Size: 36, Sort: q.Sort, GroupBangumi: true, LibraryIDs: []string{b.ID}}, 1, "b-edition")

	// Hidden versions must stop contributing to sorting without exposing the hidden row.
	b.Enabled = false
	if err := s.UpsertLibrary(ctx, b); err != nil {
		t.Fatal(err)
	}
	check(q, 3, "c-ranked", "a-anchor", "d-unknown")
	check(date, 3, "c-ranked", "a-anchor", "d-unknown")

	exec(`UPDATE series_metadata SET is_nsfw=1 WHERE series_id='c-ranked'`)
	check(q, 2, "a-anchor", "d-unknown")
	if err := s.SetSetting(ctx, "show_nsfw", "true"); err != nil {
		t.Fatal(err)
	}
	check(q, 3, "c-ranked", "a-anchor", "d-unknown")
}

func TestGroupBangumiPrefersMostBooksAndUnionTieBreak(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "group_most_books.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })

	unionLib := Library{ID: "lib-union", Name: "AAA Union", ProviderType: ProviderUnion, RootCID: "union:lib-union", Enabled: true}
	normalLib := Library{ID: "lib-normal", Name: "ZZZ Normal", ProviderType: ProviderLocal, RootCID: "local:/fixture", RootPath: "/fixture", Enabled: true}
	for _, lib := range []Library{unionLib, normalLib} {
		if err := s.UpsertLibrary(ctx, lib); err != nil {
			t.Fatal(err)
		}
	}

	// Same bgm_subject_id=500: normal library has MORE books (3 vs 1) -> must beat union library.
	insertCatalogSeries(t, s, unionLib.ID, "a-union-short", "Short Union", []catalogBookTime{
		{modified: "2025-05-01T00:00:00Z"},
	})
	insertCatalogSeries(t, s, normalLib.ID, "z-normal-full", "Full Normal", []catalogBookTime{
		{modified: "2024-01-01T00:00:00Z"},
		{modified: "2024-01-02T00:00:00Z"},
		{modified: "2024-01-03T00:00:00Z"},
	})

	// Same bgm_subject_id=600: equal book counts (2 vs 2) -> union library wins tie-break.
	insertCatalogSeries(t, s, normalLib.ID, "a-normal-tie", "Tie Normal", []catalogBookTime{
		{modified: "2024-02-01T00:00:00Z"},
		{modified: "2024-02-02T00:00:00Z"},
	})
	insertCatalogSeries(t, s, unionLib.ID, "z-union-tie", "Tie Union", []catalogBookTime{
		{modified: "2024-03-01T00:00:00Z"},
		{modified: "2024-03-02T00:00:00Z"},
	})

	if _, err := s.DB().Exec(`INSERT INTO series_metadata(series_id,bgm_subject_id,bgm_rank,updated_at) VALUES
		('a-union-short',500,5,''),
		('z-normal-full',500,NULL,''),
		('a-normal-tie',600,20,''),
		('z-union-tie',600,20,'')`); err != nil {
		t.Fatal(err)
	}

	items, total, err := s.SeriesPage(ctx, SeriesQuery{Size: 20, Sort: "bgmRank,asc", GroupBangumi: true})
	if err != nil {
		t.Fatal(err)
	}
	if total != 2 || len(items) != 2 {
		t.Fatalf("total=%d len=%d, want 2", total, len(items))
	}
	// Group 500 still gets group-level bgm_rank=5 from a-union-short, but picks z-normal-full (3 books).
	if items[0].ID != "z-normal-full" || items[0].BooksCount != 3 {
		t.Fatalf("items[0] = %s (books=%d), want z-normal-full (books=3)", items[0].ID, items[0].BooksCount)
	}
	// Group 600 tie-breaks 2 vs 2 to union edition z-union-tie.
	if items[1].ID != "z-union-tie" || items[1].BooksCount != 2 {
		t.Fatalf("items[1] = %s (books=%d), want z-union-tie (books=2)", items[1].ID, items[1].BooksCount)
	}

	// Bangumi lookup must follow the exact same order (books_count DESC, then union).
	matches500, err := s.LookupSeriesByBangumiSubject(ctx, 500)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches500) != 2 || matches500[0].SeriesID != "z-normal-full" || matches500[1].SeriesID != "a-union-short" {
		t.Fatalf("LookupSeriesByBangumiSubject(500) = %+v, want z-normal-full first", matches500)
	}
	matches600, err := s.LookupSeriesByBangumiSubject(ctx, 600)
	if err != nil {
		t.Fatal(err)
	}
	if len(matches600) != 2 || matches600[0].SeriesID != "z-union-tie" || matches600[1].SeriesID != "a-normal-tie" {
		t.Fatalf("LookupSeriesByBangumiSubject(600) = %+v, want z-union-tie first", matches600)
	}
}

func TestShelfCacheContinueReadingInvalidatedOnCommit(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "shelf.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	lib := Library{ID: "library", Name: "Library", RootCID: "fixture", Enabled: true}
	if err := s.UpsertLibrary(ctx, lib); err != nil {
		t.Fatal(err)
	}
	insertCatalogSeries(t, s, lib.ID, "series", "Example", []catalogBookTime{{}, {}})
	q := BookQuery{Size: 8, Sort: "readProgress.readDate,desc", ReadStatus: []string{"IN_PROGRESS"}}
	count := func(n int64) {
		t.Helper()
		rows, total, err := s.BooksPage(ctx, q)
		if err != nil || total != n || int64(len(rows)) != n {
			t.Fatalf("continue reading = %v / %d / %v; want %d", rows, total, err, n)
		}
	}
	count(0)
	book := Book{ID: "series-book-a", SeriesID: "series"}
	page := 3
	complete := false
	if err := s.UpdateBookReadProgress(ctx, book, &complete, &page); err != nil {
		t.Fatal(err)
	}
	count(1)
	complete = true
	if err := s.UpdateBookReadProgress(ctx, book, &complete, &page); err != nil {
		t.Fatal(err)
	}
	count(0)
	complete = false
	if err := s.UpdateBookReadProgress(ctx, book, &complete, &page); err != nil {
		t.Fatal(err)
	}
	count(1)
	lib.Enabled = false
	if err := s.UpsertLibrary(ctx, lib); err != nil {
		t.Fatal(err)
	}
	count(0)
}
