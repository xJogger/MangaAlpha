package httpserver

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
)

const (
	maxHTTPMetadataUploadBytes int64 = (10 << 30) + (2 << 20) // 10 GiB upload + multipart framing
	maxUploadFormFieldBytes    int64 = 64 << 10
)

func (s *Server) metadataRoutes(r chi.Router) {
	r.Group(func(r chi.Router) {
		r.Use(s.requireMetadata)
		r.Get("/metadata/overview", s.metadataOverview)
		r.Post("/metadata/repos", s.subscribeMetadataRepo)
		r.Post("/metadata/repos/{repoID}/refresh", s.refreshMetadataRepo)
		r.Delete("/metadata/repos/{repoID}", s.deleteMetadataRepo)
		r.Get("/metadata/sources", s.listMetadataSources)
		r.Post("/metadata/sources/upload", s.uploadMetadataSource)
		r.Post("/metadata/sources", s.addMetadataSource)
		r.Put("/metadata/sources/{sourceID}", s.updateMetadataSource)
		r.Delete("/metadata/sources/{sourceID}", s.deleteMetadataSource)
		r.Post("/metadata/sources/{sourceID}/sync", s.startMetadataSync)
		r.Get("/metadata/syncs", s.listMetadataSyncs)
		r.Put("/libraries/{libraryID}/metadata-sources", s.setLibraryMetadataSources)
		r.Post("/metadata/scrape", s.startMetadataScrape)
		r.Get("/metadata/scrape-runs", s.listMetadataScrapes)
		r.Post("/metadata/scrape-runs/{runID}/cancel", s.cancelMetadataScrape)
		r.Get("/series/{seriesID}/metadata/candidates", s.metadataCandidates)
		r.Post("/series/{seriesID}/metadata/bind", s.bindMetadata)
		r.Delete("/series/{seriesID}/metadata/bind", s.clearMetadataBinding)
		r.Get("/metadata/sources/{sourceID}/entries/{entryID}/cover", s.metadataEntryCover)
		r.Post("/metadata/mappings/export", s.exportCommunityMappings)
	})
}

func (s *Server) requireMetadata(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "private, no-store")
		if s.metadata == nil {
			writeError(w, http.StatusServiceUnavailable, "METADATA_UNAVAILABLE", "Metadata service is unavailable.")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) metadataError(w http.ResponseWriter, err error) {
	var netErr net.Error
	var maxBytesErr *http.MaxBytesError
	switch {
	case errors.As(err, &maxBytesErr):
		writeError(w, http.StatusRequestEntityTooLarge, "UPLOAD_TOO_LARGE", "Uploaded metadata file exceeds the size limit.")
	case errors.Is(err, sql.ErrNoRows), errors.Is(err, extmeta.ErrEntryNotFound):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Metadata item not found.")
	case errors.Is(err, extmeta.ErrSSRFBlocked):
		writeError(w, http.StatusBadRequest, "METADATA_SSRF_BLOCKED", extmeta.DescribeError(err))
	case errors.Is(err, extmeta.ErrInvalidProxyURL):
		writeError(w, http.StatusBadRequest, "INVALID_PROXY_URL", extmeta.DescribeError(err))
	case errors.Is(err, extmeta.ErrInvalidManifest):
		writeError(w, http.StatusBadRequest, "INVALID_METADATA_MANIFEST", extmeta.DescribeError(err))
	case errors.Is(err, extmeta.ErrUnsupportedSchema):
		writeError(w, http.StatusBadRequest, "INVALID_METADATA_FORMAT", "Uploaded file is not a supported offline metadata SQLite database.")
	case errors.Is(err, extmeta.ErrInvalidImage):
		writeError(w, http.StatusBadRequest, "INVALID_METADATA_IMAGE", "Cover image format is invalid or unsupported.")
	case errors.Is(err, extmeta.ErrInvalidConfig):
		writeError(w, http.StatusBadRequest, "INVALID_METADATA_CONFIG", "Invalid metadata configuration.")
	case errors.Is(err, extmeta.ErrNoReadySources):
		writeError(w, http.StatusBadRequest, "NO_READY_METADATA_SOURCES", err.Error())
	case errors.Is(err, extmeta.ErrBusy):
		writeError(w, http.StatusConflict, "METADATA_BUSY", "A metadata task is already running.")
	case errors.Is(err, extmeta.ErrUnavailable), errors.Is(err, extmeta.ErrCircuitOpen):
		writeError(w, http.StatusServiceUnavailable, "METADATA_UNAVAILABLE", "Metadata source is unavailable.")
	case errors.Is(err, context.Canceled):
		writeError(w, 499, "REQUEST_CANCELED", "Request canceled.")
	case errors.Is(err, context.DeadlineExceeded), errors.As(err, &netErr) && netErr.Timeout():
		writeError(w, http.StatusGatewayTimeout, "REQUEST_TIMEOUT", extmeta.DescribeError(err))
	case errors.Is(err, extmeta.ErrRemoteMetadata):
		writeError(w, http.StatusBadGateway, "METADATA_UPSTREAM_FAILED", extmeta.DescribeError(err))
	case errors.Is(err, extmeta.ErrNegativeCached):
		writeError(w, http.StatusBadGateway, "METADATA_UPSTREAM_FAILED", "Could not fetch upstream metadata.")
	default:
		writeError(w, http.StatusInternalServerError, "METADATA_FAILED", "Metadata operation failed.")
	}
}

