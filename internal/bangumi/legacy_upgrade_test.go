package bangumi

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xJogger/fake-komga-115/internal/database"
)

func TestLegacyArchiveAutoUpgrade(t *testing.T) {
	for _, tc := range []struct {
		name       string
		edit       string
		wantLegacy bool
		managed    bool
		fail       bool
		delay      time.Duration
	}{
		{name: "v4 same dump upgrades", edit: `UPDATE meta SET value='4' WHERE key='import_format_version'`, wantLegacy: true, managed: true},
		{name: "failed attempt with anonymous legacy dump", edit: `DELETE FROM meta WHERE key IN ('import_format_version','dump_name','dump_created_at','dump_digest')`, wantLegacy: true, fail: true},
		{name: "v5 with relations"},
		{name: "v5 custom without relations", edit: `DELETE FROM bgm_subject_relations`},
		{name: "new installation"},
		{name: "shutdown during delay", edit: `UPDATE meta SET value='4' WHERE key='import_format_version'`, wantLegacy: true, delay: time.Hour},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			dir := t.TempDir()
			store, err := database.Open(filepath.Join(dir, "main.db"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			dumpPath := filepath.Join(dir, "fixture.zip")
			writeSyntheticBangumiZip(t, dumpPath)
			dump, err := os.ReadFile(dumpPath)
			if err != nil {
				t.Fatal(err)
			}
			if tc.name != "new installation" {
				index, err := NewArchiveIndex(dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				if err := index.ImportFromZipFile(ctx, dumpPath, "fixture.zip", "2025-01-01T00:00:00Z"); err != nil {
					t.Fatal(err)
				}
				if err := index.Close(); err != nil {
					t.Fatal(err)
				}
				if tc.edit != "" {
					editLocalArchive(t, dir, tc.edit)
				}
			}
			if tc.managed {
				if err := store.SetSetting(ctx, settingArchiveMetadataManaged, "true"); err != nil {
					t.Fatal(err)
				}
			}

			var manifestCalls, downloadCalls atomic.Int32
			newService := func() *Service {
				t.Helper()
				svc, err := NewService(store, dir, nil)
				if err != nil {
					t.Fatal(err)
				}
				svc.archive.SetMetaURL("https://archive.example/latest.json")
				mock := &http.Client{Transport: matchingTransport(func(req *http.Request) (*http.Response, error) {
					var payload []byte
					switch req.URL.Path {
					case "/latest.json":
						manifestCalls.Add(1)
						if tc.fail {
							return nil, errors.New("offline fixture transport")
						}
						payload, _ = json.Marshal(latestDumpMeta{
							Name: "fixture.zip", CreatedAt: "2025-01-01T00:00:00Z",
							BrowserDownloadURL: "https://archive.example/dump.zip", Size: int64(len(dump)),
						})
					case "/dump.zip":
						downloadCalls.Add(1)
						payload = dump
					default:
						t.Errorf("unexpected archive request %s", req.URL.Path)
						return nil, errors.New("unexpected archive request")
					}
					return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader(payload))}, nil
				})}
				if tc.managed {
					svc.SetMetadataArchiveClient(func(context.Context) *http.Client { return mock })
				} else {
					svc.archive.clientMu.Lock()
					svc.archive.httpClient = mock
					svc.archive.clientMu.Unlock()
				}
				t.Cleanup(func() { _ = svc.Close() })
				return svc
			}

			svc := newService()
			legacy, signature := svc.archive.NeedsLegacyFormatUpgrade()
			if legacy != tc.wantLegacy || (signature != "") != tc.wantLegacy {
				t.Fatalf("legacy detection = %t, %q", legacy, signature)
			}
			if tc.fail && signature != "legacy:5" {
				t.Fatalf("anonymous legacy signature = %q", signature)
			}
			svc.StartLegacyArchiveAutoUpgrade(tc.delay)
			svc.StartLegacyArchiveAutoUpgrade(tc.delay)
			if tc.delay == 0 {
				svc.archiveWorkers.Wait()
			}
			status := svc.ArchiveStatus()
			if err := svc.Close(); err != nil {
				t.Fatal(err)
			}
			marked, err := store.GetSetting(ctx, settingArchiveLegacyUpgradeAttempt)
			if tc.wantLegacy && tc.delay == 0 {
				if err != nil || marked != signature || manifestCalls.Load() != 1 {
					t.Fatalf("attempt marker = %q, %v, manifest calls = %d", marked, err, manifestCalls.Load())
				}
				if tc.fail {
					if status.LastCheckResult != "failed" || downloadCalls.Load() != 0 {
						t.Fatalf("failed update status = %+v, downloads = %d", status, downloadCalls.Load())
					}
				} else {
					if status.LastCheckResult != "updated" || status.NeedsTankobonReimport || downloadCalls.Load() != 1 {
						t.Fatalf("upgraded status = %+v, downloads = %d", status, downloadCalls.Load())
					}
					// A restored v4 index with the same dump signature must also
					// respect the attempt already recorded in the main database.
					editLocalArchive(t, dir, tc.edit)
				}
			} else if !errors.Is(err, sql.ErrNoRows) || manifestCalls.Load() != 0 || downloadCalls.Load() != 0 {
				t.Fatalf("unexpected marker or request: setting=%q err=%v manifest=%d download=%d", marked, err, manifestCalls.Load(), downloadCalls.Load())
			}

			restarted := newService()
			priorCalls := manifestCalls.Load()
			restarted.StartLegacyArchiveAutoUpgrade(tc.delay)
			if tc.delay == 0 {
				restarted.archiveWorkers.Wait()
			} else if err := restarted.Close(); err != nil {
				t.Fatal(err)
			}
			if manifestCalls.Load() != priorCalls {
				t.Fatalf("restart repeated attempt, manifest calls %d -> %d", priorCalls, manifestCalls.Load())
			}
		})
	}
}

func editLocalArchive(t *testing.T, dir, statement string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "bangumi-archive.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.ExecContext(t.Context(), statement); err != nil {
		t.Fatal(err)
	}
}
