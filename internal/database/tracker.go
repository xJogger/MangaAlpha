package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"
)

type BangumiTrackerRecord struct {
	BgmSubjectID    int64   `json:"bgmSubjectId"`
	SeriesID        string  `json:"seriesId"`
	SeriesTitle     string  `json:"seriesTitle"`
	Status          string  `json:"status"` // synced | skipped | failed | pending
	RemoteType      int     `json:"remoteType"`
	RemoteVolStatus int     `json:"remoteVolStatus"`
	RemotePrivate   bool    `json:"remotePrivate"`
	LastLocalVol    int     `json:"lastLocalVol"`
	LastError       string  `json:"lastError,omitempty"`
	LastSyncedAt    *string `json:"lastSyncedAt,omitempty"`
	UpdatedAt       string  `json:"updatedAt"`
}

type BangumiTrackerSeriesSnapshot struct {
	SeriesID            string
	SeriesName          string
	SeriesTitle         string
	OneShot             bool
	BgmSubjectID        int64
	SeriesStatus        string
	TotalBookCount      *int
	IsNSFW              bool
	TotalBooks          int
	CompletedCount      int
	HasAnyProgress      bool
	MaxCompletedOrdinal int
	AllCompleted        bool
}

func (s *Store) UpsertBangumiTrackerRecord(ctx context.Context, rec BangumiTrackerRecord) error {
	if rec.BgmSubjectID <= 0 {
		return nil
	}
	userID := UserIDFromContext(ctx)
	now := nowText()
	if strings.TrimSpace(rec.UpdatedAt) == "" {
		rec.UpdatedAt = now
	}
	privateInt := 0
	if rec.RemotePrivate {
		privateInt = 1
	}
	_, err := s.db.ExecContext(ctx, `
INSERT INTO bangumi_tracker_records(
 user_id,bgm_subject_id,series_id,series_title,status,remote_type,remote_vol_status,
 remote_private,last_local_vol,last_error,last_synced_at,updated_at
) VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
ON CONFLICT(user_id,bgm_subject_id) DO UPDATE SET
 series_id=excluded.series_id,
 series_title=excluded.series_title,
 status=excluded.status,
 remote_type=excluded.remote_type,
 remote_vol_status=excluded.remote_vol_status,
 remote_private=excluded.remote_private,
 last_local_vol=excluded.last_local_vol,
 last_error=excluded.last_error,
 last_synced_at=coalesce(excluded.last_synced_at, bangumi_tracker_records.last_synced_at),
 updated_at=excluded.updated_at`,
		userID, rec.BgmSubjectID, strings.TrimSpace(rec.SeriesID), strings.TrimSpace(rec.SeriesTitle),
		strings.TrimSpace(rec.Status), rec.RemoteType, rec.RemoteVolStatus,
		privateInt, rec.LastLocalVol, strings.TrimSpace(rec.LastError),
		rec.LastSyncedAt, rec.UpdatedAt,
	)
	return err
}