func (s *Server) metadataResult(w http.ResponseWriter, status int, data any, err error) {
	if err != nil {
		s.metadataError(w, err)
		return
	}
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, data)
}

func (s *Server) metadataOverview(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	repos, err := s.store.ListMetadataRepos(ctx)
	if err != nil {
		s.metadataError(w, err)
		return
	}
	sources, err := s.store.ListMetadataSources(ctx, "")
	if err != nil {
		s.metadataError(w, err)
		return
	}
	chains, err := s.store.ListAllLibraryMetadataChains(ctx)
	if err != nil {
		s.metadataError(w, err)
		return
	}
	syncs, online := s.metadata.Syncs(), s.metadata.OnlineEnrichStatus()
	writeJSON(w, http.StatusOK, map[string]any{
		"recommendedRepoUrl": extmeta.RecommendedRepoURL,
		"repos":              repos, "sources": s.metadata.SourceViews(ctx, sources), "libraryChains": chains,
		"syncs": syncs, "activeSyncs": syncs, "scrapeRuns": s.metadata.ScrapeRuns(),
		"onlineEnrich": online, "onlineEnrichStatus": online,
	})
}

func (s *Server) listMetadataSources(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	sources, err := s.store.ListMetadataSources(ctx, strings.TrimSpace(r.URL.Query().Get("kind")))
	if err != nil {
		s.metadataError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sources": s.metadata.SourceViews(ctx, sources),
	})
}

func (s *Server) subscribeMetadataRepo(w http.ResponseWriter, r *http.Request) {
	var req struct {
		URL string `json:"url"`
	}
	if decodeJSON(w, r, &req) {
		repo, err := s.metadata.SubscribeRepo(r.Context(), req.URL)
		s.metadataResult(w, http.StatusCreated, repo, err)
	}
}

func (s *Server) refreshMetadataRepo(w http.ResponseWriter, r *http.Request) {
	repo, err := s.metadata.RefreshRepo(r.Context(), chi.URLParam(r, "repoID"))
	s.metadataResult(w, http.StatusOK, repo, err)
}

func (s *Server) deleteMetadataRepo(w http.ResponseWriter, r *http.Request) {
	s.metadataResult(w, http.StatusNoContent, nil, s.metadata.DeleteRepo(r.Context(), chi.URLParam(r, "repoID")))
}

func (s *Server) addMetadataSource(w http.ResponseWriter, r *http.Request) {
	req := database.MetadataSource{Enabled: true}
	if decodeJSON(w, r, &req) {
		src, err := s.metadata.AddCustomSource(r.Context(), req)
		if err != nil {
			s.metadataError(w, err)
			return
		}
		writeJSON(w, http.StatusCreated, s.metadata.SourceView(r.Context(), src))
	}
}

func (s *Server) updateMetadataSource(w http.ResponseWriter, r *http.Request) {
	var req extmeta.SourceConfig
	if decodeJSON(w, r, &req) {
		src, err := s.metadata.UpdateSourceConfig(r.Context(), chi.URLParam(r, "sourceID"), req)
		if err != nil {
			s.metadataError(w, err)
			return
		}
		writeJSON(w, http.StatusOK, s.metadata.SourceView(r.Context(), src))
	}
}

