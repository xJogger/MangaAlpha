package httpserver

import (
	"database/sql"
	"errors"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/xJogger/fake-komga-115/internal/database"
)

func (s *Server) bookshelfVersions(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	current, err := s.store.SeriesByID(ctx, chi.URLParam(r, "seriesID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, 404, "NOT_FOUND", "Series not found.")
		} else {
			s.writeDBErrorMessage(w, r, err, "Could not load series.")
		}
		return
	}
	scope := strings.TrimSpace(r.URL.Query().Get("library_id"))
	meta, err := s.store.SeriesMetadataByID(ctx, current)
	if err != nil {
		s.writeDBErrorMessage(w, r, err, "Could not load series metadata.")
		return
	}
	if current.EffectiveOneShot() || (meta.BgmSubjectID == nil || *meta.BgmSubjectID <= 0) || (scope != "" && current.LibraryID != scope) {
		writeJSON(w, 200, map[string]any{"versions": []any{}})
		return
	}
	visibleLibs, err := s.store.LibrariesBasic(ctx, true)
	if err != nil {
		s.writeDBErrorMessage(w, r, err, "Could not load visible libraries.")
		return
	}
	if len(visibleLibs) == 0 {
		writeJSON(w, 200, map[string]any{"versions": []any{}})
		return
	}
	where := " WHERE gm.bgm_subject_id=? AND NOT (s.one_shot = 1 OR (s.one_shot_override = 1 AND s.books_count = 1)) AND l.enabled=1 AND NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)"
	args := []any{*meta.BgmSubjectID}
	where += " AND s.library_id IN " + database.Placeholders(len(visibleLibs))
	for _, lib := range visibleLibs {
		args = append(args, lib.ID)
	}
	if !s.store.ShowNSFW(ctx) {
		where += " AND gm.is_nsfw=0"
	}
	if scope != "" {
		where += " AND s.library_id=?"
		args = append(args, scope)
	}
	joins := " FROM series s JOIN series_metadata gm ON gm.series_id=s.id JOIN libraries l ON l.id=s.library_id"
	rows, err := s.store.DB().QueryContext(ctx, "SELECT s.id,s.name,l.name,l.provider_type,s.books_count"+
		joins+where+
		" ORDER BY s.books_count DESC, CASE WHEN l.provider_type='union' THEN 0 ELSE 1 END ASC, l.name COLLATE NOCASE ASC, s.name COLLATE NOCASE ASC, s.id ASC",
		args...)
	if err != nil {
		s.writeDBErrorMessage(w, r, err, "Could not load versions.")
		return
	}
	defer rows.Close()
	type version struct {
		ID         string `json:"id"`
		Name       string `json:"name"`
		Library    string `json:"library"`
		Provider   string `json:"provider"`
		BooksCount int    `json:"booksCount"`
	}
	versions := make([]version, 0)
	for rows.Next() {
		var v version
		if err := rows.Scan(&v.ID, &v.Name, &v.Library, &v.Provider, &v.BooksCount); err != nil {
			s.writeDBErrorMessage(w, r, err, "Could not read versions.")
			return
		}
		versions = append(versions, v)
	}
	if err := rows.Err(); err != nil {
		s.writeDBErrorMessage(w, r, err, "Could not read versions.")
		return
	}
	rows.Close()
	result := map[string]any{"versions": versions}
	var seriesID, bookID string
	var page int
	var done bool
	if len(versions) > 1 {
		userID := database.UserIDFromContext(ctx)
		lastArgs := append([]any{userID}, args...)
		err := s.store.DB().QueryRowContext(ctx, "SELECT s.id,b.id,coalesce(p.page,1),p.completed"+joins+
			" JOIN books b ON b.series_id=s.id JOIN book_read_progress p ON p.book_id=b.id AND p.user_id=?"+
			where+" ORDER BY p.updated_at DESC,b.id LIMIT 1", lastArgs...).Scan(&seriesID, &bookID, &page, &done)
		if err == nil {
			result["lastRead"] = map[string]any{"seriesId": seriesID, "bookId": bookID, "page": page, "completed": done}
		}
	}
	writeJSON(w, 200, result)
}

func (s *Server) bookshelfContinueReading(w http.ResponseWriter, r *http.Request) {
	size := min(50, max(1, intQuery(r, "size", 8)))
	result, err := s.store.ReadingActivity(r.Context(), database.ReadingActivityQuery{
		InProgressOnly: true,
		MergeBangumi:   bookshelfMergeBangumi(r),
		Size:           size,
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	type entry struct {
		bookshelfActivityGroup
		OtherInProgressCount int `json:"otherInProgressCount"`
	}
	content := make([]entry, 0, len(result.Groups))
	for _, group := range result.Groups {
		content = append(content, entry{bookshelfActivityGroupDTO(group), len(group.Books) - 1})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content": content, "totalElements": result.TotalElements,
	})
}

func (s *Server) bookshelfHistory(w http.ResponseWriter, r *http.Request) {
	page := max(0, intQuery(r, "page", 0))
	size := min(100, max(1, intQuery(r, "size", 30)))
	result, err := s.store.ReadingActivity(r.Context(), database.ReadingActivityQuery{
		MergeBangumi: bookshelfMergeBangumi(r),
		Search:       r.URL.Query().Get("search"),
		TZOffset:     intQuery(r, "tz_offset", 0),
		Page:         page,
		Size:         size,
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	type entry struct {
		bookshelfActivityGroup
		DayKey      string `json:"dayKey"`
		SeriesTitle any    `json:"seriesTitle"`
	}
	content := make([]entry, 0, len(result.Groups))
	for _, group := range result.Groups {
		dto := bookshelfActivityGroupDTO(group)
		content = append(content, entry{dto, group.DayKey, dto.Primary["seriesTitle"]})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"content": content, "page": page, "size": size,
		"totalElements": result.TotalElements, "totalBooks": result.TotalBooks,
		"totalPages": (result.TotalElements + int64(size) - 1) / int64(size),
	})
}

func bookshelfMergeBangumi(r *http.Request) bool {
	value := r.URL.Query().Get("merge_bangumi")
	return value == "1" || strings.EqualFold(value, "true")
}

type bookshelfActivityGroup struct {
	GroupKey        string           `json:"groupKey"`
	MergedByBangumi bool             `json:"mergedByBangumi"`
	BgmSubjectID    *int64           `json:"bgmSubjectId"`
	Primary         map[string]any   `json:"primary"`
	Others          []map[string]any `json:"others"`
}

func bookshelfActivityGroupDTO(group database.ReadingActivityGroup) bookshelfActivityGroup {
	bookDTO := func(item database.ReadingActivityBook) map[string]any {
		dto := bookDTOWithProgressAndMeta(item.Book, item.Series, item.Metadata, item.Progress, true)
		if item.Metadata.BgmSubjectID != nil {
			dto["bgmSubjectId"] = *item.Metadata.BgmSubjectID
		}
		return dto
	}
	others := make([]map[string]any, 0, len(group.Books)-1)
	for _, book := range group.Books[1:] {
		others = append(others, bookDTO(book))
	}
	return bookshelfActivityGroup{
		GroupKey: group.GroupKey, MergedByBangumi: group.MergedByBangumi,
		BgmSubjectID: group.Books[0].Metadata.BgmSubjectID,
		Primary:      bookDTO(group.Books[0]), Others: others,
	}
}
