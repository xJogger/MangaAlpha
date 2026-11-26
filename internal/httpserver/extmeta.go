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
	maxMetadataUploadFileBytes int64 = 10 << 30
	maxHTTPMetadataUploadBytes int64 = maxMetadataUploadFileBytes + (2 << 20) // 10 GiB upload + multipart framing
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

	fields := make(map[string]string)
	var uploadFilename, tempFilePath string
	defer func() {
		if tempFilePath != "" {
			_ = os.Remove(tempFilePath)
		}
	}()
	readError := func(err error) {
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) || r.Context().Err() != nil {
			s.metadataError(w, err)
			return
		}
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid multipart upload.")
	}

	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			readError(err)
			return
		}
		name := part.FormName()
		switch name {
		case "file":
			if tempFilePath != "" || part.FileName() == "" {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Expected exactly one file part.")
				return
			}
			uploadFilename = sanitizeUploadFilename(part.FileName())
			tmp, err := os.CreateTemp("", ".fk115-upload-part-*")
			if err != nil {
				s.metadataError(w, err)
				return
			}
			tempFilePath = tmp.Name()
			size, copyErr := io.Copy(tmp, io.LimitReader(part, maxMetadataUploadFileBytes+1))
			closeErr := tmp.Close()
			if copyErr != nil {
				var pathErr *os.PathError
				if errors.As(copyErr, &pathErr) {
					s.metadataError(w, copyErr)
				} else {
					readError(copyErr)
				}
				return
			}
			if closeErr != nil {
				s.metadataError(w, closeErr)
				return
			}
			if size > maxMetadataUploadFileBytes {
				s.metadataError(w, &http.MaxBytesError{Limit: maxMetadataUploadFileBytes})
				return
			}
		case "sourceId", "name", "description", "kind", "enabled", "tagDictSourceId", "tagDisplayMode", "autoUpdate":
			if _, duplicate := fields[name]; duplicate || part.FileName() != "" {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid or repeated multipart field.")
				return
			}
			data, err := io.ReadAll(io.LimitReader(part, maxUploadFormFieldBytes+1))
			if err != nil {
				readError(err)
				return
			}
			if int64(len(data)) > maxUploadFormFieldBytes {
				writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Multipart form field exceeds size limit.")
				return
			}
			fields[name] = strings.TrimSpace(string(data))
		default:
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Unknown multipart field.")
			return
		}
	}
	// NextPart stops at the closing delimiter. Drain any epilogue so the
	// request size limit still applies to the entire body.
	if _, err := io.Copy(io.Discard, r.Body); err != nil {
		readError(err)
		return
	}
	if tempFilePath == "" {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Missing uploaded metadata file.")
		return
	}
	if kind := fields["kind"]; kind != "" && kind != extmeta.KindOfflineSQLite {
		s.metadataError(w, extmeta.ErrInvalidConfig)
		return
	}
	sourceID := strings.TrimSpace(r.URL.Query().Get("sourceId"))
	if id, supplied := fields["sourceId"]; supplied {
		if sourceID != "" && id != sourceID {
			s.metadataError(w, extmeta.ErrInvalidConfig)
			return
		}
		sourceID = id
	}
	var cfg extmeta.SourceConfig
	if name := fields["name"]; name != "" {
		cfg.Name = &name
	} else if sourceID == "" {
		name := defaultUploadSourceName(uploadFilename)
		cfg.Name = &name
	}
	if v := fields["description"]; v != "" {
		cfg.Description = &v
	}
	if v, ok := fields["enabled"]; ok {
		enabled, valid := parseUploadBool(v)
		if !valid {
			s.metadataError(w, extmeta.ErrInvalidConfig)
			return
		}
		cfg.Enabled = &enabled
	}
	// An omitted multipart field preserves the binding. A present empty field clears it.
	tagDictSourceID, hasTagDictField := fields["tagDictSourceId"]
	if hasTagDictField {
		cfg.TagDictSourceID = &tagDictSourceID
	}
	if v, ok := fields["tagDisplayMode"]; ok {
		cfg.TagDisplayMode = &v
	}
	if v := fields["autoUpdate"]; v != "" {
		cfg.AutoUpdate = &v
	}

	f, err := os.Open(tempFilePath)
	if err != nil {
		s.metadataError(w, err)
		return
	}
	defer f.Close()
	src, err := s.metadata.ImportConfiguredSQLite(r.Context(), sourceID, uploadFilename, f, cfg, nil)
	if err != nil {
		s.metadataError(w, err)
		return
	}
	status := http.StatusOK
	if sourceID == "" {
		status = http.StatusCreated
	}
	writeJSON(w, status, s.metadata.SourceView(r.Context(), src))
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

func parseUploadBool(raw string) (value, valid bool) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true, true
	case "0", "false", "no", "off":
		return false, true
	default:
		return false, false
	}
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