func (s *Store) BangumiTrackerRecordBySubject(ctx context.Context, subjectID int64) (BangumiTrackerRecord, bool, error) {
	if subjectID <= 0 {
		return BangumiTrackerRecord{}, false, nil
	}
	userID := UserIDFromContext(ctx)
	var (
		rec        BangumiTrackerRecord
		privateInt int
		syncedAt   sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
SELECT bgm_subject_id,series_id,series_title,status,remote_type,remote_vol_status,
 remote_private,last_local_vol,last_error,last_synced_at,updated_at
FROM bangumi_tracker_records WHERE user_id=? AND bgm_subject_id=?`, userID, subjectID).Scan(
		&rec.BgmSubjectID, &rec.SeriesID, &rec.SeriesTitle, &rec.Status,
		&rec.RemoteType, &rec.RemoteVolStatus, &privateInt, &rec.LastLocalVol,
		&rec.LastError, &syncedAt, &rec.UpdatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return BangumiTrackerRecord{}, false, nil
	}
	if err != nil {
		return BangumiTrackerRecord{}, false, err
	}
	rec.RemotePrivate = privateInt != 0
	if syncedAt.Valid && strings.TrimSpace(syncedAt.String) != "" {
		v := syncedAt.String
		rec.LastSyncedAt = &v
	}
	return rec, true, nil
}

func (s *Store) ListBangumiTrackerRecords(ctx context.Context, limit int) ([]BangumiTrackerRecord, error) {
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	userID := UserIDFromContext(ctx)
	rows, err := s.db.QueryContext(ctx, `
SELECT bgm_subject_id,series_id,series_title,status,remote_type,remote_vol_status,
 remote_private,last_local_vol,last_error,last_synced_at,updated_at
FROM bangumi_tracker_records
WHERE user_id=?
ORDER BY updated_at DESC
LIMIT ?`, userID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]BangumiTrackerRecord, 0)
	for rows.Next() {
		var (
			rec        BangumiTrackerRecord
			privateInt int
			syncedAt   sql.NullString
		)
		if err := rows.Scan(
			&rec.BgmSubjectID, &rec.SeriesID, &rec.SeriesTitle, &rec.Status,
			&rec.RemoteType, &rec.RemoteVolStatus, &privateInt, &rec.LastLocalVol,
			&rec.LastError, &syncedAt, &rec.UpdatedAt,
		); err != nil {
			return nil, err
		}
		rec.RemotePrivate = privateInt != 0
		if syncedAt.Valid && strings.TrimSpace(syncedAt.String) != "" {
			v := syncedAt.String
			rec.LastSyncedAt = &v
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

func (s *Store) BangumiTrackerSeriesSnapshot(ctx context.Context, seriesID string) (BangumiTrackerSeriesSnapshot, bool, error) {
	series, err := s.SeriesByIDAny(ctx, seriesID, false)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return BangumiTrackerSeriesSnapshot{}, false, nil
		}
		return BangumiTrackerSeriesSnapshot{}, false, err
	}
	meta, err := s.SeriesMetadataByID(ctx, series)
	if err != nil {
		return BangumiTrackerSeriesSnapshot{}, false, err
	}
	var bgmID int64
	if meta.BgmSubjectID != nil && *meta.BgmSubjectID > 0 {
		bgmID = *meta.BgmSubjectID
	}
	displayTitle := strings.TrimSpace(meta.Title)
	if displayTitle == "" {
		displayTitle = series.Name
	}
	snap := BangumiTrackerSeriesSnapshot{
		SeriesID:       series.ID,
		SeriesName:     series.Name,
		SeriesTitle:    displayTitle,
		OneShot:        series.EffectiveOneShot(),
		BgmSubjectID:   bgmID,
		SeriesStatus:   strings.TrimSpace(meta.Status),
		TotalBookCount: meta.TotalBookCount,
		IsNSFW:         meta.IsNSFW,
	}
	userID := UserIDFromContext(ctx)
	rows, err := s.db.QueryContext(ctx, `
SELECT CASE WHEN p.book_id IS NULL THEN 0 ELSE 1 END AS has_progress,
 coalesce(p.completed,0), coalesce(p.page,0)
FROM books b
LEFT JOIN book_read_progress p ON p.user_id=? AND p.book_id=b.id
WHERE b.series_id=?
ORDER BY b.effective_number_sort ASC,b.name COLLATE NOCASE ASC,b.id ASC`, userID, series.ID)
	if err != nil {
		return BangumiTrackerSeriesSnapshot{}, false, err
	}
	defer rows.Close()

	ordinal := 0
	for rows.Next() {
		var hasProg, completed, page int
		if err := rows.Scan(&hasProg, &completed, &page); err != nil {
			return BangumiTrackerSeriesSnapshot{}, false, err
		}
		ordinal++
		snap.TotalBooks++
		if hasProg != 0 && (completed != 0 || page > 0) {
			snap.HasAnyProgress = true
		}
		if hasProg != 0 && completed != 0 {
			snap.CompletedCount++
			snap.MaxCompletedOrdinal = ordinal
		}
	}
	if err := rows.Err(); err != nil {
		return BangumiTrackerSeriesSnapshot{}, false, err
	}
	snap.AllCompleted = snap.TotalBooks > 0 && snap.CompletedCount == snap.TotalBooks
	return snap, true, nil
}
