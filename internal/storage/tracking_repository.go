package storage

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/lokii000/experiment-service/internal/tracking"
)

type TrackingRepository struct{ DB *sql.DB }

func equivalentTime(a, b time.Time) bool {
	return a.UTC().Truncate(time.Microsecond).Equal(b.UTC().Truncate(time.Microsecond))
}

func (r *TrackingRepository) AddExposure(ctx context.Context, e tracking.CanonicalExposure) (tracking.Result, error) {
	res, err := r.DB.ExecContext(ctx, `INSERT INTO exposures(event_id,project_id,assignment_id,experiment_key,visitor_id,variant_key,occurred_at)
        VALUES($1,$2,$3,$4,$5,$6,$7) ON CONFLICT DO NOTHING`, e.EventID, e.ProjectID, e.AssignmentID, e.ExperimentKey, e.VisitorID, e.VariantKey, e.OccurredAt)
	if err != nil {
		return tracking.Result{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return tracking.Result{}, err
	}
	if n == 1 {
		return tracking.Result{Status: "recorded"}, nil
	}
	var existing tracking.CanonicalExposure
	err = r.DB.QueryRowContext(ctx, `SELECT event_id,project_id,assignment_id,experiment_key,visitor_id,variant_key,occurred_at FROM exposures WHERE event_id=$1`, e.EventID).Scan(
		&existing.EventID, &existing.ProjectID, &existing.AssignmentID, &existing.ExperimentKey, &existing.VisitorID, &existing.VariantKey, &existing.OccurredAt)
	if err == nil {
		if existing.ProjectID == e.ProjectID && existing.AssignmentID == e.AssignmentID && existing.ExperimentKey == e.ExperimentKey && existing.VisitorID == e.VisitorID && existing.VariantKey == e.VariantKey && equivalentTime(existing.OccurredAt, e.OccurredAt) {
			return tracking.Result{Status: "duplicate"}, nil
		}
		return tracking.Result{}, tracking.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return tracking.Result{}, err
	}
	err = r.DB.QueryRowContext(ctx, `SELECT event_id,project_id,assignment_id,experiment_key,visitor_id,variant_key,occurred_at FROM exposures WHERE project_id=$1 AND assignment_id=$2 AND visitor_id=$3`, e.ProjectID, e.AssignmentID, e.VisitorID).Scan(
		&existing.EventID, &existing.ProjectID, &existing.AssignmentID, &existing.ExperimentKey, &existing.VisitorID, &existing.VariantKey, &existing.OccurredAt)
	if err != nil {
		return tracking.Result{}, err
	}
	if existing.ExperimentKey != e.ExperimentKey || existing.VariantKey != e.VariantKey {
		return tracking.Result{}, tracking.ErrConflict
	}
	return tracking.Result{Status: "duplicate"}, nil
}
func (r *TrackingRepository) LookupExposure(ctx context.Context, projectID, assignmentID, visitorID string) (tracking.CanonicalExposure, error) {
	var e tracking.CanonicalExposure
	err := r.DB.QueryRowContext(ctx, `SELECT event_id,project_id,assignment_id,experiment_key,visitor_id,variant_key,occurred_at FROM exposures WHERE project_id=$1 AND assignment_id=$2 AND visitor_id=$3`, projectID, assignmentID, visitorID).Scan(
		&e.EventID, &e.ProjectID, &e.AssignmentID, &e.ExperimentKey, &e.VisitorID, &e.VariantKey, &e.OccurredAt)
	if errors.Is(err, sql.ErrNoRows) {
		return e, tracking.ErrMissingExposure
	}
	return e, err
}
func (r *TrackingRepository) AddConversion(ctx context.Context, c tracking.CanonicalConversion) (tracking.Result, error) {
	res, err := r.DB.ExecContext(ctx, `INSERT INTO conversions(event_id,exposure_event_id,goal,occurred_at) VALUES($1,$2,$3,$4) ON CONFLICT DO NOTHING`, c.EventID, c.ExposureEventID, c.Goal, c.OccurredAt)
	if err != nil {
		return tracking.Result{}, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return tracking.Result{}, err
	}
	if n == 1 {
		return tracking.Result{Status: "recorded"}, nil
	}
	var previous tracking.CanonicalConversion
	err = r.DB.QueryRowContext(ctx, `SELECT event_id,exposure_event_id,goal,occurred_at FROM conversions WHERE event_id=$1`, c.EventID).Scan(&previous.EventID, &previous.ExposureEventID, &previous.Goal, &previous.OccurredAt)
	if err == nil {
		if previous.ExposureEventID == c.ExposureEventID && previous.Goal == c.Goal && equivalentTime(previous.OccurredAt, c.OccurredAt) {
			return tracking.Result{Status: "duplicate"}, nil
		}
		return tracking.Result{}, tracking.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return tracking.Result{}, err
	}
	err = r.DB.QueryRowContext(ctx, `SELECT event_id,exposure_event_id,goal,occurred_at FROM conversions WHERE exposure_event_id=$1 AND goal=$2`, c.ExposureEventID, c.Goal).Scan(&previous.EventID, &previous.ExposureEventID, &previous.Goal, &previous.OccurredAt)
	if err != nil {
		return tracking.Result{}, err
	}
	return tracking.Result{Status: "duplicate"}, nil
}

type VariantResult struct {
	VariantKey     string   `json:"variant_key"`
	Exposures      int64    `json:"exposures"`
	Conversions    int64    `json:"conversions"`
	ConversionRate *float64 `json:"conversion_rate"`
}

func (r *TrackingRepository) Results(ctx context.Context, projectID, assignmentID, goal string) ([]VariantResult, error) {
	// Counts are separately aggregated; joining raw exposure & conversion
	// events directly would inflate both counts for multi-goal reporting.
	rows, err := r.DB.QueryContext(ctx, `WITH e AS (
       SELECT variant_key,COUNT(*) AS exposures FROM exposures
       WHERE project_id=$1 AND assignment_id=$2 GROUP BY variant_key
    ), c AS (
       SELECT ex.variant_key,COUNT(*) AS conversions FROM conversions co
       JOIN exposures ex ON ex.event_id=co.exposure_event_id
       WHERE ex.project_id=$1 AND ex.assignment_id=$2 AND co.goal=$3
       GROUP BY ex.variant_key
    ) SELECT e.variant_key,e.exposures,COALESCE(c.conversions,0) FROM e
       LEFT JOIN c ON c.variant_key=e.variant_key ORDER BY e.variant_key`, projectID, assignmentID, goal)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []VariantResult{}
	for rows.Next() {
		var v VariantResult
		if err := rows.Scan(&v.VariantKey, &v.Exposures, &v.Conversions); err != nil {
			return nil, err
		}
		if v.Exposures > 0 {
			rate := float64(v.Conversions) / float64(v.Exposures)
			v.ConversionRate = &rate
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