func (s *Server) uploadMetadataSource(w http.ResponseWriter, r *http.Request) {
	r.Body = http.MaxBytesReader(w, r.Body, maxHTTPMetadataUploadBytes)
	mr, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Expected multipart/form-data upload.")
		return
	}

	ctx := r.Context()
	fields := map[string]string{
		"sourceId": strings.TrimSpace(r.URL.Query().Get("sourceId")),
		"kind":     extmeta.KindOfflineSQLite,
	}
	var (
		uploaded       bool
		createdNew     bool
		importedDirect bool
		uploadedSrc    database.MetadataSource
		uploadFilename string
		tempFilePath   string
	)
	cleanupTemp := func() {
		if tempFilePath != "" {
			_ = os.Remove(tempFilePath)
			tempFilePath = ""
		}
	}
	defer cleanupTemp()

	applyFormField := func(name, val string) error {
		switch name {
		case "sourceId", "name", "description", "kind", "enabled", "tagDictSourceId", "tagDisplayMode", "autoUpdate":
			fields[name] = strings.TrimSpace(val)
		}
		return nil
	}

	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			s.metadataError(w, err)
			return
		}
		formName := strings.TrimSpace(part.FormName())
		if formName != "file" {
			data, readErr := io.ReadAll(io.LimitReader(part, maxUploadFormFieldBytes+1))
			_ = part.Close()
			if readErr != nil {
				s.metadataError(w, readErr)
				return
			}
			if int64(len(data)) > maxUploadFormFieldBytes {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Multipart form field exceeds size limit.")
				return
			}
			_ = applyFormField(formName, string(data))
			continue
		}
		if uploaded {
			_ = part.Close()
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Multiple file parts are not supported.")
			return
		}
		uploaded = true
		uploadFilename = sanitizeUploadFilename(part.FileName())

		if fields["kind"] != "" && fields["kind"] != extmeta.KindOfflineSQLite {
			_ = part.Close()
			writeError(w, http.StatusBadRequest, "INVALID_METADATA_CONFIG", "Invalid metadata source kind for SQLite upload.")
			return
		}

		// When sourceId (or name for a new source) precedes the file part, stream
		// directly into ImportUploadedSQLite without writing an intermediate file.
		if targetID := fields["sourceId"]; targetID != "" {
			uploadedSrc, err = s.metadata.ImportUploadedSQLite(ctx, targetID, uploadFilename, part, nil)
			_ = part.Close()
			if err != nil {
				s.metadataError(w, err)
				return
			}
			importedDirect = true
			continue
		}
		if fields["name"] != "" {
			enabled := parseUploadBool(fields["enabled"], true)
			createdSrc, addErr := s.metadata.AddCustomSource(ctx, database.MetadataSource{
				Kind:            extmeta.KindOfflineSQLite,
				Name:            fields["name"],
				Description:     fields["description"],
				TagDictSourceID: fields["tagDictSourceId"],
				TagDisplayMode:  fields["tagDisplayMode"],
				AutoUpdate:      firstUploadNonEmpty(fields["autoUpdate"], "manual"),
				Enabled:         enabled,
			})
			if addErr != nil {
				_ = part.Close()
				s.metadataError(w, addErr)
				return
			}
			createdNew = true
			fields["sourceId"] = createdSrc.ID
			uploadedSrc, err = s.metadata.ImportUploadedSQLite(ctx, createdSrc.ID, uploadFilename, part, nil)
			_ = part.Close()
			if err != nil {
				_ = s.metadata.DeleteSource(ctx, createdSrc.ID)
				s.metadataError(w, err)
				return
			}
			importedDirect = true
			continue
		}

		// If the file part appeared before sourceId/name, spill once to a bounded temp file.
		tmpFile, tmpErr := os.CreateTemp("", ".fk115-upload-part-*")
		if tmpErr != nil {
			_ = part.Close()
			s.metadataError(w, tmpErr)
			return
		}
		tempFilePath = tmpFile.Name()
		_, copyErr := io.Copy(tmpFile, io.LimitReader(part, maxHTTPMetadataUploadBytes))
		closeErr := tmpFile.Close()
		_ = part.Close()
		if copyErr != nil {
			s.metadataError(w, copyErr)
			return
		}
		if closeErr != nil {
			s.metadataError(w, closeErr)
			return
		}
	}

	if !uploaded {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Missing uploaded metadata file.")
		return
	}
	if fields["kind"] != "" && fields["kind"] != extmeta.KindOfflineSQLite {
		if createdNew {
			_ = s.metadata.DeleteSource(ctx, fields["sourceId"])
		}
		writeError(w, http.StatusBadRequest, "INVALID_METADATA_CONFIG", "Invalid metadata source kind for SQLite upload.")
		return
	}

	if !importedDirect {
		targetID := fields["sourceId"]
		if targetID == "" {
			name := fields["name"]
			if name == "" {
				name = defaultUploadSourceName(uploadFilename)
			}
			enabled := parseUploadBool(fields["enabled"], true)
			createdSrc, addErr := s.metadata.AddCustomSource(ctx, database.MetadataSource{
				Kind:            extmeta.KindOfflineSQLite,
				Name:            name,
				Description:     fields["description"],
				TagDictSourceID: fields["tagDictSourceId"],
				TagDisplayMode:  fields["tagDisplayMode"],
				AutoUpdate:      firstUploadNonEmpty(fields["autoUpdate"], "manual"),
				Enabled:         enabled,
			})
			if addErr != nil {
				s.metadataError(w, addErr)
				return
			}
			createdNew = true
			targetID = createdSrc.ID
			fields["sourceId"] = targetID
		}
		f, openErr := os.Open(tempFilePath)
		if openErr != nil {
			if createdNew {
				_ = s.metadata.DeleteSource(ctx, targetID)
			}
			s.metadataError(w, openErr)
			return
		}
		uploadedSrc, err = s.metadata.ImportUploadedSQLite(ctx, targetID, uploadFilename, f, nil)
		_ = f.Close()
		if err != nil {
			if createdNew {
				_ = s.metadata.DeleteSource(ctx, targetID)
			}
			s.metadataError(w, err)
			return
		}
	}

	// Apply any optional metadata configuration updates supplied alongside an upload
	// (including fields transmitted after the file part).
	cfg := extmeta.SourceConfig{}
	var hasCfg bool
	if !createdNew {
		if v := fields["name"]; v != "" && v != uploadedSrc.Name {
			cfg.Name = &v
			hasCfg = true
		}
		if v := fields["description"]; v != "" && v != uploadedSrc.Description {
			cfg.Description = &v
			hasCfg = true
		}
	}
	if v, ok := fields["enabled"]; ok {
		b := parseUploadBool(v, uploadedSrc.Enabled)
		if b != uploadedSrc.Enabled {
			cfg.Enabled = &b
			hasCfg = true
		}
	}
	if v, ok := fields["tagDictSourceId"]; ok && v != uploadedSrc.TagDictSourceID {
		cfg.TagDictSourceID = &v
		hasCfg = true
	}
	if v, ok := fields["tagDisplayMode"]; ok && v != uploadedSrc.TagDisplayMode {
		cfg.TagDisplayMode = &v
		hasCfg = true
	}
	if v := fields["autoUpdate"]; v != "" && v != uploadedSrc.AutoUpdate {
		cfg.AutoUpdate = &v
		hasCfg = true
	}
	if hasCfg && uploadedSrc.RepoID == "" {
		if updated, cfgErr := s.metadata.UpdateSourceConfig(ctx, uploadedSrc.ID, cfg); cfgErr == nil {
			uploadedSrc = updated
		}
	}

	status := http.StatusOK
	if createdNew {
		status = http.StatusCreated
	}
	writeJSON(w, status, s.metadata.SourceView(ctx, uploadedSrc))
}

