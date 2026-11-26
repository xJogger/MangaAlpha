package httpserver

import (
	"database/sql"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/xJogger/fake-komga-115/internal/database"
	"github.com/xJogger/fake-komga-115/internal/extmeta"
)

func (s *Server) patchSeriesOneShot(w http.ResponseWriter, r *http.Request) {
	var req struct {
		OneShotOverride *bool `json:"oneShotOverride"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	if req.OneShotOverride == nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "oneShotOverride is required.")
		return
	}

	ctx := r.Context()
	series, err := s.store.SeriesByID(ctx, chi.URLParam(r, "seriesID"))
	if err == nil {
		err = s.store.SetSeriesOneShotOverride(ctx, series.ID, *req.OneShotOverride)
	}
	if err == nil {
		series, err = s.store.SeriesByID(ctx, series.ID)
	}
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
	case errors.Is(err, database.ErrInvalidOneShotOverride):
		writeError(w, http.StatusBadRequest, "INVALID_ONESHOT_OVERRIDE", database.ErrInvalidOneShotOverride.Error())
	case err != nil:
		s.writeDBError(w, r, err)
	default:
		writeJSON(w, http.StatusOK, map[string]any{
			"seriesId":         series.ID,
			"oneShot":          series.OneShot,
			"oneShotOverride":  series.OneShotOverride,
			"effectiveOneShot": series.EffectiveOneShot(),
			"canToggleOneShot": !series.OneShot && series.BooksCount == 1,
		})
	}
}

func (s *Server) putBookCustomCover(w http.ResponseWriter, r *http.Request) {
	book, ok := s.adminV34Book(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	mediaType, _, _ := mime.ParseMediaType(r.Header.Get("Content-Type"))
	var err error
	switch mediaType {
	case "multipart/form-data":
		raw, ok := readBookCoverUpload(w, r)
		if !ok {
			return
		}
		_, err = s.thumbs.SaveBookCustomCover(ctx, book.ID, raw)
		// The thumbnail service wraps image decoding errors with these messages.
		if err != nil && (strings.HasPrefix(err.Error(), "decode cover image") ||
			err.Error() == "cover image dimensions are invalid or too large") {
			writeError(w, http.StatusBadRequest, "INVALID_COVER", "Invalid or unsupported cover image.")
			return
		}
	case "application/json":
		var req struct {
			URL string `json:"url"`
		}
		if !decodeJSON(w, r, &req) {
			return
		}
		rawURL := strings.TrimSpace(req.URL)
		if err := extmeta.ValidateExternalURL(rawURL); err != nil {
			writeError(w, http.StatusBadRequest, "INVALID_COVER_URL", "A valid public HTTP or HTTPS cover URL is required.")
			return
		}
		prev, _, _ := s.store.BookMetadataByID(ctx, book.ID)
		// Removing an uploaded file can reset an explicit custom selection.
		if prev.CustomCoverPath != "" {
			_ = s.thumbs.DeleteBookCustomCover(ctx, book.ID)
		}
		err = s.store.SetBookCustomCover(ctx, book.ID, "", rawURL)
		if err == nil && prev.CustomCoverPath != "" && prev.CoverSource == "custom" {
			err = s.store.SetBookCoverSource(ctx, book.ID, "custom")
		}
	default:
		writeError(w, http.StatusUnsupportedMediaType, "UNSUPPORTED_MEDIA_TYPE", "Use multipart/form-data or application/json.")
		return
	}
	if err != nil {
		writeBookCoverError(w, err)
		return
	}
	bm, found, err := s.store.BookMetadataByID(ctx, book.ID)
	if err == nil && !found {
		err = sql.ErrNoRows
	}
	if err != nil {
		writeBookCoverError(w, err)
		return
	}
	if source := bookCoverSource(bm); source != database.CoverSourceAuto && source != "custom" {
		if err := s.store.SetBookCoverSource(ctx, book.ID, "custom"); err != nil {
			writeBookCoverError(w, err)
			return
		}
		bm.CoverSource = "custom"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"bookId":         book.ID,
		"coverSource":    bookCoverSource(bm),
		"hasCustomCover": bm.CustomCoverPath != "" || bm.CustomCoverURL != "",
		"customCoverUrl": bm.CustomCoverURL,
	})
}

func readBookCoverUpload(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	const maxCoverSize = 10 << 20
	const maxMultipartOverhead = 1 << 20
	r.Body = http.MaxBytesReader(w, r.Body, maxCoverSize+maxMultipartOverhead)
	reader, err := r.MultipartReader()
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid multipart cover upload.")
		return nil, false
	}
	var raw []byte
	for {
		part, err := reader.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			writeBookCoverUploadError(w, err)
			return nil, false
		}
		if part.FormName() != "file" {
			// NextPart drains fields that are not needed by the cover endpoint.
			continue
		}
		if raw != nil {
			writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Upload only one cover file.")
			return nil, false
		}
		raw, err = io.ReadAll(io.LimitReader(part, maxCoverSize+1))
		if err != nil {
			writeBookCoverUploadError(w, err)
			return nil, false
		}
		if len(raw) > maxCoverSize {
			writeError(w, http.StatusRequestEntityTooLarge, "COVER_TOO_LARGE", "Cover file exceeds 10 MiB.")
			return nil, false
		}
	}
	if len(raw) == 0 {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "A cover file is required.")
		return nil, false
	}
	return raw, true
}

func writeBookCoverUploadError(w http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeError(w, http.StatusRequestEntityTooLarge, "COVER_TOO_LARGE", "Multipart cover upload is too large.")
		return
	}
	writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid multipart cover upload.")
}

func (s *Server) deleteBookCustomCover(w http.ResponseWriter, r *http.Request) {
	book, ok := s.adminV34Book(w, r)
	if !ok {
		return
	}
	if err := s.thumbs.DeleteBookCustomCover(r.Context(), book.ID); err != nil {
		writeBookCoverError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) patchBookCoverSource(w http.ResponseWriter, r *http.Request) {
	var req struct {
		CoverSource string `json:"coverSource"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	switch req.CoverSource {
	case database.CoverSourceAuto, "custom", "ext", database.CoverSourceBangumi, database.CoverSourceBook:
	default:
		writeError(w, http.StatusBadRequest, "INVALID_COVER_SOURCE", "Unknown book cover source.")
		return
	}
	book, ok := s.adminV34Book(w, r)
	if !ok {
		return
	}
	if err := s.store.SetBookCoverSource(r.Context(), book.ID, req.CoverSource); err != nil {
		writeBookCoverError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"bookId": book.ID, "coverSource": req.CoverSource})
}

