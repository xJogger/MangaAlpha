package httpserver

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/xml"
	"errors"
	"fmt"
	"image"
	_ "image/gif"
	"image/jpeg"
	_ "image/png"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"
	_ "golang.org/x/image/webp"

	"github.com/xJogger/fake-komga-115/internal/archive"
	"github.com/xJogger/fake-komga-115/internal/database"
)

const (
	opdsMIMEFeedNav   = "application/atom+xml;profile=opds-catalog;kind=navigation;charset=utf-8"
	opdsMIMEFeedAcq   = "application/atom+xml;profile=opds-catalog;kind=acquisition;charset=utf-8"
	opdsMIMELinkNav   = "application/atom+xml;profile=opds-catalog;kind=navigation"
	opdsMIMELinkAcq   = "application/atom+xml;profile=opds-catalog;kind=acquisition"
	opdsMIMEOpenSrch  = "application/opensearchdescription+xml"
	opdsRelSubsection = "subsection"
	opdsRelStart      = "start"
	opdsRelSelf       = "self"
	opdsRelNext       = "next"
	opdsRelPrev       = "previous"
	opdsRelSearch     = "search"
	opdsRelFacet      = "http://opds-spec.org/facet"
	opdsRelImage      = "http://opds-spec.org/image"
	opdsRelThumbnail  = "http://opds-spec.org/image/thumbnail"
	opdsRelPSEStream  = "http://vaemendis.net/opds-pse/stream"

	opdsDefaultSeriesPageSize  = 30
	opdsDefaultChapterPageSize = 100
	opdsIndexDetachedTimeout   = 45 * time.Second
)

// --- Atom / OPDS XML Model & Deterministic Serializer ---

type opdsLink struct {
	Rel          string
	Type         string
	Href         string
	Title        string
	FacetGroup   string
	ActiveFacet  bool
	ThrCount     int
	PSECount     int
	PSELastRead  int
	PSELastDate  string
	HasPSEStream bool
}

type opdsEntry struct {
	ID      string
	Title   string
	Updated time.Time
	Authors []string
	Summary string
	Links   []opdsLink
}

type opdsFeed struct {
	ID          string
	Title       string
	Updated     time.Time
	Acquisition bool
	Links       []opdsLink
	Entries     []opdsEntry
}

func writeXMLText(buf *bytes.Buffer, s string) {
	_ = xml.EscapeText(buf, []byte(s))
}

func writeXMLAttr(buf *bytes.Buffer, name, value string) {
	if value == "" {
		return
	}
	buf.WriteByte(' ')
	buf.WriteString(name)
	buf.WriteString(`="`)
	_ = xml.EscapeText(buf, []byte(value))
	buf.WriteByte('"')
}

func writeOPDSLink(buf *bytes.Buffer, l opdsLink) {
	buf.WriteString("    <link")
	writeXMLAttr(buf, "rel", l.Rel)
	writeXMLAttr(buf, "type", l.Type)
	writeXMLAttr(buf, "href", l.Href)
	writeXMLAttr(buf, "title", l.Title)
	writeXMLAttr(buf, "opds:facetGroup", l.FacetGroup)
	if l.ActiveFacet {
		writeXMLAttr(buf, "opds:activeFacet", "true")
	}
	if l.ThrCount > 0 {
		writeXMLAttr(buf, "thr:count", strconv.Itoa(l.ThrCount))
	}
	if l.HasPSEStream {
		writeXMLAttr(buf, "pse:count", strconv.Itoa(l.PSECount))
		if l.PSELastRead > 0 {
			writeXMLAttr(buf, "pse:lastRead", strconv.Itoa(l.PSELastRead))
		}
		if l.PSELastDate != "" {
			writeXMLAttr(buf, "pse:lastReadDate", l.PSELastDate)
		}
	}
	buf.WriteString(" />\n")
}