func sanitizeUploadFilename(raw string) string {
	base := path.Base(strings.ReplaceAll(strings.TrimSpace(raw), "\\", "/"))
	if base == "" || base == "." || base == "/" {
		return "upload.sqlite"
	}
	return base
}

func defaultUploadSourceName(filename string) string {
	name := strings.TrimSpace(filename)
	for _, ext := range []string{".zstd", ".zst", ".gzip", ".gz", ".zip", ".sqlite3", ".sqlite", ".db"} {
		if strings.HasSuffix(strings.ToLower(name), ext) {
			name = name[:len(name)-len(ext)]
		}
	}
	name = strings.TrimSpace(name)
	if name == "" || name == "." {
		return "Uploaded SQLite Metadata"
	}
	return name
}

func parseUploadBool(raw string, fallback bool) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	case "0", "false", "no", "off":
		return false
	default:
		return fallback
	}
}

func firstUploadNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func (s *Server) deleteMetadataSource(w http.ResponseWriter, r *http.Request) {
	s.metadataResult(w, http.StatusNoContent, nil, s.metadata.DeleteSource(r.Context(), chi.URLParam(r, "sourceID")))
}

func (s *Server) startMetadataSync(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Force bool `json:"force"`
	}
	if r.ContentLength != 0 && !decodeJSON(w, r, &req) {
		return
	}
	run, err := s.metadata.StartSourceSync(r.Context(), chi.URLParam(r, "sourceID"), req.Force)
	s.metadataResult(w, http.StatusAccepted, run, err)
}

