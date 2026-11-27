package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"fmt"
)

type LibraryMetadataChainState struct {
	LibraryID           string `json:"libraryId"`
	ChainVersion        string `json:"chainVersion"`
	AppliedChainVersion string `json:"appliedChainVersion"`
	NeedsRematch        bool   `json:"needsRematch"`
}

const metadataChainNeedsRematchSQL = "coalesce(mcs.chain_version<>'' AND mcs.chain_version<>mcs.applied_chain_version,0)"

func scanLibraryMetadataChainState(row interface{ Scan(...any) error }) (LibraryMetadataChainState, error) {
	var state LibraryMetadataChainState
	err := row.Scan(&state.LibraryID, &state.ChainVersion, &state.AppliedChainVersion)
	state.NeedsRematch = state.ChainVersion != "" && state.ChainVersion != state.AppliedChainVersion
	return state, err
}

func (s *Store) GetLibraryMetadataChainState(ctx context.Context, libraryID string) (LibraryMetadataChainState, error) {
	return scanLibraryMetadataChainState(s.db.QueryRowContext(ctx, `
SELECT l.id,coalesce(mcs.chain_version,''),coalesce(mcs.applied_chain_version,'')
FROM libraries l LEFT JOIN library_metadata_chain_state mcs ON mcs.library_id=l.id WHERE l.id=?`, libraryID))
}

func (s *Store) ListAllLibraryMetadataChainStates(ctx context.Context) (map[string]LibraryMetadataChainState, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT library_id,chain_version,applied_chain_version FROM library_metadata_chain_state`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]LibraryMetadataChainState)
	for rows.Next() {
		state, err := scanLibraryMetadataChainState(rows)
		if err != nil {
			return nil, err
		}
		out[state.LibraryID] = state
	}
	return out, rows.Err()
}

type metadataExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

const upsertMetadataChainVersionSQL = `
ON CONFLICT(library_id) DO UPDATE SET chain_version=excluded.chain_version,updated_at=excluded.updated_at
WHERE library_metadata_chain_state.chain_version<>excluded.chain_version`

func (s *Store) MarkLibraryMetadataChainChanged(ctx context.Context, libraryID, newVersion string) error {
	return markLibraryMetadataChainChanged(ctx, s.db, libraryID, newVersion)
}

func markLibraryMetadataChainChanged(ctx context.Context, db metadataExecer, libraryID, newVersion string) error {
	_, err := db.ExecContext(ctx, `
INSERT INTO library_metadata_chain_state(library_id,chain_version,updated_at) SELECT id,?,? FROM libraries WHERE id=?
`+upsertMetadataChainVersionSQL, newVersion, nowText(), libraryID)
	return err
}

func (s *Store) MarkLibrariesUsingSourceChainChanged(ctx context.Context, sourceID, reasonToken string) error {
	return markLibrariesUsingSourceChainChanged(ctx, s.db, "= ?", sourceID, reasonToken)
}

// sourceFilter is a fixed SQL fragment selecting one source or a repository's sources.
func markLibrariesUsingSourceChainChanged(ctx context.Context, db metadataExecer, sourceFilter, id, reasonToken string) error {
	nonce, err := randomHexID(16)
	if err != nil {
		return err
	}
	now := nowText()
	version := fmt.Sprintf("%x", sha256.Sum256([]byte(nonce+"\x00"+id+"\x00"+reasonToken)))
	_, err = db.ExecContext(ctx, `
INSERT INTO library_metadata_chain_state(library_id,chain_version,updated_at)
SELECT l.id,?,? FROM libraries l
WHERE NOT EXISTS (SELECT 1 FROM union_layers ul WHERE ul.member_library_id=l.id)
AND (NOT EXISTS (SELECT 1 FROM library_metadata_sources lc WHERE lc.library_id=l.id)
 OR EXISTS (SELECT 1 FROM library_metadata_sources lc WHERE lc.library_id=l.id AND lc.source_id `+sourceFilter+`))
`+upsertMetadataChainVersionSQL, version, now, id)
	return err
}

func (s *Store) MarkLibraryMetadataChainApplied(ctx context.Context, libraryID, appliedVersion string) error {
	_, err := s.db.ExecContext(ctx, `
UPDATE library_metadata_chain_state SET applied_chain_version=?,updated_at=?
WHERE library_id=? AND chain_version=? AND applied_chain_version<>?`,
		appliedVersion, nowText(), libraryID, appliedVersion, appliedVersion)
	return err
}

type metadataChainSourceVersion struct {
	enabled, ready, importCovers                     bool
	digest, etag, remoteUpdated, lastUpdated, schema string
}

func metadataSourceChainVersion(ctx context.Context, tx *sql.Tx, id string) (metadataChainSourceVersion, error) {
	var version metadataChainSourceVersion
	err := tx.QueryRowContext(ctx, `
SELECT enabled,ready,import_covers,local_sha256,remote_etag,remote_updated_at,last_updated_at,schema_type
FROM metadata_sources WHERE id=?`, id).Scan(
		&version.enabled, &version.ready, &version.importCovers, &version.digest,
		&version.etag, &version.remoteUpdated, &version.lastUpdated, &version.schema,
	)
	return version, err
}

func markMetadataSourceVersionChange(ctx context.Context, tx *sql.Tx, id string, before metadataChainSourceVersion, newSource bool) error {
	after, err := metadataSourceChainVersion(ctx, tx, id)
	if err != nil {
		return err
	}
	if before != after && (!newSource || after.enabled) {
		return markLibrariesUsingSourceChainChanged(ctx, tx, "= ?", id, "source configuration or version")
	}
	return nil
}
