package database

import (
	"context"
	"path/filepath"
	"slices"
	"testing"
)

func TestV33ReadingActivityGrouping(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "activity.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	for _, id := range []string{"a", "b", "hidden", "forbidden", "disabled"} {
		if err := s.UpsertLibrary(ctx, Library{ID: id, Name: id, RootCID: id, Enabled: id != "disabled"}); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct{ library, series, book string }{
		{"a", "edition-a", "a-1"}, {"a", "edition-a", "a-2"}, {"a", "edition-a", "a-3"},
		{"b", "edition-b", "b-1"}, {"b", "edition-b", "b-2"}, {"b", "oneshot", "short"},
		{"a", "adult", "adult"}, {"hidden", "hidden", "hidden"},
		{"forbidden", "forbidden", "forbidden"}, {"disabled", "disabled", "disabled"},
	} {
		seedV33Book(t, s, row.library, row.series, row.book)
	}
	execV33(t, s, `UPDATE series SET one_shot=1 WHERE id='oneshot'`)
	execV33(t, s, `
INSERT INTO series_metadata(series_id,bgm_subject_id,title,alternate_titles_json,is_nsfw,updated_at)
VALUES ('edition-a',44,'Display title','[{"label":"ja","title":"Original alias"}]',0,?),
       ('edition-b',44,'Other edition','[]',0,?),('oneshot',44,'Short','[]',0,?),('adult',44,'Adults','[]',1,?)`,
		nowText(), nowText(), nowText(), nowText())

	for _, row := range []struct {
		book, when string
		completed  bool
	}{
		{"a-1", "2026-01-01T15:59:59.999999999Z", false},
		{"a-2", "2026-01-01T16:00:00Z", false},
		{"a-3", "2026-01-01T16:00:00.000000001Z", false},
		{"b-1", "2026-01-02T00:00:00.000000001+08:00", false},
		{"b-2", "2026-01-01T16:01:00Z", true},
		{"short", "2026-01-01T16:00:00Z", false},
		{"adult", "2026-01-01T16:09:00Z", false},
		{"hidden", "2026-01-01T16:09:00Z", false},
		{"forbidden", "2026-01-01T16:09:00Z", false},
		{"disabled", "2026-01-01T16:09:00Z", false},
	} {
		execV33(t, s, `INSERT INTO book_read_progress(user_id, book_id, series_id, completed, page, updated_at)
SELECT 'reader', id, series_id, ?, 2, ? FROM books WHERE id=?`, row.completed, row.when, row.book)
	}
	execV33(t, s, `INSERT INTO book_read_progress(user_id,book_id,series_id,completed,updated_at)
VALUES('other','a-1','edition-a',0,'2026-02-01T14:30:00Z')`)

	reader := WithUserScope(ctx, UserScope{
		UserID: "reader", AllowedLibraryIDs: []string{"a", "b", "hidden", "disabled"},
		HiddenLibraries: map[string]struct{}{"hidden": {}},
	})
	bookIDs := func(group ReadingActivityGroup) []string {
		out := make([]string, 0, len(group.Books))
		for _, item := range group.Books {
			out = append(out, item.Book.ID)
		}
		return out
	}
	get := func(scope context.Context, query ReadingActivityQuery, groups, books int64) ReadingActivityPage {
		t.Helper()
		got, err := s.ReadingActivity(scope, query)
		if err != nil || got.TotalElements != groups || got.TotalBooks != books || got.Groups == nil {
			t.Fatalf("ReadingActivity(%+v): %+v, %v", query, got, err)
		}
		return got
	}

	continueReading := get(reader, ReadingActivityQuery{InProgressOnly: true, MergeBangumi: true, Size: 1}, 2, 5)
	if g := continueReading.Groups[0]; g.GroupKey != "bgm:44" || !g.MergedByBangumi ||
		!slices.Equal(bookIDs(g), []string{"a-3", "b-1", "a-2", "a-1"}) {
		t.Fatalf("Bangumi grouped active editions: %+v", g)
	}
	bySeries := get(reader, ReadingActivityQuery{InProgressOnly: true, Size: 10}, 3, 5)
	if len(bySeries.Groups) != 3 || bySeries.Groups[0].GroupKey != "series:edition-a" ||
		bySeries.Groups[2].GroupKey != "series:oneshot" {
		t.Fatalf("series grouped activity: %+v", bySeries)
	}

	q := ReadingActivityQuery{MergeBangumi: true, TZOffset: 480, Size: 1}
	history := get(reader, q, 3, 6)
	if len(history.Groups) != 1 || history.Groups[0].DayKey != "2026-01-02" ||
		!slices.Equal(bookIDs(history.Groups[0]), []string{"b-2", "a-3", "b-1", "a-2"}) ||
		!history.Groups[0].Books[0].Progress.Completed {
		t.Fatalf("first history page: %+v", history)
	}
	q.Page = 1
	history = get(reader, q, 3, 6)
	if len(history.Groups) != 1 || history.Groups[0].GroupKey != "series:oneshot" || history.Groups[0].MergedByBangumi {
		t.Fatalf("second history page: %+v", history)
	}
	q.Page = 2
	history = get(reader, q, 3, 6)
	if len(history.Groups) != 1 || history.Groups[0].DayKey != "2026-01-01" ||
		!slices.Equal(bookIDs(history.Groups[0]), []string{"a-1"}) {
		t.Fatalf("third history page: %+v", history)
	}
	q.Page = 3
	if got := get(reader, q, 3, 6); len(got.Groups) != 0 {
		t.Fatalf("out-of-range page: %+v", got)
	}
	if got := get(reader, ReadingActivityQuery{Search: "Original Alias", TZOffset: 480, Size: 9}, 2, 3); len(got.Groups) != 2 {
		t.Fatalf("alternate title search: %+v", got)
	}
	if got := get(reader, ReadingActivityQuery{Search: "display title", Size: 9}, 1, 3); len(got.Groups) != 1 {
		t.Fatalf("display title search: %+v", got)
	}
	if got := get(reader, ReadingActivityQuery{Search: "b-1.cbz", Size: 9}, 1, 1); got.Groups[0].Books[0].Book.ID != "b-1" {
		t.Fatalf("volume search: %+v", got)
	}
	get(WithUserScope(ctx, UserScope{UserID: "reader"}), q, 0, 0)
	get(WithUserScope(ctx, UserScope{UserID: "reader", AllLibraries: true, AllowNSFW: true, ShowNSFW: true}), ReadingActivityQuery{Size: 10}, 6, 9)
	isolated := get(WithUserScope(ctx, UserScope{UserID: "other", AllLibraries: true}), ReadingActivityQuery{Size: 10}, 1, 1)
	if isolated.Groups[0].DayKey != "2026-02-01" {
		t.Fatalf("history must use only the current user: %+v", isolated)
	}
}