func (s *Server) listMetadataSyncs(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"syncs": s.metadata.Syncs()})
}

func (s *Server) setLibraryMetadataSources(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceIDs []string `json:"sourceIds"`
	}
	if decodeJSON(w, r, &req) {
		chain, err := s.metadata.SetLibrarySources(r.Context(), chi.URLParam(r, "libraryID"), req.SourceIDs)
		s.metadataResult(w, http.StatusOK, map[string]any{"sources": chain}, err)
	}
}

func (s *Server) startMetadataScrape(w http.ResponseWriter, r *http.Request) {
	var req struct {
		LibraryID   string `json:"libraryId"`
		Mode        string `json:"mode"`
		AllowOnline bool   `json:"allowOnline"`
	}
	if decodeJSON(w, r, &req) {
		run, err := s.metadata.StartMultiScrape(r.Context(), req.LibraryID, req.Mode, req.AllowOnline)
		s.metadataResult(w, http.StatusAccepted, run, err)
	}
}

func (s *Server) listMetadataScrapes(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"runs": s.metadata.ScrapeRuns(), "onlineEnrich": s.metadata.OnlineEnrichStatus()})
}

func (s *Server) cancelMetadataScrape(w http.ResponseWriter, r *http.Request) {
	s.metadataResult(w, http.StatusNoContent, nil, s.metadata.CancelScrape(chi.URLParam(r, "runID")))
}

func (s *Server) metadataCandidates(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	sourceID := query.Get("sourceId")
	if sourceID == "" {
		sourceID = "bangumi"
	}
	hits, err := s.metadata.SearchCandidates(r.Context(), chi.URLParam(r, "seriesID"), sourceID, query.Get("q"))
	s.metadataResult(w, http.StatusOK, hits, err)
}

func (s *Server) bindMetadata(w http.ResponseWriter, r *http.Request) {
	var req struct {
		SourceID string `json:"sourceId"`
		EntryID  string `json:"entryId"`
	}
	if decodeJSON(w, r, &req) {
		meta, err := s.metadata.BindSeriesExternal(r.Context(), chi.URLParam(r, "seriesID"), req.SourceID, req.EntryID, "manual")
		s.metadataResult(w, http.StatusOK, meta, err)
	}
}

func (s *Server) clearMetadataBinding(w http.ResponseWriter, r *http.Request) {
	meta, err := s.metadata.ClearSeriesExternal(r.Context(), chi.URLParam(r, "seriesID"))
	s.metadataResult(w, http.StatusOK, meta, err)
}

func (s *Server) metadataEntryCover(w http.ResponseWriter, r *http.Request) {
	data, mediaType, err := s.metadata.EntryCover(r.Context(), chi.URLParam(r, "sourceID"), chi.URLParam(r, "entryID"))
	if err != nil {
		s.metadataError(w, err)
		return
	}
	w.Header().Set("Content-Type", mediaType)
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	_, _ = w.Write(data)
}

func (s *Server) exportCommunityMappings(w http.ResponseWriter, r *http.Request) {
	var req struct {
		database.MappingExportFilter
		Filter  *database.MappingExportFilter `json:"filter"`
		Preview bool                          `json:"preview"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.Filter != nil {
		req.MappingExportFilter = *req.Filter
	}
	req.Preview = req.Preview || strings.EqualFold(r.URL.Query().Get("preview"), "true")
	export, err := s.metadata.ExportCommunityMappings(r.Context(), req.MappingExportFilter, req.Preview)
	if err != nil || req.Preview {
		s.metadataResult(w, http.StatusOK, export, err)
		return
	}
	w.Header().Set("Content-Type", "application/x-ndjson")
	w.Header().Set("Content-Disposition", `attachment; filename="metadata-mappings.ndjson"`)
	w.Header().Set("Content-Length", strconv.Itoa(len(export.NDJSON)))
	_, _ = w.Write(export.NDJSON)
}