func (s *Server) refreshBookComicInfo(w http.ResponseWriter, r *http.Request) {
	req := struct {
		Force bool `json:"force"`
	}{Force: true}
	decoder := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&req); err != nil && err != io.EOF {
		writeError(w, http.StatusBadRequest, "INVALID_REQUEST", "Invalid ComicInfo refresh options.")
		return
	}
	book, ok := s.adminV34Book(w, r)
	if !ok {
		return
	}
	status, _, err := s.archive.ExtractBookComicInfoWithOptions(r.Context(), book, req.Force)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
	case err != nil:
		writeError(w, http.StatusBadGateway, "COMICINFO_REFRESH_FAILED", "Failed to read ComicInfo from the comic archive.")
	default:
		writeJSON(w, http.StatusOK, map[string]string{"bookId": book.ID, "status": status})
	}
}

func (s *Server) adminV34Book(w http.ResponseWriter, r *http.Request) (database.Book, bool) {
	book, err := s.store.BookByID(r.Context(), chi.URLParam(r, "bookID"))
	switch {
	case errors.Is(err, sql.ErrNoRows):
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
	case err != nil:
		s.writeDBError(w, r, err)
	}
	return book, err == nil
}

func writeBookCoverError(w http.ResponseWriter, err error) {
	if errors.Is(err, sql.ErrNoRows) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
		return
	}
	writeError(w, http.StatusInternalServerError, "BOOK_COVER_ERROR", "Failed to update the book cover.")
}

func bookCoverSource(bm database.BookMetadata) string {
	if bm.CoverSource == "" {
		return database.CoverSourceAuto
	}
	return bm.CoverSource
}

func bookMetadataSummary(bm database.BookMetadata) map[string]any {
	return map[string]any{
		"coverSource":        bookCoverSource(bm),
		"hasCustomCover":     bm.CustomCoverPath != "" || bm.CustomCoverURL != "",
		"hasExtCover":        bm.ExtCoverPath != "" || bm.ExtCoverURL != "",
		"bgmSubjectId":       bm.BgmSubjectID,
		"comicInfoStatus":    bm.ComicInfoStatus,
		"numberSortExplicit": bm.NumberSort != nil,
	}
}
