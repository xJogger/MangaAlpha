package bangumi

import (
	"context"
	"database/sql"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xJogger/fake-komga-115/internal/database"
	"golang.org/x/time/rate"
)

func TestArchiveTankobonReimportStatus(t *testing.T) {
	dir := t.TempDir()
	archive, err := NewArchiveIndex(dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer archive.Close()
	dump := filepath.Join(dir, "synthetic.zip")
	writeSyntheticBangumiZip(t, dump)
	if err := archive.ImportFromZipFile(t.Context(), dump, "fixture.zip", "2025-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if st := archive.Status(); !st.Ready || st.NeedsTankobonReimport || st.TankobonRelationCount != 1 {
		t.Fatalf("v5 archive: %+v", st)
	}
	db, err := sql.Open("sqlite", archive.dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	for _, tc := range []struct {
		sql   string
		count int
	}{
		{`UPDATE meta SET value='4' WHERE key='import_format_version'`, 1},
		{`DELETE FROM meta WHERE key='import_format_version'`, 1},
		{`INSERT INTO meta VALUES('import_format_version','5'); DELETE FROM bgm_subject_relations WHERE relation_type=1003`, 0},
	} {
		if _, err := db.Exec(tc.sql); err != nil {
			t.Fatal(err)
		}
		archive.openExistingIfPresent()
		st := archive.Status()
		if !st.Ready || !st.NeedsTankobonReimport || st.TankobonRelationCount != tc.count {
			t.Fatalf("archive requiring reimport: %+v", st)
		}
	}
}

func TestVerifyTankobonRatingsChecksOfficialDetailOnce(t *testing.T) {
	live := NewLiveClient()
	live.limiter = rate.NewLimiter(rate.Inf, 1)
	live.readToken = func(context.Context) string { return "fixture-token" }
	calls := map[string]int{}
	live.http = newLiveHTTPClient(matchingTransport(func(req *http.Request) (*http.Response, error) {
		if req.Header.Get("Authorization") != "" {
			t.Fatal("restricted detail request was authenticated")
		}
		path := req.URL.Path
		calls[path]++
		var raw string
		status := http.StatusOK
		switch path {
		case "/v0/subjects/1":
			raw = `{"id":1,"nsfw":false}`
		case "/v0/subjects/2":
			raw = `{"id":2,"nsfw":true}`
		case "/v0/subjects/3":
			raw = `{"id":3}`
		case "/v0/subjects/4":
			raw = `{"id":404,"nsfw":false}`
		case "/v0/subjects/5":
			status = http.StatusForbidden
		default:
			t.Fatalf("unexpected detail request %s", path)
		}
		return &http.Response{StatusCode: status, Body: io.NopCloser(strings.NewReader(raw)), Header: make(http.Header), Request: req}, nil
	}))
	ctx := database.WithUserScope(t.Context(), database.UserScope{UserID: "guest", AllowNSFW: true})
	matches := []VolumeMatchResult{{SubjectID: 1}, {SubjectID: 1}, {SubjectID: 2}, {SubjectID: 3}, {SubjectID: 4}, {SubjectID: 5}, {SubjectID: 6, NSFWKnown: true}}
	live.VerifyTankobonRatings(ctx, matches)
	if !matches[0].NSFWKnown || matches[0].NSFW || !matches[1].NSFWKnown || calls["/v0/subjects/1"] != 1 || len(calls) != 5 {
		t.Fatalf("public verification: %+v calls=%+v", matches, calls)
	}
	for _, m := range matches[2:6] {
		if m.NSFWKnown {
			t.Fatalf("unverified subject became public: %+v", m)
		}
	}
}

func TestOfflineBackfillPreservesMergedFields(t *testing.T) {
	store, err := database.Open(filepath.Join(t.TempDir(), "main.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := t.Context()
	dir := t.TempDir()
	svc, err := NewService(store, dir, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer svc.Close()
	dump := filepath.Join(dir, "fixture.zip")
	writeSyntheticBangumiZip(t, dump)
	if err := svc.ImportArchiveZip(ctx, dump, "fixture.zip", "2025-01-01T00:00:00Z"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.DB().ExecContext(ctx, `
INSERT INTO libraries(id,name,root_cid,enabled,created_at,updated_at) VALUES ('lib','Catalog','root',1,'','');
INSERT INTO series(id,library_id,cid,name,relative_path,created_at,updated_at,seen_scan_id) VALUES ('work','lib','work','Folder','Folder','','','');
INSERT INTO series_metadata(series_id,bgm_subject_id,title,summary,tags_json,is_nsfw,match_origin,updated_at)
VALUES('work',100,'Preferred title','Merged summary','["merged"]',1,'auto','');
INSERT INTO series_source_bindings(series_id,source_id,entry_id,match_origin,updated_at) VALUES('work','komga','entry','auto','');
 `); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.syncOfflineMetadata(ctx, true); err != nil {
		t.Fatal(err)
	}
	meta, err := store.SeriesMetadataByID(ctx, database.Series{ID: "work", Name: "Folder"})
	if err != nil || meta.Title != "Preferred title" || meta.Summary != "Merged summary" || len(meta.Tags) != 1 || meta.Tags[0] != "merged" ||
		!meta.IsNSFW || meta.BgmScore == nil || *meta.BgmScore != 9.4 || meta.BgmRank == nil || *meta.BgmRank != 3 {
		t.Fatalf("offline refresh: %+v %v", meta, err)
	}
}
