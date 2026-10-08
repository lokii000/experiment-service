package tracking

import (
	"context"
	"errors"
	"time"
)

var (
	ErrInvalid         = errors.New("invalid event")
	ErrMismatch        = errors.New("assignment mismatch")
	ErrConflict        = errors.New("event conflict")
	ErrMissingExposure = errors.New("exposure not found")
)

type Exposure struct {
	EventID       string    `json:"event_id"`
	ProjectKey    string    `json:"project_key"`
	ExperimentKey string    `json:"experiment_key"`
	AssignmentID  string    `json:"assignment_id"`
	VisitorID     string    `json:"visitor_id"`
	VariantKey    string    `json:"variant_key"`
	OccurredAt    time.Time `json:"occurred_at"`
}

type Conversion struct {
	EventID       string    `json:"event_id"`
	ProjectKey    string    `json:"project_key"`
	ExperimentKey string    `json:"experiment_key"`
	AssignmentID  string    `json:"assignment_id"`
	VisitorID     string    `json:"visitor_id"`
	Goal          string    `json:"goal"`
	OccurredAt    time.Time `json:"occurred_at"`
}

type CanonicalExposure struct {
	EventID, ProjectID, AssignmentID, ExperimentKey, VisitorID, VariantKey string
	OccurredAt                                                             time.Time
}

type CanonicalConversion struct {
	EventID, ExposureEventID, Goal string
	OccurredAt                     time.Time
}

type Result struct {
	Status string `json:"status"`
} // recorded or duplicate

type Repository interface {
	AddExposure(ctx context.Context, e CanonicalExposure) (Result, error)
	LookupExposure(ctx context.Context, projectID, assignmentID, visitorID string) (CanonicalExposure, error)
	AddConversion(ctx context.Context, c CanonicalConversion) (Result, error)
}

type Resolver interface {
	ResolveExposure(projectKey, experimentKey, assignmentID, visitorID, variantKey string) (projectID string, ok bool)
	ResolveCohort(projectKey, experimentKey, assignmentID string) (projectID string, ok bool)
}

type Service struct {
	Repo     Repository
	Resolver Resolver
	Now      func() time.Time
}

func validTime(t, timeNow time.Time) bool {
	return !t.IsZero() && !t.After(timeNow.Add(5*time.Minute)) && !t.Before(timeNow.Add(-7*24*time.Hour))
}
func validIdentifier(s string, limit int) bool { return len(s) > 0 && len(s) <= limit }

func (s *Service) RecordExposure(ctx context.Context, e Exposure) (Result, error) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	if !validIdentifier(e.EventID, 128) || !validIdentifier(e.VisitorID, 256) || !validIdentifier(e.ProjectKey, 128) || !validIdentifier(e.ExperimentKey, 128) || !validIdentifier(e.AssignmentID, 128) || !validIdentifier(e.VariantKey, 128) || !validTime(e.OccurredAt, now) {
		return Result{}, ErrInvalid
	}
	pid, ok := s.Resolver.ResolveExposure(e.ProjectKey, e.ExperimentKey, e.AssignmentID, e.VisitorID, e.VariantKey)
	if !ok {
		return Result{}, ErrMismatch
	}
	return s.Repo.AddExposure(ctx, CanonicalExposure{EventID: e.EventID, ProjectID: pid, AssignmentID: e.AssignmentID, ExperimentKey: e.ExperimentKey, VisitorID: e.VisitorID, VariantKey: e.VariantKey, OccurredAt: e.OccurredAt})
}
func (s *Service) RecordConversion(ctx context.Context, c Conversion) (Result, error) {
	now := time.Now().UTC()
	if s.Now != nil {
		now = s.Now()
	}
	if !validIdentifier(c.EventID, 128) || !validIdentifier(c.VisitorID, 256) || !validIdentifier(c.ProjectKey, 128) || !validIdentifier(c.ExperimentKey, 128) || !validIdentifier(c.AssignmentID, 128) || !validIdentifier(c.Goal, 128) || !validTime(c.OccurredAt, now) {
		return Result{}, ErrInvalid
	}
	pid, ok := s.Resolver.ResolveCohort(c.ProjectKey, c.ExperimentKey, c.AssignmentID)
	if !ok {
		return Result{}, ErrMismatch
	}
	exposure, err := s.Repo.LookupExposure(ctx, pid, c.AssignmentID, c.VisitorID)
	if err != nil {
		return Result{}, err
	}
	// No conversion should be attributed to an exposure it predates.
	if c.OccurredAt.Before(exposure.OccurredAt) {
		return Result{}, ErrConflict
	}
	return s.Repo.AddConversion(ctx, CanonicalConversion{EventID: c.EventID, ExposureEventID: exposure.EventID, Goal: c.Goal, OccurredAt: c.OccurredAt})
}