func TestV33ReadProgressUsesSavedIndexAndMirrorsFinalValues(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "progress.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	for _, id := range []string{"source", "union"} {
		if err := s.UpsertLibrary(ctx, Library{ID: id, Name: id, RootCID: id, Enabled: true}); err != nil {
			t.Fatal(err)
		}
		seedV33Book(t, s, id, "series-"+id, "book-"+id)
	}
	execV33(t, s, `INSERT INTO union_book_sources(union_book_id,source_book_id,source_library_id,priority,source_size)
VALUES('book-union','book-source','source',0,100)`)
	if err := s.RecordArchiveIndexBuild(ctx, "book-union", "test", "[]", 12, 0); err != nil {
		t.Fatal(err)
	}
	book := Book{ID: "book-union", SeriesID: "series-union"}
	scope := WithUserScope(ctx, UserScope{UserID: "reader", AllLibraries: true})
	done, unfinished := true, false
	last, oversized, early := 12, 90, 2
	for _, tc := range []struct {
		name      string
		completed *bool
		page      *int
		wantDone  bool
		wantPage  int
	}{
		{"last page", nil, &last, true, 12},
		{"oversized page", nil, &oversized, true, 12},
		{"earlier page", nil, &early, false, 2},
		{"explicit completion only", &done, nil, true, 12},
		{"explicit unfinished wins", &unfinished, &oversized, false, 12},
		{"explicit finished retains earlier page", &done, &early, true, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := s.UpdateBookReadProgress(scope, book, tc.completed, tc.page); err != nil {
				t.Fatal(err)
			}
			for _, id := range []string{"book-union", "book-source"} {
				p, found, err := s.BookReadProgress(scope, id)
				if err != nil || !found || p.Completed != tc.wantDone || p.Page == nil || *p.Page != tc.wantPage ||
					(p.CompletedAt != nil) != tc.wantDone {
					t.Fatalf("%s progress: %+v, found=%v err=%v", id, p, found, err)
				}
				if _, found, err := s.BookReadProgress(ctx, id); found || err != nil {
					t.Fatalf("another user's progress was modified: found=%v err=%v", found, err)
				}
			}
		})
	}
}