func writeOPDSFeed(w http.ResponseWriter, feed opdsFeed) {
	updated := feed.Updated
	if updated.IsZero() {
		updated = time.Now().UTC()
	}
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<feed xmlns="http://www.w3.org/2005/Atom" xmlns:opds="http://opds-spec.org/2010/catalog" xmlns:pse="http://vaemendis.net/opds-pse/ns" xmlns:dcterms="http://purl.org/dc/terms/" xmlns:thr="http://purl.org/syndication/thread/1.0">` + "\n")
	buf.WriteString("  <id>")
	writeXMLText(&buf, feed.ID)
	buf.WriteString("</id>\n  <title>")
	writeXMLText(&buf, feed.Title)
	buf.WriteString("</title>\n  <updated>")
	buf.WriteString(updated.UTC().Format(time.RFC3339))
	buf.WriteString("</updated>\n  <author><name>MangaAlpha</name></author>\n")

	for _, l := range feed.Links {
		buf.WriteString("  <link")
		writeXMLAttr(&buf, "rel", l.Rel)
		writeXMLAttr(&buf, "type", l.Type)
		writeXMLAttr(&buf, "href", l.Href)
		writeXMLAttr(&buf, "title", l.Title)
		writeXMLAttr(&buf, "opds:facetGroup", l.FacetGroup)
		if l.ActiveFacet {
			writeXMLAttr(&buf, "opds:activeFacet", "true")
		}
		if l.ThrCount > 0 {
			writeXMLAttr(&buf, "thr:count", strconv.Itoa(l.ThrCount))
		}
		buf.WriteString(" />\n")
	}

	for _, e := range feed.Entries {
		eUpd := e.Updated
		if eUpd.IsZero() {
			eUpd = updated
		}
		buf.WriteString("  <entry>\n    <id>")
		writeXMLText(&buf, e.ID)
		buf.WriteString("</id>\n    <title>")
		writeXMLText(&buf, e.Title)
		buf.WriteString("</title>\n    <updated>")
		buf.WriteString(eUpd.UTC().Format(time.RFC3339))
		buf.WriteString("</updated>\n")
		if len(e.Authors) > 0 {
			buf.WriteString("    <author><name>")
			writeXMLText(&buf, strings.Join(e.Authors, ", "))
			buf.WriteString("</name></author>\n")
		}
		if strings.TrimSpace(e.Summary) != "" {
			buf.WriteString("    <summary>")
			writeXMLText(&buf, e.Summary)
			buf.WriteString("</summary>\n")
		}
		for _, l := range e.Links {
			writeOPDSLink(&buf, l)
		}
		buf.WriteString("  </entry>\n")
	}
	buf.WriteString("</feed>\n")

	ct := opdsMIMEFeedNav
	if feed.Acquisition {
		ct = opdsMIMEFeedAcq
	}
	w.Header().Set("Content-Type", ct)
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

func writeOpenSearchXML(w http.ResponseWriter, shortName, description, linkType, templateURL string) {
	var buf bytes.Buffer
	buf.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	buf.WriteString(`<OpenSearchDescription xmlns="http://a9.com/-/spec/opensearch/1.1/">` + "\n")
	buf.WriteString("  <ShortName>")
	writeXMLText(&buf, shortName)
	buf.WriteString("</ShortName>\n  <Description>")
	writeXMLText(&buf, description)
	buf.WriteString("</Description>\n  <Url")
	writeXMLAttr(&buf, "type", linkType)
	writeXMLAttr(&buf, "template", templateURL)
	buf.WriteString(" />\n</OpenSearchDescription>\n")

	w.Header().Set("Content-Type", opdsMIMEOpenSrch+";charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	w.Header().Set("Content-Length", strconv.Itoa(buf.Len()))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(buf.Bytes())
}

// --- Shared OPDS Helpers ---

func opdsSeriesDisplayTitle(item database.Series, meta database.SeriesMetadata) string {
	if t := strings.TrimSpace(meta.Title); t != "" {
		return t
	}
	return item.Name
}

func opdsSeriesAuthors(meta database.SeriesMetadata) []string {
	if len(meta.Authors) == 0 {
		return nil
	}
	seen := make(map[string]bool, len(meta.Authors))
	out := make([]string, 0, len(meta.Authors))
	for _, a := range meta.Authors {
		name := strings.TrimSpace(a.Name)
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func opdsChapterStatusPrefix(progress database.BookReadProgress, hasProgress bool) string {
	if hasProgress && progress.Completed {
		return "[●] "
	}
	if hasProgress && progress.Page != nil && *progress.Page > 0 {
		return "[◐] "
	}
	return "[○] "
}

func opdsBookUpdatedAt(book database.Book) time.Time {
	if book.FileModifiedAt != nil && !book.FileModifiedAt.IsZero() {
		return book.FileModifiedAt.UTC()
	}
	if !book.UpdatedAt.IsZero() {
		return book.UpdatedAt.UTC()
	}
	return time.Now().UTC()
}

func opdsSeriesUpdatedAt(item database.Series) time.Time {
	if item.FileModifiedAt != nil && !item.FileModifiedAt.IsZero() {
		return item.FileModifiedAt.UTC()
	}
	if !item.UpdatedAt.IsZero() {
		return item.UpdatedAt.UTC()
	}
	return time.Now().UTC()
}

func opdsProgressDate(progress database.BookReadProgress, hasProgress bool) string {
	if !hasProgress {
		return ""
	}
	if progress.ReadDate != nil && !progress.ReadDate.IsZero() {
		return progress.ReadDate.UTC().Format(time.RFC3339)
	}
	if !progress.UpdatedAt.IsZero() {
		return progress.UpdatedAt.UTC().Format(time.RFC3339)
	}
	return ""
}

func opdsBookSummary(book database.Book, seriesTitle string, meta database.SeriesMetadata, progress database.BookReadProgress, hasProgress bool) string {
	var parts []string
	parts = append(parts, fmt.Sprintf("%s · %s", strings.TrimPrefix(strings.ToLower(webArchiveType(book.Name)), "."), formatBytes(book.Size)))
	if book.PageCount > 0 {
		if hasProgress && progress.Completed {
			parts = append(parts, fmt.Sprintf("已读完 · 共 %d 页", book.PageCount))
		} else if hasProgress && progress.Page != nil && *progress.Page > 0 {
			parts = append(parts, fmt.Sprintf("阅读至第 %d/%d 页", min(book.PageCount, *progress.Page), book.PageCount))
		} else {
			parts = append(parts, fmt.Sprintf("共 %d 页", book.PageCount))
		}
	} else if hasProgress && progress.Completed {
		parts = append(parts, "已读完")
	} else if hasProgress && progress.Page != nil && *progress.Page > 0 {
		parts = append(parts, fmt.Sprintf("阅读至第 %d 页", *progress.Page))
	}
	header := strings.Join(parts, " · ")
	if seriesTitle != "" {
		header = seriesTitle + " — " + header
	}
	if sum := strings.TrimSpace(meta.Summary); sum != "" {
		return header + "\n\n" + sum
	}
	return header
}

func opdsBuildURL(path string, params url.Values) string {
	if len(params) == 0 {
		return path
	}
	return path + "?" + params.Encode()
}

func parsePositiveInt64(raw string) (int64, bool) {
	v, err := strconv.ParseInt(strings.TrimSpace(raw), 10, 64)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

// listPagesDetached indexes a single book on demand while detaching from short client
// timeouts (such as KOReader's 5s FEED_BLOCK_TIMEOUT). If a slow remote archive exceeds
// the client timeout, the index finishes in the background so the next tap succeeds immediately.
func (s *Server) listPagesDetached(ctx context.Context, book database.Book) ([]archive.PageEntry, error) {
	if s.archive == nil {
		return nil, archive.ErrUnsupportedArchive
	}
	type result struct {
		pages []archive.PageEntry
		err   error
	}
	ch := make(chan result, 1)
	go func() {
		bgCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), opdsIndexDetachedTimeout)
		defer cancel()
		pages, err := s.archive.ListPages(bgCtx, book)
		ch <- result{pages: pages, err: err}
	}()
	select {
	case res := <-ch:
		return res.pages, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func convertImageToJPEG(raw []byte, mimeType string) ([]byte, string) {
	if strings.EqualFold(mimeType, "image/jpeg") {
		return raw, "image/jpeg"
	}
	img, _, err := image.Decode(bytes.NewReader(raw))
	if err != nil {
		return raw, mimeType
	}
	var out bytes.Buffer
	if err := jpeg.Encode(&out, img, &jpeg.Options{Quality: 88}); err != nil {
		return raw, mimeType
	}
	return out.Bytes(), "image/jpeg"
}

// serveOPDSPageImage streams a single page for both /api/opds/v1.2 and /opds/v1.2.
// It strictly separates real page turns / close-time reports (updateProgress=true)
// from speculative prefetches (updateProgress=false) so prefetching never pollutes progress.
func (s *Server) serveOPDSPageImage(w http.ResponseWriter, r *http.Request, book database.Book, rawPageNum string, defaultZeroBased bool, requireExplicitUpdateFlag bool) {
	rawNum, err := strconv.Atoi(strings.TrimSpace(rawPageNum))
	if err != nil {
		writeError(w, http.StatusBadRequest, "INVALID_PAGE", "Invalid page number.")
		return
	}
	zeroBased := defaultZeroBased
	if zb := strings.TrimSpace(r.URL.Query().Get("zero_based")); zb != "" {
		if parsed, pErr := strconv.ParseBool(zb); pErr == nil {
			zeroBased = parsed
		}
	}
	page1 := rawNum
	if zeroBased {
		page1 = rawNum + 1
	}
	if page1 < 1 {
		writeError(w, http.StatusBadRequest, "INVALID_PAGE", "Page number out of range.")
		return
	}

	page, err := s.archive.ReadPage(r.Context(), book, page1)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Page not found.")
			return
		}
		s.archiveError(w, err)
		return
	}

	data := page.Data
	mimeType := page.Entry.MimeType
	if convert := strings.TrimSpace(r.URL.Query().Get("convert")); strings.EqualFold(convert, "jpeg") || strings.EqualFold(convert, "jpg") {
		data, mimeType = convertImageToJPEG(data, mimeType)
	}

	w.Header().Set("Content-Type", mimeType)
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("Content-Length", strconv.Itoa(len(data)))
	if _, err := w.Write(data); err != nil {
		return
	}

	rawUpd := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("updateProgress")))
	isExplicitPrefetch := rawUpd == "false" || rawUpd == "0"
	shouldRecordReadProgress := rawUpd == "true" || rawUpd == "1"
	if !requireExplicitUpdateFlag && rawUpd == "" {
		// Standard OPDS-PSE clients without updateProgress parameter still record inferred progress below,
		// while explicit updateProgress=true records full read progress.
		shouldRecordReadProgress = false
	}

	if shouldRecordReadProgress {
		s.recordOPDSReadProgress(r.Context(), book, page1, page.TotalPages)
	}
	if !isExplicitPrefetch {
		if err := s.store.RecordBookPageProgress(r.Context(), book, page1, page.TotalPages); err != nil {
			s.logger.Warn("record opds inferred page progress", "error", err, "book_id", book.ID)
		}
	}

	s.thumbs.MaybeGenerate(book, page1, page.Data)
	s.archive.Prefetch(book, page1, int(s.store.Int64Setting(r.Context(), "page_prefetch_count", 2)))
	s.archive.PrefetchNextVolumeIndex(book, page1, page.TotalPages)
}

func (s *Server) recordOPDSReadProgress(ctx context.Context, book database.Book, page1, totalPages int) {
	if page1 < 1 {
		return
	}
	completed := totalPages > 0 && page1 >= totalPages
	if !completed {
		if existing, ok, err := s.store.BookReadProgress(ctx, book.ID); err == nil && ok && existing.Completed {
			// Do not downgrade an already-completed book back to in-progress when re-reading earlier pages.
			return
		}
	}
	pageVal := page1
	if err := s.store.UpdateBookReadProgress(ctx, book, &completed, &pageVal); err != nil {
		s.logger.Warn("record opds read progress", "error", err, "book_id", book.ID)
		return
	}
	userID := database.UserIDFromContext(ctx)
	s.sse.PublishForUser(userID, "ReadProgressChanged", map[string]any{
		"bookId": book.ID,
		"userId": userID,
	})
	s.notifyBangumiTracker(ctx, book.SeriesID)
}

// ============================================================================
// Route Tree 1: /api/opds/v1.2 (Suwayomi OPDS v1.2 for KOReader opdsforcomic.koplugin)
// ============================================================================

func (s *Server) suwayomiOPDSRoutes(r chi.Router) {
	r.Get("/", s.opdsSuwayomiRoot)
	r.Get("/search", s.opdsSuwayomiSearch)
	r.Get("/history", s.opdsSuwayomiHistory)
	r.Get("/library-updates", s.opdsSuwayomiUpdates)

	r.Get("/library/series", s.opdsSuwayomiSeries)
	r.Get("/library/sources", s.opdsSuwayomiSources)
	r.Get("/library/categories", s.opdsSuwayomiCategories)
	r.Get("/library/genres", s.opdsSuwayomiGenres)
	r.Get("/library/statuses", s.opdsSuwayomiStatuses)

	r.Get("/source/{sourceID}", s.opdsSuwayomiSourceSeries)
	r.Get("/category/{categoryID}", s.opdsSuwayomiCategorySeries)
	r.Get("/genre/{genre}", s.opdsSuwayomiGenreSeries)
	r.Get("/status/{status}", s.opdsSuwayomiStatusSeries)

	r.Get("/series/{seriesID}/chapters", s.opdsSuwayomiSeriesChapters)
	r.Get("/series/{seriesID}/chapter/{chapterID}/metadata", s.opdsSuwayomiChapterMetadata)
	r.Get("/series/{seriesID}/chapter/{chapterID}/page/{pageNumber}", s.opdsSuwayomiChapterPage)
	r.Get("/series/{seriesID}/thumbnail", s.opdsSuwayomiSeriesThumbnail)
}

const opdsSuwayomiBase = "/api/opds/v1.2"

func opdsSuwayomiBaseLinks(selfHref string, acquisition bool) []opdsLink {
	selfType := opdsMIMELinkNav
	if acquisition {
		selfType = opdsMIMELinkAcq
	}
	return []opdsLink{
		{Rel: opdsRelSelf, Type: selfType, Href: selfHref},
		{Rel: opdsRelStart, Type: opdsMIMELinkNav, Href: opdsSuwayomiBase},
		{Rel: opdsRelSearch, Type: opdsMIMEOpenSrch, Href: opdsSuwayomiBase + "/search", Title: "搜索漫画"},
	}
}

func (s *Server) opdsSuwayomiRoot(w http.ResponseWriter, r *http.Request) {
	now := time.Now().UTC()
	entries := []opdsEntry{
		{
			ID:      "ma:opds:suwayomi:series",
			Title:   "全部漫画",
			Updated: now,
			Summary: "浏览全部可见书库中的漫画系列",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsSuwayomiBase + "/library/series"}},
		},
		{
			ID:      "ma:opds:suwayomi:sources",
			Title:   "按书库浏览",
			Updated: now,
			Summary: "按书库分类浏览漫画",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsSuwayomiBase + "/library/sources"}},
		},
		{
			ID:      "ma:opds:suwayomi:history",
			Title:   "阅读历史 / 继续阅读",
			Updated: now,
			Summary: "最近阅读过的漫画卷（点选直接续读并支持切换上下卷）",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsSuwayomiBase + "/history"}},
		},
		{
			ID:      "ma:opds:suwayomi:updates",
			Title:   "最近更新",
			Updated: now,
			Summary: "按最新更新时间倒序查看单卷",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsSuwayomiBase + "/library-updates"}},
		},
		{
			ID:      "ma:opds:suwayomi:categories",
			Title:   "收藏夹",
			Updated: now,
			Summary: "按收藏夹浏览漫画系列",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsSuwayomiBase + "/library/categories"}},
		},
		{
			ID:      "ma:opds:suwayomi:genres",
			Title:   "按类型筛选",
			Updated: now,
			Summary: "按 Bangumi / 元数据类型浏览",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsSuwayomiBase + "/library/genres"}},
		},
		{
			ID:      "ma:opds:suwayomi:statuses",
			Title:   "按连载状态筛选",
			Updated: now,
			Summary: "按连载中 / 已完结 / 休刊 / 弃坑筛选",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsSuwayomiBase + "/library/statuses"}},
		},
	}

	writeOPDSFeed(w, opdsFeed{
		ID:      "ma:opds:suwayomi:root",
		Title:   "MangaAlpha",
		Updated: now,
		Links:   opdsSuwayomiBaseLinks(opdsSuwayomiBase, false),
		Entries: entries,
	})
}

func (s *Server) opdsSuwayomiSearch(w http.ResponseWriter, _ *http.Request) {
	writeOpenSearchXML(
		w,
		"MangaAlpha",
		"搜索 MangaAlpha 漫画系列",
		opdsMIMELinkAcq,
		opdsSuwayomiBase+"/library/series?query={searchTerms}",
	)
}

func opdsSuwayomiPageNumber(r *http.Request) int {
	p := intQuery(r, "pageNumber", 1)
	if p < 1 {
		p = 1
	}
	return p
}

func opdsSuwayomiSeriesSortSpec(raw string) (code, dbSort string) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "title":
		return "title", "metadata.titleSort,asc"
	case "created":
		return "created", "createdDate,desc"
	case "rank":
		return "rank", "bgmRank,asc"
	default:
		return "updated", "lastModifiedDate,desc"
	}
}

func opdsSuwayomiSeriesSortFacets(basePath string, baseParams url.Values, activeSort string) []opdsLink {
	opts := []struct {
		code  string
		label string
	}{
		{"updated", "最近更新"},
		{"title", "标题 A-Z"},
		{"created", "最新入库"},
		{"rank", "Bangumi 排名"},
	}
	out := make([]opdsLink, 0, len(opts))
	for _, opt := range opts {
		p := cloneURLValues(baseParams)
		p.Set("sort", opt.code)
		p.Del("pageNumber")
		out = append(out, opdsLink{
			Rel:         opdsRelFacet,
			Type:        opdsMIMELinkAcq,
			Href:        opdsBuildURL(basePath, p),
			Title:       opt.label,
			FacetGroup:  "排序",
			ActiveFacet: opt.code == activeSort,
		})
	}
	return out
}

func cloneURLValues(in url.Values) url.Values {
	out := make(url.Values, len(in))
	for k, v := range in {
		cp := make([]string, len(v))
		copy(cp, v)
		out[k] = cp
	}
	return out
}

func (s *Server) renderSuwayomiSeriesFeed(
	w http.ResponseWriter,
	r *http.Request,
	feedID, feedTitle, basePath string,
	baseParams url.Values,
	q database.SeriesQuery,
) {
	pageNum := opdsSuwayomiPageNumber(r)
	sortCode, dbSort := opdsSuwayomiSeriesSortSpec(r.URL.Query().Get("sort"))
	if strings.TrimSpace(q.Search) != "" && strings.TrimSpace(r.URL.Query().Get("sort")) == "" {
		dbSort = "relevance"
	}
	q.Page = pageNum - 1
	q.Size = opdsDefaultSeriesPageSize
	q.Sort = dbSort

	items, total, err := s.store.SeriesPage(r.Context(), q)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	metaMap, err := s.store.SeriesMetadatas(r.Context(), items)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	ids := make([]string, len(items))
	for i, it := range items {
		ids[i] = it.ID
	}
	rowIDs, err := s.store.SeriesRowIDs(r.Context(), ids)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	progMap, err := s.store.SeriesReadProgresses(r.Context(), ids)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}

	selfParams := cloneURLValues(baseParams)
	if sortParam := strings.TrimSpace(r.URL.Query().Get("sort")); sortParam != "" {
		selfParams.Set("sort", sortCode)
	}
	if pageNum > 1 {
		selfParams.Set("pageNumber", strconv.Itoa(pageNum))
	}
	links := opdsSuwayomiBaseLinks(opdsBuildURL(basePath, selfParams), true)
	if pageNum > 1 {
		prevParams := cloneURLValues(selfParams)
		prevParams.Set("pageNumber", strconv.Itoa(pageNum-1))
		links = append(links, opdsLink{Rel: opdsRelPrev, Type: opdsMIMELinkAcq, Href: opdsBuildURL(basePath, prevParams)})
	}
	if int64(pageNum*q.Size) < total {
		nextParams := cloneURLValues(selfParams)
		nextParams.Set("pageNumber", strconv.Itoa(pageNum+1))
		links = append(links, opdsLink{Rel: opdsRelNext, Type: opdsMIMELinkAcq, Href: opdsBuildURL(basePath, nextParams)})
	}
	links = append(links, opdsSuwayomiSeriesSortFacets(basePath, baseParams, sortCode)...)

	entries := make([]opdsEntry, 0, len(items))
	for _, it := range items {
		rowID := rowIDs[it.ID]
		if rowID <= 0 {
			continue
		}
		meta := metaMap[it.ID]
		prog := progMap[it.ID]
		title := opdsSeriesDisplayTitle(it, meta)
		thumbURL := fmt.Sprintf("%s/series/%d/thumbnail", opdsSuwayomiBase, rowID)
		summary := fmt.Sprintf("共 %d 卷 · 已读 %d 卷", it.BooksCount, prog.BooksReadCount)
		if prog.BooksInProgressCount > 0 {
			summary += fmt.Sprintf(" · 阅读中 %d 卷", prog.BooksInProgressCount)
		}
		if sTxt := strings.TrimSpace(meta.Summary); sTxt != "" {
			summary += "\n\n" + sTxt
		}
		entries = append(entries, opdsEntry{
			ID:      fmt.Sprintf("ma:series:%d", rowID),
			Title:   title,
			Updated: opdsSeriesUpdatedAt(it),
			Authors: opdsSeriesAuthors(meta),
			Summary: summary,
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: fmt.Sprintf("%s/series/%d/chapters", opdsSuwayomiBase, rowID)},
				{Rel: opdsRelImage, Type: "image/jpeg", Href: thumbURL},
				{Rel: opdsRelThumbnail, Type: "image/jpeg", Href: thumbURL},
			},
		})
	}

	writeOPDSFeed(w, opdsFeed{
		ID:          feedID,
		Title:       feedTitle,
		Updated:     time.Now().UTC(),
		Acquisition: true,
		Links:       links,
		Entries:     entries,
	})
}

func (s *Server) opdsSuwayomiSeries(w http.ResponseWriter, r *http.Request) {
	query := strings.TrimSpace(r.URL.Query().Get("query"))
	if query == "" {
		query = strings.TrimSpace(r.URL.Query().Get("search"))
	}
	genre := strings.TrimSpace(r.URL.Query().Get("genre"))
	params := url.Values{}
	title := "全部漫画"
	feedID := "ma:opds:suwayomi:series"
	var genres []string
	if genre != "" {
		params.Set("genre", genre)
		genres = []string{genre}
		title = "类型：" + genre
		feedID = "ma:opds:suwayomi:genre:" + genre
	}
	if query != "" {
		params.Set("query", query)
		title = "搜索：" + query
	}
	s.renderSuwayomiSeriesFeed(
		w, r,
		feedID,
		title,
		opdsSuwayomiBase+"/library/series",
		params,
		database.SeriesQuery{Search: query, Genres: genres},
	)
}

func (s *Server) opdsSuwayomiSources(w http.ResponseWriter, r *http.Request) {
	libs, err := s.store.LibrariesBasic(r.Context(), true)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	ids := make([]string, len(libs))
	for i, l := range libs {
		ids[i] = l.ID
	}
	rowIDs, err := s.store.LibraryRowIDs(r.Context(), ids)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	now := time.Now().UTC()
	entries := make([]opdsEntry, 0, len(libs))
	for _, lib := range libs {
		rowID := rowIDs[lib.ID]
		if rowID <= 0 {
			continue
		}
		entries = append(entries, opdsEntry{
			ID:      fmt.Sprintf("ma:library:%d", rowID),
			Title:   lib.Name,
			Updated: now,
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: fmt.Sprintf("%s/source/%d", opdsSuwayomiBase, rowID)},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      "ma:opds:suwayomi:sources",
		Title:   "书库列表",
		Updated: now,
		Links:   opdsSuwayomiBaseLinks(opdsSuwayomiBase+"/library/sources", false),
		Entries: entries,
	})
}

func (s *Server) opdsSuwayomiSourceSeries(w http.ResponseWriter, r *http.Request) {
	rowID, ok := parsePositiveInt64(chi.URLParam(r, "sourceID"))
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Library not found.")
		return
	}
	lib, err := s.store.LibraryByRowID(r.Context(), rowID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Library not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	s.renderSuwayomiSeriesFeed(
		w, r,
		fmt.Sprintf("ma:opds:suwayomi:source:%d", rowID),
		lib.Name,
		fmt.Sprintf("%s/source/%d", opdsSuwayomiBase, rowID),
		url.Values{},
		database.SeriesQuery{LibraryIDs: []string{lib.ID}},
	)
}

func (s *Server) opdsSuwayomiCategories(w http.ResponseWriter, r *http.Request) {
	cols, _, err := s.store.ListCollections(r.Context(), "", nil, 0, 500, true, "name,asc")
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	ids := make([]string, len(cols))
	for i, c := range cols {
		ids[i] = c.ID
	}
	rowIDs, err := s.store.CollectionRowIDs(r.Context(), ids)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	entries := make([]opdsEntry, 0, len(cols))
	for _, c := range cols {
		rowID := rowIDs[c.ID]
		if rowID <= 0 {
			continue
		}
		entries = append(entries, opdsEntry{
			ID:      fmt.Sprintf("ma:collection:%d", rowID),
			Title:   c.Name,
			Updated: c.UpdatedAt.UTC(),
			Summary: fmt.Sprintf("包含 %d 部可见漫画", len(c.SeriesIDs)),
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: fmt.Sprintf("%s/category/%d", opdsSuwayomiBase, rowID)},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      "ma:opds:suwayomi:categories",
		Title:   "收藏夹",
		Updated: time.Now().UTC(),
		Links:   opdsSuwayomiBaseLinks(opdsSuwayomiBase+"/library/categories", false),
		Entries: entries,
	})
}

func (s *Server) opdsSuwayomiCategorySeries(w http.ResponseWriter, r *http.Request) {
	rowID, ok := parsePositiveInt64(chi.URLParam(r, "categoryID"))
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Collection not found.")
		return
	}
	col, err := s.store.CollectionByRowID(r.Context(), rowID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Collection not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	s.renderSuwayomiSeriesFeed(
		w, r,
		fmt.Sprintf("ma:opds:suwayomi:category:%d", rowID),
		col.Name,
		fmt.Sprintf("%s/category/%d", opdsSuwayomiBase, rowID),
		url.Values{},
		database.SeriesQuery{CollectionIDs: []string{col.ID}},
	)
}

func (s *Server) opdsSuwayomiGenres(w http.ResponseWriter, r *http.Request) {
	genres, err := s.store.Genres(r.Context(), nil)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	now := time.Now().UTC()
	entries := make([]opdsEntry, 0, len(genres))
	for _, g := range genres {
		entries = append(entries, opdsEntry{
			ID:      "ma:genre:" + g,
			Title:   g,
			Updated: now,
			Links: []opdsLink{
				{
					Rel:  opdsRelSubsection,
					Type: opdsMIMELinkAcq,
					Href: opdsBuildURL(opdsSuwayomiBase+"/library/series", url.Values{"genre": {g}}),
				},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      "ma:opds:suwayomi:genres",
		Title:   "按类型筛选",
		Updated: now,
		Links:   opdsSuwayomiBaseLinks(opdsSuwayomiBase+"/library/genres", false),
		Entries: entries,
	})
}

func (s *Server) opdsSuwayomiGenreSeries(w http.ResponseWriter, r *http.Request) {
	raw := chi.URLParam(r, "genre")
	genre, err := url.PathUnescape(raw)
	if err != nil || strings.TrimSpace(genre) == "" {
		genre = raw
	}
	s.renderSuwayomiSeriesFeed(
		w, r,
		"ma:opds:suwayomi:genre:"+genre,
		"类型："+genre,
		opdsSuwayomiBase+"/genre/"+url.PathEscape(genre),
		url.Values{},
		database.SeriesQuery{Genres: []string{genre}},
	)
}

var opdsStatusDefs = []struct {
	code  string
	label string
}{
	{"ONGOING", "连载中"},
	{"ENDED", "已完结"},
	{"HIATUS", "休刊"},
	{"ABANDONED", "弃坑"},
}

func (s *Server) opdsSuwayomiStatuses(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	entries := make([]opdsEntry, 0, len(opdsStatusDefs))
	for _, st := range opdsStatusDefs {
		entries = append(entries, opdsEntry{
			ID:      "ma:status:" + st.code,
			Title:   st.label,
			Updated: now,
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsSuwayomiBase + "/status/" + st.code},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      "ma:opds:suwayomi:statuses",
		Title:   "按连载状态筛选",
		Updated: now,
		Links:   opdsSuwayomiBaseLinks(opdsSuwayomiBase+"/library/statuses", false),
		Entries: entries,
	})
}

func (s *Server) opdsSuwayomiStatusSeries(w http.ResponseWriter, r *http.Request) {
	code := strings.ToUpper(strings.TrimSpace(chi.URLParam(r, "status")))
	label := code
	valid := false
	for _, st := range opdsStatusDefs {
		if st.code == code {
			label = st.label
			valid = true
			break
		}
	}
	if !valid {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Unknown status.")
		return
	}
	s.renderSuwayomiSeriesFeed(
		w, r,
		"ma:opds:suwayomi:status:"+code,
		"状态："+label,
		opdsSuwayomiBase+"/status/"+code,
		url.Values{},
		database.SeriesQuery{Statuses: []string{code}},
	)
}

func (s *Server) renderSuwayomiChapterList(
	w http.ResponseWriter,
	r *http.Request,
	feedID, feedTitle, basePath string,
	books []database.Book,
	total int64,
	pageNum, pageSize int,
	includeSeriesTitle bool,
	bookOrdinals map[string]int,
	extraLinks []opdsLink,
) {
	bookIDs := make([]string, len(books))
	seriesIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
		seriesIDs[i] = b.SeriesID
	}
	bookRows, err := s.store.BookRowIDs(r.Context(), bookIDs)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	seriesRows, err := s.store.SeriesRowIDs(r.Context(), seriesIDs)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	progMap, err := s.store.BookReadProgresses(r.Context(), bookIDs)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	seriesMap, err := s.store.SeriesByIDs(r.Context(), seriesIDs)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	seriesSlice := make([]database.Series, 0, len(seriesMap))
	for _, sr := range seriesMap {
		seriesSlice = append(seriesSlice, sr)
	}
	metaMap, err := s.store.SeriesMetadatas(r.Context(), seriesSlice)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}

	selfParams := cloneURLValues(r.URL.Query())
	if pageNum <= 1 {
		selfParams.Del("pageNumber")
	} else {
		selfParams.Set("pageNumber", strconv.Itoa(pageNum))
	}
	links := opdsSuwayomiBaseLinks(opdsBuildURL(basePath, selfParams), true)
	if pageNum > 1 {
		prevParams := cloneURLValues(selfParams)
		prevParams.Set("pageNumber", strconv.Itoa(pageNum-1))
		links = append(links, opdsLink{Rel: opdsRelPrev, Type: opdsMIMELinkAcq, Href: opdsBuildURL(basePath, prevParams)})
	}
	if int64(pageNum*pageSize) < total {
		nextParams := cloneURLValues(selfParams)
		nextParams.Set("pageNumber", strconv.Itoa(pageNum+1))
		links = append(links, opdsLink{Rel: opdsRelNext, Type: opdsMIMELinkAcq, Href: opdsBuildURL(basePath, nextParams)})
	}
	links = append(links, extraLinks...)

	entries := make([]opdsEntry, 0, len(books))
	for _, b := range books {
		sRow := seriesRows[b.SeriesID]
		bRow := bookRows[b.ID]
		if sRow <= 0 || bRow <= 0 {
			continue
		}
		prog, hasProg := progMap[b.ID]
		sr := seriesMap[b.SeriesID]
		meta := metaMap[b.SeriesID]
		sTitle := opdsSeriesDisplayTitle(sr, meta)

		prefix := opdsChapterStatusPrefix(prog, hasProg)
		entryTitle := prefix + b.Name
		if includeSeriesTitle && sTitle != "" {
			entryTitle = prefix + sTitle + ": " + b.Name
		} else if ord := bookOrdinals[b.ID]; ord > 0 {
			// Suwayomi-style leading ordinal ("1 - Vol.01.cbz") lets opdsforcomic.koplugin's
			// chapterNumberIn() identify previous/next chapters accurately in both asc & desc sorts.
			entryTitle = fmt.Sprintf("%s%d - %s", prefix, ord, b.Name)
		}
		// Notice: NO acquisition link on list entries — tapping triggers opdsforcomic.koplugin's
		// Suwayomi direct-stream fast path (/api/opds/v1.2/series/{sNum}/chapter/{bNum}/metadata)
		// with lazy single-book indexing and full prev/next chapter navigation!
		metaHref := fmt.Sprintf("%s/series/%d/chapter/%d/metadata", opdsSuwayomiBase, sRow, bRow)
		thumbHref := fmt.Sprintf("%s/series/%d/thumbnail", opdsSuwayomiBase, sRow)

		// Do NOT emit "Progress: N of M" in chapter list summaries: when /metadata omits
		// pse:lastRead on a completed chapter (to reopen from page 1), opdsforcomic's
		// suwayomiProgressFromItem() regex fallback would otherwise parse "Progress: N of N"
		// and jump straight to the last page. /metadata always supplies authoritative pse:lastRead.
		var sumParts []string
		sumParts = append(sumParts, formatBytes(b.Size))
		if b.PageCount > 0 {
			if hasProg && prog.Completed {
				sumParts = append(sumParts, fmt.Sprintf("已读完 · 共 %d 页", b.PageCount))
			} else if hasProg && prog.Page != nil && *prog.Page > 0 {
				sumParts = append(sumParts, fmt.Sprintf("阅读至第 %d/%d 页", min(b.PageCount, *prog.Page), b.PageCount))
			} else {
				sumParts = append(sumParts, fmt.Sprintf("共 %d 页", b.PageCount))
			}
		} else if hasProg && prog.Completed {
			sumParts = append(sumParts, "已读完")
		} else if hasProg && prog.Page != nil && *prog.Page > 0 {
			sumParts = append(sumParts, fmt.Sprintf("阅读至第 %d 页", *prog.Page))
		}
		updated := opdsBookUpdatedAt(b)
		if hasProg && !prog.UpdatedAt.IsZero() && includeSeriesTitle {
			updated = prog.UpdatedAt.UTC()
		}

		// Omit redundant author suffix on single-series chapter lists so e-ink rows stay concise.
		var authors []string
		if includeSeriesTitle {
			authors = opdsSeriesAuthors(meta)
		}

		entries = append(entries, opdsEntry{
			ID:      fmt.Sprintf("ma:chapter:%d:%d", sRow, bRow),
			Title:   entryTitle,
			Updated: updated,
			Authors: authors,
			Summary: strings.Join(sumParts, " · "),
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: metaHref},
				{Rel: opdsRelImage, Type: "image/jpeg", Href: thumbHref},
				{Rel: opdsRelThumbnail, Type: "image/jpeg", Href: thumbHref},
			},
		})
	}

	writeOPDSFeed(w, opdsFeed{
		ID:          feedID,
		Title:       feedTitle,
		Updated:     time.Now().UTC(),
		Acquisition: true,
		Links:       links,
		Entries:     entries,
	})
}

func (s *Server) opdsSuwayomiHistory(w http.ResponseWriter, r *http.Request) {
	pageNum := opdsSuwayomiPageNumber(r)
	size := opdsDefaultSeriesPageSize
	books, total, err := s.store.BooksPage(r.Context(), database.BookQuery{
		ReadStatus: []string{"IN_PROGRESS", "READ"},
		Page:       pageNum - 1,
		Size:       size,
		Sort:       "readProgress.readDate,desc",
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	s.renderSuwayomiChapterList(
		w, r,
		"ma:opds:suwayomi:history",
		"阅读历史",
		opdsSuwayomiBase+"/history",
		books, total, pageNum, size, true, nil, nil,
	)
}

func (s *Server) opdsSuwayomiUpdates(w http.ResponseWriter, r *http.Request) {
	pageNum := opdsSuwayomiPageNumber(r)
	size := opdsDefaultSeriesPageSize
	books, total, err := s.store.BooksPage(r.Context(), database.BookQuery{
		Page: pageNum - 1,
		Size: size,
		Sort: "lastModifiedDate,desc",
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	s.renderSuwayomiChapterList(
		w, r,
		"ma:opds:suwayomi:updates",
		"最近更新",
		opdsSuwayomiBase+"/library-updates",
		books, total, pageNum, size, true, nil, nil,
	)
}

func (s *Server) opdsSuwayomiSeriesChapters(w http.ResponseWriter, r *http.Request) {
	rowID, ok := parsePositiveInt64(chi.URLParam(r, "seriesID"))
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	series, err := s.store.SeriesByRowID(r.Context(), rowID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	if !s.store.IsLibraryVisible(r.Context(), series.LibraryID) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	meta, _ := s.store.SeriesMetadataByID(r.Context(), series)
	title := opdsSeriesDisplayTitle(series, meta)

	pageNum := opdsSuwayomiPageNumber(r)
	sortRaw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("sort")))
	dbSort := "numberSort,asc"
	sortCode := "asc"
	if sortRaw == "desc" {
		dbSort = "numberSort,desc"
		sortCode = "desc"
	}
	filterRaw := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("filter")))
	var readStatuses []string
	filterCode := "all"
	switch filterRaw {
	case "unread":
		readStatuses = []string{"UNREAD"}
		filterCode = "unread"
	case "in_progress":
		readStatuses = []string{"IN_PROGRESS"}
		filterCode = "in_progress"
	case "read":
		readStatuses = []string{"READ"}
		filterCode = "read"
	}

	books, total, err := s.store.BooksPage(r.Context(), database.BookQuery{
		SeriesID:   series.ID,
		ReadStatus: readStatuses,
		Page:       pageNum - 1,
		Size:       opdsDefaultChapterPageSize,
		Sort:       dbSort,
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	ordinals, err := s.store.SeriesBookOrdinals(r.Context(), series.ID)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}

	basePath := fmt.Sprintf("%s/series/%d/chapters", opdsSuwayomiBase, rowID)
	var facets []opdsLink
	for _, opt := range []struct{ code, label string }{{"asc", "正序（从第 1 卷起）"}, {"desc", "倒序（最新卷在前）"}} {
		p := url.Values{}
		p.Set("sort", opt.code)
		if filterCode != "all" {
			p.Set("filter", filterCode)
		}
		facets = append(facets, opdsLink{
			Rel:         opdsRelFacet,
			Type:        opdsMIMELinkAcq,
			Href:        opdsBuildURL(basePath, p),
			Title:       opt.label,
			FacetGroup:  "章节排序",
			ActiveFacet: opt.code == sortCode,
		})
	}
	for _, opt := range []struct{ code, label string }{{"all", "全部"}, {"unread", "未读"}, {"in_progress", "阅读中"}, {"read", "已读"}} {
		p := url.Values{}
		if sortCode != "asc" {
			p.Set("sort", sortCode)
		}
		if opt.code != "all" {
			p.Set("filter", opt.code)
		}
		facets = append(facets, opdsLink{
			Rel:         opdsRelFacet,
			Type:        opdsMIMELinkAcq,
			Href:        opdsBuildURL(basePath, p),
			Title:       opt.label,
			FacetGroup:  "阅读状态",
			ActiveFacet: opt.code == filterCode,
		})
	}

	s.renderSuwayomiChapterList(
		w, r,
		fmt.Sprintf("ma:opds:suwayomi:series:%d:chapters", rowID),
		title,
		basePath,
		books, total, pageNum, opdsDefaultChapterPageSize, false, ordinals, facets,
	)
}

func (s *Server) opdsSuwayomiChapterMetadata(w http.ResponseWriter, r *http.Request) {
	sRow, okS := parsePositiveInt64(chi.URLParam(r, "seriesID"))
	bRow, okB := parsePositiveInt64(chi.URLParam(r, "chapterID"))
	if !okS || !okB {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Chapter not found.")
		return
	}
	series, err := s.store.SeriesByRowID(r.Context(), sRow)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	book, err := s.store.BookByRowID(r.Context(), bRow)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	if book.SeriesID != series.ID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Chapter does not belong to series.")
		return
	}

	// On-demand lazy indexing ONLY for the single chapter the reader actually tapped.
	// Always validate against the archive's current bookVersion (instant SQLite hit when fresh;
	// automatically re-indexes if the underlying archive file was replaced).
	pages, lErr := s.listPagesDetached(r.Context(), book)
	if lErr != nil {
		s.archiveError(w, lErr)
		return
	}
	pageCount := len(pages)
	book.PageCount = pageCount
	if pageCount <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "EMPTY_ARCHIVE", "No readable image pages found in archive.")
		return
	}

	meta, _ := s.store.SeriesMetadataByID(r.Context(), series)
	prog, hasProg, _ := s.store.BookReadProgress(r.Context(), book.ID)
	sTitle := opdsSeriesDisplayTitle(series, meta)

	// opdsforcomic.koplugin expects pse:lastRead as a 0-based index (adds +1 on resume).
	// Omit pse:lastRead for unread or already-completed volumes so they open cleanly at page 1.
	lastReadZeroBased := 0
	if hasProg && !prog.Completed && prog.Page != nil && *prog.Page > 1 {
		lastReadZeroBased = min(pageCount-1, *prog.Page-1)
	}

	pageStreamHref := fmt.Sprintf("%s/series/%d/chapter/%d/page/{pageNumber}?updateProgress=true", opdsSuwayomiBase, sRow, bRow)
	thumbHref := fmt.Sprintf("%s/series/%d/thumbnail", opdsSuwayomiBase, sRow)
	selfHref := fmt.Sprintf("%s/series/%d/chapter/%d/metadata", opdsSuwayomiBase, sRow, bRow)

	streamTitle := "开始在线阅读"
	if lastReadZeroBased > 0 {
		streamTitle = fmt.Sprintf("继续在线阅读（第 %d 页）", lastReadZeroBased+1)
	}

	entry := opdsEntry{
		ID:      fmt.Sprintf("ma:chapter:%d:%d", sRow, bRow),
		Title:   opdsChapterStatusPrefix(prog, hasProg) + book.Name,
		Updated: opdsBookUpdatedAt(book),
		Authors: opdsSeriesAuthors(meta),
		Summary: opdsBookSummary(book, sTitle, meta, prog, hasProg),
		Links: []opdsLink{
			{
				Rel:          opdsRelPSEStream,
				Type:         "image/jpeg",
				Href:         pageStreamHref,
				Title:        streamTitle,
				HasPSEStream: true,
				PSECount:     pageCount,
				PSELastRead:  lastReadZeroBased,
				PSELastDate:  opdsProgressDate(prog, hasProg),
			},
			{Rel: opdsRelImage, Type: "image/jpeg", Href: thumbHref},
			{Rel: opdsRelThumbnail, Type: "image/jpeg", Href: thumbHref},
		},
	}

	writeOPDSFeed(w, opdsFeed{
		ID:          fmt.Sprintf("ma:opds:suwayomi:chapter:%d:%d:metadata", sRow, bRow),
		Title:       sTitle + " - " + book.Name,
		Updated:     opdsBookUpdatedAt(book),
		Acquisition: true,
		Links:       opdsSuwayomiBaseLinks(selfHref, true),
		Entries:     []opdsEntry{entry},
	})
}

func (s *Server) opdsSuwayomiChapterPage(w http.ResponseWriter, r *http.Request) {
	sRow, okS := parsePositiveInt64(chi.URLParam(r, "seriesID"))
	bRow, okB := parsePositiveInt64(chi.URLParam(r, "chapterID"))
	if !okS || !okB {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Chapter not found.")
		return
	}
	series, err := s.store.SeriesByRowID(r.Context(), sRow)
	if err != nil {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	book, err := s.store.BookByRowID(r.Context(), bRow)
	if err != nil || book.SeriesID != series.ID {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
		return
	}
	s.serveOPDSPageImage(w, r, book, chi.URLParam(r, "pageNumber"), true, true)
}

func (s *Server) opdsSuwayomiSeriesThumbnail(w http.ResponseWriter, r *http.Request) {
	sRow, ok := parsePositiveInt64(chi.URLParam(r, "seriesID"))
	if !ok {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	series, err := s.store.SeriesByRowID(r.Context(), sRow)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	s.serveSeriesThumbOrPlaceholder(w, r, series.ID)
}

// ============================================================================
// Route Tree 2: /opds/v1.2 (Standard Komga OPDS v1.2 + OPDS-PSE)
// ============================================================================

func (s *Server) komgaOPDSRoutes(r chi.Router) {
	r.Get("/", s.opdsKomgaCatalog)
	r.Get("/catalog", s.opdsKomgaCatalog)
	r.Get("/search", s.opdsKomgaSearch)

	r.Get("/keep-reading", s.opdsKomgaKeepReading)
	r.Get("/on-deck", s.opdsKomgaOnDeck)
	r.Get("/series", s.opdsKomgaSeriesAll)
	r.Get("/series/latest", s.opdsKomgaSeriesLatest)
	r.Get("/books/latest", s.opdsKomgaBooksLatest)

	r.Get("/libraries", s.opdsKomgaLibraries)
	r.Get("/libraries/{libraryID}", s.opdsKomgaLibrarySeries)
	r.Get("/collections", s.opdsKomgaCollections)
	r.Get("/collections/{collectionID}", s.opdsKomgaCollectionSeries)
	r.Get("/readlists", s.opdsKomgaReadLists)
	r.Get("/readlists/{readListID}", s.opdsKomgaReadListBooks)
	r.Get("/publishers", s.opdsKomgaPublishers)

	r.Get("/series/{seriesID}", s.opdsKomgaSeriesBooks)
	r.Get("/series/{seriesID}/thumbnail", s.opdsKomgaSeriesThumbnail)
	r.Get("/books/{bookID}", s.opdsKomgaBookDetail)
	r.Get("/books/{bookID}/pages/{pageNumber}", s.opdsKomgaBookPage)
	r.Get("/books/{bookID}/thumbnail", s.opdsKomgaBookThumbnail)
	r.Get("/books/{bookID}/thumbnail/small", s.opdsKomgaBookThumbnail)
}

const opdsKomgaBase = "/opds/v1.2"

func opdsKomgaBaseLinks(selfHref string, acquisition bool) []opdsLink {
	selfType := opdsMIMELinkNav
	if acquisition {
		selfType = opdsMIMELinkAcq
	}
	return []opdsLink{
		{Rel: opdsRelSelf, Type: selfType, Href: selfHref},
		{Rel: opdsRelStart, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/catalog"},
		{Rel: opdsRelSearch, Type: opdsMIMEOpenSrch, Href: opdsKomgaBase + "/search", Title: "Search"},
	}
}

func opdsKomgaPageParams(r *http.Request, defaultSize int) (page, size int) {
	page = intQuery(r, "page", 0)
	if page < 0 {
		page = 0
	}
	size = intQuery(r, "size", defaultSize)
	if size <= 0 || size > 200 {
		size = defaultSize
	}
	return page, size
}

func opdsKomgaPageLinks(r *http.Request, basePath string, baseParams url.Values, page, size int, total int64, acquisition bool) []opdsLink {
	linkType := opdsMIMELinkNav
	if acquisition {
		linkType = opdsMIMELinkAcq
	}
	params := cloneURLValues(baseParams)
	if strings.TrimSpace(r.URL.Query().Get("size")) != "" {
		params.Set("size", strconv.Itoa(size))
	}
	selfParams := cloneURLValues(params)
	if page > 0 {
		selfParams.Set("page", strconv.Itoa(page))
	}
	links := opdsKomgaBaseLinks(opdsBuildURL(basePath, selfParams), acquisition)
	if page > 0 {
		prevParams := cloneURLValues(params)
		prevParams.Set("page", strconv.Itoa(page-1))
		links = append(links, opdsLink{Rel: opdsRelPrev, Type: linkType, Href: opdsBuildURL(basePath, prevParams)})
	}
	if int64((page+1)*size) < total {
		nextParams := cloneURLValues(params)
		nextParams.Set("page", strconv.Itoa(page+1))
		links = append(links, opdsLink{Rel: opdsRelNext, Type: linkType, Href: opdsBuildURL(basePath, nextParams)})
	}
	return links
}

func (s *Server) opdsKomgaCatalog(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().UTC()
	entries := []opdsEntry{
		{
			ID:      "keep_reading",
			Title:   "Keep Reading",
			Updated: now,
			Summary: "Continue reading your in-progress books",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsKomgaBase + "/keep-reading"}},
		},
		{
			ID:      "on_deck",
			Title:   "On Deck",
			Updated: now,
			Summary: "Browse what to read next",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsKomgaBase + "/on-deck"}},
		},
		{
			ID:      "all_series",
			Title:   "All series",
			Updated: now,
			Summary: "Browse by series",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/series"}},
		},
		{
			ID:      "latest_series",
			Title:   "Latest series",
			Updated: now,
			Summary: "Browse latest series",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/series/latest"}},
		},
		{
			ID:      "latest_books",
			Title:   "Latest books",
			Updated: now,
			Summary: "Browse latest books",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsKomgaBase + "/books/latest"}},
		},
		{
			ID:      "all_libraries",
			Title:   "All libraries",
			Updated: now,
			Summary: "Browse by library",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/libraries"}},
		},
		{
			ID:      "all_collections",
			Title:   "All collections",
			Updated: now,
			Summary: "Browse by collection",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/collections"}},
		},
		{
			ID:      "all_readlists",
			Title:   "All read lists",
			Updated: now,
			Summary: "Browse by read lists",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/readlists"}},
		},
		{
			ID:      "all_publishers",
			Title:   "All publishers",
			Updated: now,
			Summary: "Browse by publishers",
			Links:   []opdsLink{{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/publishers"}},
		},
	}

	writeOPDSFeed(w, opdsFeed{
		ID:      "root",
		Title:   "MangaAlpha OPDS catalog",
		Updated: now,
		Links:   opdsKomgaBaseLinks(opdsKomgaBase+"/catalog", false),
		Entries: entries,
	})
}

func (s *Server) opdsKomgaSearch(w http.ResponseWriter, _ *http.Request) {
	writeOpenSearchXML(
		w,
		"Search",
		"Search for series",
		opdsMIMELinkNav,
		opdsKomgaBase+"/series?search={searchTerms}",
	)
}

func (s *Server) renderKomgaSeriesFeed(
	w http.ResponseWriter,
	r *http.Request,
	feedID, feedTitle, basePath string,
	baseParams url.Values,
	q database.SeriesQuery,
) {
	page, size := opdsKomgaPageParams(r, opdsDefaultSeriesPageSize)
	q.Page = page
	q.Size = size
	items, total, err := s.store.SeriesPage(r.Context(), q)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	metaMap, err := s.store.SeriesMetadatas(r.Context(), items)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}

	entries := make([]opdsEntry, 0, len(items))
	for _, it := range items {
		meta := metaMap[it.ID]
		thumbHref := opdsKomgaBase + "/series/" + url.PathEscape(it.ID) + "/thumbnail"
		entries = append(entries, opdsEntry{
			ID:      it.ID,
			Title:   opdsSeriesDisplayTitle(it, meta),
			Updated: opdsSeriesUpdatedAt(it),
			Authors: opdsSeriesAuthors(meta),
			Summary: strings.TrimSpace(meta.Summary),
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsKomgaBase + "/series/" + url.PathEscape(it.ID)},
				{Rel: opdsRelImage, Type: "image/jpeg", Href: thumbHref},
				{Rel: opdsRelThumbnail, Type: "image/jpeg", Href: thumbHref},
			},
		})
	}

	writeOPDSFeed(w, opdsFeed{
		ID:      feedID,
		Title:   feedTitle,
		Updated: time.Now().UTC(),
		Links:   opdsKomgaPageLinks(r, basePath, baseParams, page, size, total, false),
		Entries: entries,
	})
}

func (s *Server) opdsKomgaSeriesAll(w http.ResponseWriter, r *http.Request) {
	search := strings.TrimSpace(r.URL.Query().Get("search"))
	publishers := listQuery(r, "publisher")
	params := url.Values{}
	sort := "metadata.titleSort,asc"
	title := "All series"
	if search != "" {
		params.Set("search", search)
		sort = "relevance"
		title = "Series search for: " + search
	}
	for _, p := range publishers {
		params.Add("publisher", p)
		if search == "" {
			title = "Publisher: " + p
		}
	}
	s.renderKomgaSeriesFeed(
		w, r,
		"all_series", title, opdsKomgaBase+"/series", params,
		database.SeriesQuery{Search: search, Publishers: publishers, Sort: sort},
	)
}

func (s *Server) opdsKomgaSeriesLatest(w http.ResponseWriter, r *http.Request) {
	s.renderKomgaSeriesFeed(
		w, r,
		"latest_series", "Latest series", opdsKomgaBase+"/series/latest", url.Values{},
		database.SeriesQuery{Sort: "lastModifiedDate,desc"},
	)
}

func (s *Server) opdsKomgaLibraries(w http.ResponseWriter, r *http.Request) {
	libs, err := s.store.LibrariesBasic(r.Context(), true)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	now := time.Now().UTC()
	entries := make([]opdsEntry, 0, len(libs))
	for _, lib := range libs {
		entries = append(entries, opdsEntry{
			ID:      lib.ID,
			Title:   lib.Name,
			Updated: now,
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/libraries/" + url.PathEscape(lib.ID)},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      "all_libraries",
		Title:   "All libraries",
		Updated: now,
		Links:   opdsKomgaBaseLinks(opdsKomgaBase+"/libraries", false),
		Entries: entries,
	})
}

func (s *Server) opdsKomgaLibrarySeries(w http.ResponseWriter, r *http.Request) {
	libID := chi.URLParam(r, "libraryID")
	if !s.store.IsLibraryVisible(r.Context(), libID) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Library not found.")
		return
	}
	lib, err := s.store.LibraryBasic(r.Context(), libID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Library not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	s.renderKomgaSeriesFeed(
		w, r,
		lib.ID, lib.Name, opdsKomgaBase+"/libraries/"+url.PathEscape(lib.ID), url.Values{},
		database.SeriesQuery{LibraryIDs: []string{lib.ID}, Sort: "metadata.titleSort,asc"},
	)
}

func (s *Server) opdsKomgaCollections(w http.ResponseWriter, r *http.Request) {
	page, size := opdsKomgaPageParams(r, opdsDefaultSeriesPageSize)
	cols, total, err := s.store.ListCollections(r.Context(), "", nil, page, size, false, "name,asc")
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	entries := make([]opdsEntry, 0, len(cols))
	for _, c := range cols {
		entries = append(entries, opdsEntry{
			ID:      c.ID,
			Title:   c.Name,
			Updated: c.UpdatedAt.UTC(),
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsKomgaBase + "/collections/" + url.PathEscape(c.ID)},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      "all_collections",
		Title:   "All collections",
		Updated: time.Now().UTC(),
		Links:   opdsKomgaPageLinks(r, opdsKomgaBase+"/collections", url.Values{}, page, size, total, false),
		Entries: entries,
	})
}

func (s *Server) opdsKomgaCollectionSeries(w http.ResponseWriter, r *http.Request) {
	colID := chi.URLParam(r, "collectionID")
	col, err := s.store.CollectionByID(r.Context(), colID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Collection not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	page, size := opdsKomgaPageParams(r, opdsDefaultSeriesPageSize)
	items, total, err := s.store.CollectionSeriesPage(r.Context(), col.ID, database.SeriesQuery{
		Page: page,
		Size: size,
	}, false)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	metaMap, err := s.store.SeriesMetadatas(r.Context(), items)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	entries := make([]opdsEntry, 0, len(items))
	for _, it := range items {
		meta := metaMap[it.ID]
		thumbHref := opdsKomgaBase + "/series/" + url.PathEscape(it.ID) + "/thumbnail"
		entries = append(entries, opdsEntry{
			ID:      it.ID,
			Title:   opdsSeriesDisplayTitle(it, meta),
			Updated: opdsSeriesUpdatedAt(it),
			Authors: opdsSeriesAuthors(meta),
			Summary: strings.TrimSpace(meta.Summary),
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsKomgaBase + "/series/" + url.PathEscape(it.ID)},
				{Rel: opdsRelImage, Type: "image/jpeg", Href: thumbHref},
				{Rel: opdsRelThumbnail, Type: "image/jpeg", Href: thumbHref},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      col.ID,
		Title:   col.Name,
		Updated: col.UpdatedAt.UTC(),
		Links:   opdsKomgaPageLinks(r, opdsKomgaBase+"/collections/"+url.PathEscape(col.ID), url.Values{}, page, size, total, false),
		Entries: entries,
	})
}

func (s *Server) opdsKomgaReadLists(w http.ResponseWriter, r *http.Request) {
	page, size := opdsKomgaPageParams(r, opdsDefaultSeriesPageSize)
	rls, total, err := s.store.ListReadLists(r.Context(), "", nil, page, size, false, "name,asc")
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	entries := make([]opdsEntry, 0, len(rls))
	for _, rl := range rls {
		entries = append(entries, opdsEntry{
			ID:      rl.ID,
			Title:   rl.Name,
			Updated: rl.UpdatedAt.UTC(),
			Summary: rl.Summary,
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkAcq, Href: opdsKomgaBase + "/readlists/" + url.PathEscape(rl.ID)},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      "all_readlists",
		Title:   "All read lists",
		Updated: time.Now().UTC(),
		Links:   opdsKomgaPageLinks(r, opdsKomgaBase+"/readlists", url.Values{}, page, size, total, false),
		Entries: entries,
	})
}

func (s *Server) opdsKomgaPublishers(w http.ResponseWriter, r *http.Request) {
	pubs, err := s.store.Publishers(r.Context(), nil)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	now := time.Now().UTC()
	entries := make([]opdsEntry, 0, len(pubs))
	for _, pub := range pubs {
		params := url.Values{"publisher": {pub}}
		entries = append(entries, opdsEntry{
			ID:      "publisher:" + pub,
			Title:   pub,
			Updated: now,
			Links: []opdsLink{
				{Rel: opdsRelSubsection, Type: opdsMIMELinkNav, Href: opdsBuildURL(opdsKomgaBase+"/series", params)},
			},
		})
	}
	writeOPDSFeed(w, opdsFeed{
		ID:      "all_publishers",
		Title:   "All publishers",
		Updated: now,
		Links:   opdsKomgaBaseLinks(opdsKomgaBase+"/publishers", false),
		Entries: entries,
	})
}

// buildKomgaOPDSBookEntry creates an OPDS entry for a book under /opds/v1.2.
// Indexed books expose an OPDS-PSE page stream link directly (no whole-file download).
// Unindexed books expose a subsection link to /opds/v1.2/books/{bookID} so listing
// a series NEVER triggers eager archive downloads across remote 115 / WebDAV storage.
func buildKomgaOPDSBookEntry(
	book database.Book,
	seriesTitle string,
	meta database.SeriesMetadata,
	prog database.BookReadProgress,
	hasProg bool,
	includeSeriesTitle bool,
) opdsEntry {
	title := book.Name
	if includeSeriesTitle && seriesTitle != "" {
		title = seriesTitle + " - " + book.Name
	}
	thumbHref := opdsKomgaBase + "/books/" + url.PathEscape(book.ID) + "/thumbnail"
	smallThumbHref := opdsKomgaBase + "/books/" + url.PathEscape(book.ID) + "/thumbnail/small"

	var links []opdsLink
	if book.PageCount > 0 {
		// Standard Komga OPDS-PSE uses 1-based pse:lastRead (only when in-progress).
		lastRead := 0
		if hasProg && !prog.Completed && prog.Page != nil && *prog.Page > 0 {
			lastRead = min(book.PageCount, *prog.Page)
		}
		links = append(links, opdsLink{
			Rel:          opdsRelPSEStream,
			Type:         "image/jpeg",
			Href:         opdsKomgaBase + "/books/" + url.PathEscape(book.ID) + "/pages/{pageNumber}?zero_based=true&updateProgress=true",
			Title:        "Page stream",
			HasPSEStream: true,
			PSECount:     book.PageCount,
			PSELastRead:  lastRead,
			PSELastDate:  opdsProgressDate(prog, hasProg),
		})
	} else {
		links = append(links, opdsLink{
			Rel:  opdsRelSubsection,
			Type: opdsMIMELinkAcq,
			Href: opdsKomgaBase + "/books/" + url.PathEscape(book.ID),
		})
	}
	links = append(links,
		opdsLink{Rel: opdsRelThumbnail, Type: "image/jpeg", Href: smallThumbHref},
		opdsLink{Rel: opdsRelImage, Type: "image/jpeg", Href: thumbHref},
	)

	return opdsEntry{
		ID:      book.ID,
		Title:   title,
		Updated: opdsBookUpdatedAt(book),
		Authors: opdsSeriesAuthors(meta),
		Summary: opdsBookSummary(book, seriesTitle, meta, prog, hasProg),
		Links:   links,
	}
}

func (s *Server) renderKomgaBookEntries(
	w http.ResponseWriter,
	r *http.Request,
	feedID, feedTitle, basePath string,
	books []database.Book,
	total int64,
	page, size int,
	includeSeriesTitle bool,
) {
	bookIDs := make([]string, len(books))
	seriesIDs := make([]string, len(books))
	for i, b := range books {
		bookIDs[i] = b.ID
		seriesIDs[i] = b.SeriesID
	}
	progMap, err := s.store.BookReadProgresses(r.Context(), bookIDs)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	seriesMap, err := s.store.SeriesByIDs(r.Context(), seriesIDs)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	seriesSlice := make([]database.Series, 0, len(seriesMap))
	for _, sr := range seriesMap {
		seriesSlice = append(seriesSlice, sr)
	}
	metaMap, err := s.store.SeriesMetadatas(r.Context(), seriesSlice)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}

	entries := make([]opdsEntry, 0, len(books))
	for _, b := range books {
		sr := seriesMap[b.SeriesID]
		meta := metaMap[b.SeriesID]
		prog, hasProg := progMap[b.ID]
		entries = append(entries, buildKomgaOPDSBookEntry(
			b, opdsSeriesDisplayTitle(sr, meta), meta, prog, hasProg, includeSeriesTitle,
		))
	}

	writeOPDSFeed(w, opdsFeed{
		ID:          feedID,
		Title:       feedTitle,
		Updated:     time.Now().UTC(),
		Acquisition: true,
		Links:       opdsKomgaPageLinks(r, basePath, url.Values{}, page, size, total, true),
		Entries:     entries,
	})
}

func (s *Server) opdsKomgaKeepReading(w http.ResponseWriter, r *http.Request) {
	page, size := opdsKomgaPageParams(r, opdsDefaultSeriesPageSize)
	books, total, err := s.store.BooksPage(r.Context(), database.BookQuery{
		ReadStatus: []string{"IN_PROGRESS"},
		Page:       page,
		Size:       size,
		Sort:       "readProgress.readDate,desc",
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	s.renderKomgaBookEntries(w, r, "keep_reading", "Keep Reading", opdsKomgaBase+"/keep-reading", books, total, page, size, true)
}

func (s *Server) opdsKomgaOnDeck(w http.ResponseWriter, r *http.Request) {
	page, size := opdsKomgaPageParams(r, opdsDefaultSeriesPageSize)
	books, total, err := s.store.OnDeckBooksPage(r.Context(), page, size)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	s.renderKomgaBookEntries(w, r, "on_deck", "On Deck", opdsKomgaBase+"/on-deck", books, total, page, size, true)
}

func (s *Server) opdsKomgaBooksLatest(w http.ResponseWriter, r *http.Request) {
	page, size := opdsKomgaPageParams(r, opdsDefaultSeriesPageSize)
	books, total, err := s.store.BooksPage(r.Context(), database.BookQuery{
		Page: page,
		Size: size,
		Sort: "lastModifiedDate,desc",
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	s.renderKomgaBookEntries(w, r, "latest_books", "Latest books", opdsKomgaBase+"/books/latest", books, total, page, size, true)
}

func (s *Server) opdsKomgaReadListBooks(w http.ResponseWriter, r *http.Request) {
	rlID := chi.URLParam(r, "readListID")
	rl, err := s.store.ReadListByID(r.Context(), rlID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "ReadList not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	page, size := opdsKomgaPageParams(r, opdsDefaultSeriesPageSize)
	books, total, err := s.store.ReadListBooksPage(r.Context(), rl.ID, database.BookQuery{
		Page: page,
		Size: size,
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	s.renderKomgaBookEntries(w, r, rl.ID, rl.Name, opdsKomgaBase+"/readlists/"+url.PathEscape(rl.ID), books, total, page, size, true)
}

func (s *Server) opdsKomgaSeriesBooks(w http.ResponseWriter, r *http.Request) {
	series, err := s.store.SeriesByID(r.Context(), chi.URLParam(r, "seriesID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	if !s.store.IsLibraryVisible(r.Context(), series.LibraryID) {
		writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
		return
	}
	meta, _ := s.store.SeriesMetadataByID(r.Context(), series)
	page, size := opdsKomgaPageParams(r, opdsDefaultChapterPageSize)
	books, total, err := s.store.BooksPage(r.Context(), database.BookQuery{
		SeriesID: series.ID,
		Page:     page,
		Size:     size,
		Sort:     "numberSort,asc",
	})
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	s.renderKomgaBookEntries(
		w, r,
		series.ID, opdsSeriesDisplayTitle(series, meta), opdsKomgaBase+"/series/"+url.PathEscape(series.ID),
		books, total, page, size, false,
	)
}

func (s *Server) opdsKomgaBookDetail(w http.ResponseWriter, r *http.Request) {
	book, err := s.store.BookByID(r.Context(), chi.URLParam(r, "bookID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	series, err := s.store.SeriesByID(r.Context(), book.SeriesID)
	if err != nil {
		s.writeDBError(w, r, err)
		return
	}
	// Always validate against the archive's current bookVersion (instant SQLite hit when fresh;
	// automatically re-indexes if the underlying archive file was replaced).
	pages, lErr := s.listPagesDetached(r.Context(), book)
	if lErr != nil {
		s.archiveError(w, lErr)
		return
	}
	book.PageCount = len(pages)
	if book.PageCount <= 0 {
		writeError(w, http.StatusUnprocessableEntity, "EMPTY_ARCHIVE", "No readable image pages found in archive.")
		return
	}
	meta, _ := s.store.SeriesMetadataByID(r.Context(), series)
	prog, hasProg, _ := s.store.BookReadProgress(r.Context(), book.ID)
	entry := buildKomgaOPDSBookEntry(book, opdsSeriesDisplayTitle(series, meta), meta, prog, hasProg, false)

	writeOPDSFeed(w, opdsFeed{
		ID:          "book:" + book.ID,
		Title:       book.Name,
		Updated:     opdsBookUpdatedAt(book),
		Acquisition: true,
		Links:       opdsKomgaBaseLinks(opdsKomgaBase+"/books/"+url.PathEscape(book.ID), true),
		Entries:     []opdsEntry{entry},
	})
}

func (s *Server) opdsKomgaBookPage(w http.ResponseWriter, r *http.Request) {
	book, err := s.store.BookByID(r.Context(), chi.URLParam(r, "bookID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	s.serveOPDSPageImage(w, r, book, chi.URLParam(r, "pageNumber"), true, false)
}

func (s *Server) opdsKomgaSeriesThumbnail(w http.ResponseWriter, r *http.Request) {
	series, err := s.store.SeriesByID(r.Context(), chi.URLParam(r, "seriesID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Series not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	s.serveSeriesThumbOrPlaceholder(w, r, series.ID)
}

func (s *Server) opdsKomgaBookThumbnail(w http.ResponseWriter, r *http.Request) {
	book, err := s.store.BookByID(r.Context(), chi.URLParam(r, "bookID"))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
			return
		}
		s.writeDBError(w, r, err)
		return
	}
	if s.thumbs != nil {
		data, found, err := s.thumbs.GetBookCover(r.Context(), book.ID)
		if errors.Is(err, sql.ErrNoRows) {
			writeError(w, http.StatusNotFound, "NOT_FOUND", "Book not found.")
			return
		}
		if err == nil && found {
			writeBookCover(w, data)
			return
		}
	}
	s.serveSeriesThumbOrPlaceholder(w, r, book.SeriesID)
}
