package database

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"slices"
	"testing"
)

func execV33(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.Exec(query, args...); err != nil {
		t.Fatal(err)
	}
}

func seedV33Book(t *testing.T, s *Store, libraryID, seriesID, bookID string) {
	t.Helper()
	now := nowText()
	execV33(t, s, `INSERT OR IGNORE INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id)
VALUES(?,?,?,?,?,?,?,'scan')`, seriesID, libraryID, seriesID, seriesID, seriesID, now, now)
	execV33(t, s, `INSERT INTO books(id,series_id,library_id,file_id,name,parent_cid,pick_code,size,number_sort,created_at,updated_at,seen_scan_id)
VALUES(?,?,?,?,?,'','',100,1,?,?,'scan')`, bookID, seriesID, libraryID, bookID, bookID+".cbz", now, now)
}

func TestV33HiddenLibrariesMigrationAndVisibility(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "preferences.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if _, err := s.ClaimAdmin(ctx, "owner", "test-password"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"a", "b", "c", "disabled", "member", "union"} {
		provider := Provider115
		if id == "union" {
			provider = ProviderUnion
		}
		if err := s.UpsertLibrary(ctx, Library{ID: id, Name: id, RootCID: id, ProviderType: provider, Enabled: id != "disabled"}); err != nil {
			t.Fatal(err)
		}
	}
	execV33(t, s, `INSERT INTO union_layers(union_library_id,member_library_id,priority,created_at) VALUES('union','member',0,?)`, nowText())
	for _, id := range []string{"a", "b", "c", "disabled", "member", "union"} {
		seedV33Book(t, s, id, "series-"+id, "book-"+id)
	}

	collection, err := s.CreateCollection(ctx, "Legacy collection", true, []string{"series-a"})
	if err != nil {
		t.Fatal(err)
	}
	collectionRows, err := s.CollectionRowIDs(ctx, []string{collection.ID})
	if err != nil {
		t.Fatal(err)
	}
	execV33(t, s, `DROP TABLE user_hidden_libraries`)
	if err := applyMigrations(s.db); err != nil {
		t.Fatal(err)
	}
	admin, err := s.UserByID(ctx, PrimaryAdminUserID)
	if err != nil || admin.Username != "owner" || admin.HiddenLibraryIDs == nil || len(admin.HiddenLibraryIDs) != 0 {
		t.Fatalf("migrated admin: %+v, %v", admin, err)
	}
	gotRows, err := s.CollectionRowIDs(ctx, []string{collection.ID})
	if err != nil || gotRows[collection.ID] == 0 || gotRows[collection.ID] != collectionRows[collection.ID] {
		t.Fatalf("collection rowid changed: %v -> %v, %v", collectionRows, gotRows, err)
	}
	user, err := s.CreateUser(ctx, CreateUserInput{
		Username: "reader", Password: "test-password", AllowedLibraryIDs: []string{"a", "b", "disabled", "member", "union"},
	})
	if err != nil {
		t.Fatal(err)
	}

	scopeCtx := WithUserScope(ctx, user.Scope())
	seriesQuery := SeriesQuery{Size: 20}
	if _, n, err := s.SeriesPage(scopeCtx, seriesQuery); err != nil || n != 3 {
		t.Fatalf("warm visible series: %d, %v", n, err)
	}
	before, err := s.shelfRevision(ctx, false)
	if err != nil {
		t.Fatal(err)
	}
	hidden, err := s.SetUserHiddenLibraries(ctx, user.ID, []string{"union", " b ", "a", "a", "unknown", "c"})
	if err != nil || !slices.Equal(hidden, []string{"a", "b", "union"}) {
		t.Fatalf("normalized hidden library IDs = %v, %v", hidden, err)
	}
	after, err := s.shelfRevision(ctx, false)
	if err != nil || after.catalog <= before.catalog {
		t.Fatalf("shelf revision did not advance: %v -> %v, %v", before, after, err)
	}
	if err := applyMigrations(s.db); err != nil {
		t.Fatal(err)
	}
	user, _, err = s.UserByUsername(ctx, "reader")
	if err != nil || !slices.Equal(user.HiddenLibraryIDs, hidden) {
		t.Fatalf("hidden library IDs did not survive migration: %v, %v", user.HiddenLibraryIDs, err)
	}
	scope := user.Scope()
	hiddenCtx := WithUserScope(ctx, scope)
	if !scope.AllowsLibrary("a") || scope.DisplaysLibrary("a") || scope.AllowsLibrary("c") {
		t.Fatalf("unexpected scope: %+v", scope)
	}
	if s.IsLibraryVisible(hiddenCtx, "a") || !s.IsLibraryAccessible(hiddenCtx, "a") ||
		s.IsLibraryAccessible(hiddenCtx, "c") || s.IsLibraryAccessible(hiddenCtx, "disabled") || s.IsLibraryAccessible(hiddenCtx, "member") {
		t.Fatal("visibility and accessibility did not distinguish preference, authorization, and library state")
	}
	reversedGrants := slices.Clone(user.AllowedLibraryIDs)
	slices.Reverse(reversedGrants)
	if s.VisibilityCacheKey(hiddenCtx) == s.VisibilityCacheKey(scopeCtx) ||
		scope.VisibilityKey() != (UserScope{AllowedLibraryIDs: reversedGrants, HiddenLibraries: map[string]struct{}{"union": {}, "b": {}, "a": {}}}).VisibilityKey() {
		t.Fatal("visibility cache key must include a canonical hidden set")
	}
	if _, n, err := s.SeriesPage(hiddenCtx, seriesQuery); err != nil || n != 0 {
		t.Fatalf("hidden cached series: %d, %v", n, err)
	}
	items, n, err := s.SeriesPage(hiddenCtx, SeriesQuery{LibraryIDs: []string{"a", "c", "disabled", "member"}, Size: 20})
	if err != nil || n != 1 || len(items) != 1 || items[0].ID != "series-a" {
		t.Fatalf("explicit library scope: %+v, %d, %v", items, n, err)
	}
	if _, n, err := s.SeriesPage(scopeCtx, seriesQuery); err != nil || n != 3 {
		t.Fatalf("original visibility scope: %d, %v", n, err)
	}
	for _, id := range []string{"a", "union"} {
		if _, err := s.SeriesByID(hiddenCtx, "series-"+id); err != nil {
			t.Fatal(err)
		}
		if _, err := s.BookByID(hiddenCtx, "book-"+id); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.SeriesByID(hiddenCtx, "series-c"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("unauthorized direct series: %v", err)
	}
	if authorized, err := s.AuthorizedVisibleLibrariesBasic(hiddenCtx); err != nil || len(authorized) != 3 {
		t.Fatalf("authorized libraries: %+v, %v", authorized, err)
	}
	if visible, err := s.LibrariesBasic(hiddenCtx, true); err != nil || len(visible) != 0 {
		t.Fatalf("displayed libraries: %+v, %v", visible, err)
	}
	if l, series, books, _, err := s.Counts(hiddenCtx); err != nil || l != 0 || series != 0 || books != 0 {
		t.Fatalf("visible counts: %d, %d, %d, %v", l, series, books, err)
	}

	grants := []string{"a", "b", "c"}
	user, err = s.UpdateUser(ctx, user.ID, UpdateUserInput{AllowedLibraryIDs: &grants})
	if err != nil || !slices.Equal(user.HiddenLibraryIDs, []string{"a", "b"}) || !user.Scope().DisplaysLibrary("c") {
		t.Fatalf("new grants should display by default and revoked IDs should disappear: %+v, %v", user, err)
	}
	if err := s.DeleteLibrary(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	user, err = s.UserByID(ctx, user.ID)
	if err != nil || !slices.Equal(user.HiddenLibraryIDs, []string{"b"}) {
		t.Fatalf("library deletion must cascade: %+v, %v", user, err)
	}
	if err := s.DeleteUser(ctx, user.ID); err != nil {
		t.Fatal(err)
	}
	var count int
	if err := s.db.QueryRow(`SELECT count(*) FROM user_hidden_libraries`).Scan(&count); err != nil || count != 0 {
		t.Fatalf("user deletion must cascade: %d, %v", count, err)
	}
}

func TestV33AdminShowNSFWWritesAreAtomic(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "nsfw.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if _, err := s.ClaimAdmin(ctx, "owner", "test-password"); err != nil {
		t.Fatal(err)
	}
	check := func(want bool) {
		t.Helper()
		admin, err := s.UserByID(ctx, PrimaryAdminUserID)
		if err != nil || admin.ShowNSFW != want || s.BoolSetting(ctx, "show_nsfw", !want) != want {
			t.Fatalf("unexpected synchronized NSFW state: user=%v setting=%v err=%v", admin.ShowNSFW, s.ShowNSFW(ctx), err)
		}
	}
	if err := s.SetSetting(ctx, "show_nsfw", "1"); err != nil {
		t.Fatal(err)
	}
	check(true)
	if show, err := s.SetUserShowNSFW(ctx, PrimaryAdminUserID, false); show || err != nil {
		t.Fatalf("SetUserShowNSFW: %v, %v", show, err)
	}
	check(false)

	execV33(t, s, `CREATE TRIGGER reject_nsfw_setting BEFORE UPDATE ON settings
WHEN new.key='show_nsfw' BEGIN SELECT raise(ABORT,'test rejection'); END`)
	if _, err := s.SetUserShowNSFW(ctx, PrimaryAdminUserID, true); err == nil {
		t.Fatal("expected setting failure to propagate")
	}
	if err := s.SetSetting(ctx, "show_nsfw", "true"); err == nil {
		t.Fatal("expected direct setting failure to propagate")
	}
	check(false)
	execV33(t, s, `DROP TRIGGER reject_nsfw_setting`)
	execV33(t, s, `CREATE TRIGGER reject_nsfw_user BEFORE UPDATE OF show_nsfw ON users
BEGIN SELECT raise(ABORT,'test rejection'); END`)
	if err := s.SetSetting(ctx, "show_nsfw", "true"); err == nil {
		t.Fatal("expected admin preference failure to propagate")
	}
	check(false)
}
