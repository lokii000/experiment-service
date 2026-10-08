package storage

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/lokii000/experiment-service/internal/config"
)

var ErrRevisionConflict = errors.New("configuration revision conflict")
var ErrInvalidConfiguration = errors.New("invalid configuration")

type ConfigRepository struct{ DB *sql.DB }

func (r *ConfigRepository) Migrate(ctx context.Context) error {
	tx, err := r.DB.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	// Serialize startup migrations across replicas. For larger deployments,
	// use a dedicated migration job rather than migrating on every startup.
	if _, err = tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(628299771)`); err != nil {
		return err
	}
	for _, statement := range strings.Split(schema, ";") {
		statement = strings.TrimSpace(statement)
		if statement == "" {
			continue
		}
		if _, err = tx.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return tx.Commit()
}

const schema = `
CREATE TABLE IF NOT EXISTS config_snapshots (
 revision BIGINT PRIMARY KEY, document JSONB NOT NULL,
 published_at TIMESTAMPTZ NOT NULL DEFAULT now());
CREATE TABLE IF NOT EXISTS config_current (
 singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK(singleton),
 revision BIGINT REFERENCES config_snapshots(revision));
INSERT INTO config_current(singleton,revision) VALUES(TRUE,NULL)
 ON CONFLICT(singleton) DO NOTHING;
CREATE TABLE IF NOT EXISTS exposures (
 event_id TEXT PRIMARY KEY, project_id TEXT NOT NULL,
 assignment_id TEXT NOT NULL, experiment_key TEXT NOT NULL,
 visitor_id TEXT NOT NULL, variant_key TEXT NOT NULL,
 occurred_at TIMESTAMPTZ NOT NULL, received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(project_id,assignment_id,visitor_id));
CREATE INDEX IF NOT EXISTS exposures_cohort_variant
 ON exposures(project_id,assignment_id,variant_key);
CREATE TABLE IF NOT EXISTS conversions (
 event_id TEXT PRIMARY KEY,
 exposure_event_id TEXT NOT NULL REFERENCES exposures(event_id),
 goal TEXT NOT NULL, occurred_at TIMESTAMPTZ NOT NULL,
 received_at TIMESTAMPTZ NOT NULL DEFAULT now(),
 UNIQUE(exposure_event_id,goal));
CREATE INDEX IF NOT EXISTS conversions_exposure_goal
 ON conversions(exposure_event_id,goal);`

// Load returns the current complete snapshot (if any). Reads are done as one
// query to avoid seeing a pointer and payload from different publications.
func (r *ConfigRepository) Load(ctx context.Context) (config.Document, int64, error) {
	var raw []byte
	var rev int64
	err := r.DB.QueryRowContext(ctx, `SELECT s.revision,s.document FROM config_current c
        JOIN config_snapshots s ON s.revision=c.revision WHERE c.singleton=TRUE`).Scan(&rev, &raw)
	if err != nil {
		return config.Document{}, 0, err
	}
	var doc config.Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		return config.Document{}, 0, err
	}
	if _, err := config.FromDocument(doc); err != nil {
		return config.Document{}, 0, fmt.Errorf("stored snapshot invalid: %w", err)
	}
	return doc, rev, nil
}

// Publish is a compare-and-swap operation serialized at the database, not the
// process. This is needed for multiple API replicas and process restarts.
// expected=0 may create only the initial config. Later updates MUST supply
// the revision seen by the caller, preventing accidental lost updates.
func (r *ConfigRepository) Publish(ctx context.Context, doc config.Document, expected int64) (int64, error) {
	next, err := config.FromDocument(doc)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		return 0, err
	}
	tx, err := r.DB.BeginTx(ctx, &sql.TxOptions{Isolation: sql.LevelReadCommitted})
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	var existing sql.NullInt64
	if err = tx.QueryRowContext(ctx, `SELECT revision FROM config_current WHERE singleton=TRUE FOR UPDATE`).Scan(&existing); err != nil {
		return 0, err
	}
	current := int64(0)
	if existing.Valid {
		current = existing.Int64
	}
	if expected != current {
		return 0, ErrRevisionConflict
	}
	if current != 0 {
		var prevRaw []byte
		if err = tx.QueryRowContext(ctx, `SELECT document FROM config_snapshots WHERE revision=$1`, current).Scan(&prevRaw); err != nil {
			return 0, err
		}
		var prev config.Document
		if err = json.Unmarshal(prevRaw, &prev); err != nil {
			return 0, err
		}
		prevSnapshot, err := config.FromDocument(prev)
		if err != nil {
			return 0, err
		}
		if err = config.NewStore(prevSnapshot).Publish(next); err != nil {
			return 0, fmt.Errorf("%w: %v", ErrInvalidConfiguration, err)
		}
	}
	updated := current + 1
	if _, err = tx.ExecContext(ctx, `INSERT INTO config_snapshots(revision,document) VALUES($1,$2::jsonb)`, updated, string(raw)); err != nil {
		return 0, err
	}
	if _, err = tx.ExecContext(ctx, `UPDATE config_current SET revision=$1 WHERE singleton=TRUE`, updated); err != nil {
		return 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, err
	}
	return updated, nil
}
