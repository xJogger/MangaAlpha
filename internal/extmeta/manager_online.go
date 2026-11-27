package extmeta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/xJogger/fake-komga-115/internal/bangumi"
	"github.com/xJogger/fake-komga-115/internal/cjk"
	"github.com/xJogger/fake-komga-115/internal/database"
)

type OnlineStatus struct {
	RunID         string     `json:"runId,omitempty"`
	Status        string     `json:"status"`
	Total         int        `json:"total"`
	Processed     int        `json:"processed"`
	Running       bool       `json:"running"`
	CircuitOpen   bool       `json:"circuitOpen"`
	Matched       int        `json:"matched"`
	Failed        int        `json:"failed"`
	LastError     string     `json:"lastError,omitempty"`
	SourceID      string     `json:"sourceId,omitempty"`
	HTTPStatus    int        `json:"httpStatus,omitempty"`
	CooldownUntil *time.Time `json:"cooldownUntil,omitempty"`
}

func (m *Manager) OnlineEnrichStatus() OnlineStatus {
	m.mu.RLock()
	defer m.mu.RUnlock()
	status := m.online
	if status.Status == "" {
		status.Status = "idle"
	}
	if status.CircuitOpen && status.CooldownUntil != nil && !time.Now().Before(*status.CooldownUntil) {
		status.CircuitOpen, status.CooldownUntil = false, nil
	}
	return status
}

func (m *Manager) setOnlineStatus(status OnlineStatus) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.online = status
}

func (m *Manager) enrichOnline(ctx context.Context, run *ScrapeRun, sources []database.MetadataSource, pending []scrapeSeries, skippedLibraries map[string]bool) error {
	if len(pending) == 0 {
		return nil
	}
	client := m.client(ctx, 30*time.Second)
	defer client.CloseIdleConnections()
	status := OnlineStatus{RunID: run.ID, Status: "running", Running: true, Total: len(pending)}
	if previous := m.OnlineEnrichStatus(); previous.CircuitOpen {
		status.Status, status.CircuitOpen = "cooldown", true
		status.SourceID, status.HTTPStatus = previous.SourceID, previous.HTTPStatus
		status.CooldownUntil, status.LastError = previous.CooldownUntil, previous.LastError
	}
	m.setOnlineStatus(status)
	defer func() {
		status.Status = "success"
		status.Running = false
		if ctx.Err() != nil {
			status.Status = "canceled"
		}
		m.setOnlineStatus(status)
	}()
	// Keep the current series and source on a circuit trip. The same request
	// resumes after cooldown, and completed items remain committed.
	for _, target := range pending {
		run.CurrentSeries = target.series.Name
		m.publishScrapeProgress(run)
		parsed := bangumi.ParseSeriesName(target.series.Name)
		query := firstNonEmpty(parsed.Candidates...)
		var (
			meta   database.ScrapedMetadata
			found  bool
			failed error
		)
		if query != "" && !containsSensitiveOrPrivateValue(target.series.Name) && !containsSensitiveOrPrivateValue(query) {
			for _, source := range sources {
				var err error
				meta, found, err = m.onlineMatch(ctx, client, source.ID, target, query, &status)
				if found || errors.Is(err, database.ErrMetadataChanged) {
					failed = err
					break
				}
				if err != nil {
					failed = err
				}
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		err := m.finishSeries(ctx, run, target, meta, found, failed, skippedLibraries)
		status.Processed++
		switch {
		case errors.Is(err, database.ErrMetadataChanged):
		case err != nil:
			status.Failed++
			if failed == nil || status.LastError == "" {
				status.LastError = "online metadata enrichment failed"
			}
		case found:
			status.Matched++
		}
		m.setOnlineStatus(status)
	}
	return nil
}

func (m *Manager) onlineMatch(ctx context.Context, client *http.Client, sourceID string, target scrapeSeries, query string, status *OnlineStatus) (database.ScrapedMetadata, bool, error) {
	var empty database.ScrapedMetadata
	for trips := 0; ; trips++ {
		if err := waitUntil(ctx, m.nextOnline); err != nil {
			return empty, false, err
		}
		status.Status, status.SourceID = "running", sourceID
		status.HTTPStatus, status.CooldownUntil, status.CircuitOpen = 0, nil, false
		m.setOnlineStatus(*status)
		series, err := m.store.SeriesByIDAny(ctx, target.series.ID, false)
		if err != nil {
			return empty, false, database.ErrMetadataChanged
		}
		current, err := m.store.SeriesMetadataByID(ctx, series)
		if err != nil {
			return empty, false, err
		}
		if current.MatchOrigin == "manual" || current.MatchOrigin == "agent" ||
			(current.MatchOrigin == "" && (current.ExtEntryID != "" || current.BgmSubjectID != nil && *current.BgmSubjectID > 0)) {
			return empty, false, database.ErrMetadataChanged
		}
		source, err := m.store.GetMetadataSource(ctx, sourceID)
		if err != nil || !source.Enabled || !source.OnlineEnabled || source.OnlineAPIURL == "" {
			return empty, false, database.ErrMetadataChanged
		}
		entry, code, retry, err := requestOnlineEntry(ctx, client, source.OnlineAPIURL, query)
		m.nextOnline = time.Now().Add(5*time.Second + rand.N(3*time.Second+1))
		status.HTTPStatus = code
		if err != nil && ctx.Err() == nil {
			status.LastError = "online metadata request failed"
			if code != 0 {
				status.LastError += " (HTTP " + strconv.Itoa(code) + ")"
			}
		}
		if isCircuitBreakerStatus(code) {
			delay := max(retry, time.Minute<<min(trips, 5))
			until := time.Now().Add(delay)
			m.nextOnline = until
			status.Status, status.CircuitOpen, status.CooldownUntil = "cooldown", true, &until
			m.setOnlineStatus(*status)
			continue
		}
		if entry == nil || err != nil {
			return empty, false, err
		}
		if source.SchemaType == SchemaGalleryV1 || source.SchemaType == "" || source.SchemaType == "auto" {
			m.mu.RLock()
			sidecar := m.sidecars[sourceID]
			isGallery := source.SchemaType == SchemaGalleryV1 || sidecar != nil && sidecar.Schema() == SchemaGalleryV1
			m.mu.RUnlock()
			if isGallery {
				entry.IsNSFW = IsGalleryCategoryNSFW(entry.Category)
			}
		}
		meta := m.externalMetadata(ctx, source, entry, "auto", 1)
		_, meta.ExtCoverURL = offlineCoverRef("", entry.CoverURL)
		return meta, true, nil
	}
}

func waitUntil(ctx context.Context, until time.Time) error {
	if delay := time.Until(until); delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	return ctx.Err()
}

// Online sources accept a cleaned title as the q query parameter and return a
// normalized Entry, an array of entries, or an object with an entries array.
func requestOnlineEntry(ctx context.Context, client *http.Client, rawURL, query string) (*Entry, int, time.Duration, error) {
	u, err := validateHTTPURL(rawURL)
	if err != nil || u.User != nil {
		return nil, 0, 0, ErrInvalidConfig
	}
	values := u.Query()
	values.Set("q", query)
	u.RawQuery = values.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, 0, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", repoUserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, err
	}
	defer resp.Body.Close()
	code := resp.StatusCode
	if code == http.StatusNotFound || code == http.StatusNoContent {
		return nil, code, 0, nil
	}
	if code != http.StatusOK {
		var retry time.Duration
		if seconds, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && seconds > 0 {
			retry = time.Duration(min(seconds, 86400)) * time.Second
		} else if until, err := http.ParseTime(resp.Header.Get("Retry-After")); err == nil {
			retry = min(time.Until(until), 24*time.Hour)
		}
		return nil, code, retry, errors.New("online metadata request failed")
	}
	const maxResponse = 1 << 20
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
	if err != nil {
		return nil, code, 0, err
	}
	if len(data) > maxResponse {
		return nil, code, 0, errors.New("online metadata response exceeds size limit")
	}
	var payload struct {
		Entry
		Entries []Entry `json:"entries"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		if err := json.Unmarshal(data, &payload.Entries); err != nil {
			return nil, code, 0, err
		}
	}
	key := cjk.ExactTitleKey(query)
	candidates := append(slices.Clone(payload.Entries), payload.Entry)
	var matched *Entry
	for _, entry := range candidates {
		if strings.TrimSpace(entry.EntryID) == "" || key == "" ||
			(key != cjk.ExactTitleKey(entry.Title) && key != cjk.ExactTitleKey(entry.TitleAlt)) {
			continue
		}
		if matched != nil && matched.EntryID != entry.EntryID {
			return nil, code, 0, nil
		}
		matched = &entry
	}
	return matched, code, 0, nil
}
