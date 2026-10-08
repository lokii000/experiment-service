package tracking

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/lokii000/experiment-service/internal/assignment"
	"github.com/lokii000/experiment-service/internal/config"
)

type fakeRepo struct {
	mu              sync.Mutex
	eventExposure   map[string]CanonicalExposure
	visitorExposure map[string]CanonicalExposure
	eventConversion map[string]CanonicalConversion
	goalConversion  map[string]CanonicalConversion
}

func freshRepo() *fakeRepo {
	return &fakeRepo{eventExposure: map[string]CanonicalExposure{}, visitorExposure: map[string]CanonicalExposure{}, eventConversion: map[string]CanonicalConversion{}, goalConversion: map[string]CanonicalConversion{}}
}
func (r *fakeRepo) AddExposure(_ context.Context, e CanonicalExposure) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.eventExposure[e.EventID]; ok {
		if old != e {
			return Result{}, ErrConflict
		}
		return Result{Status: "duplicate"}, nil
	}
	key := e.ProjectID + "/" + e.AssignmentID + "/" + e.VisitorID
	if old, ok := r.visitorExposure[key]; ok {
		if old.VariantKey != e.VariantKey {
			return Result{}, ErrConflict
		}
		return Result{Status: "duplicate"}, nil
	}
	r.eventExposure[e.EventID] = e
	r.visitorExposure[key] = e
	return Result{Status: "recorded"}, nil
}
func (r *fakeRepo) LookupExposure(_ context.Context, pid, aid, vid string) (CanonicalExposure, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if v, ok := r.visitorExposure[pid+"/"+aid+"/"+vid]; ok {
		return v, nil
	}
	return CanonicalExposure{}, ErrMissingExposure
}
func (r *fakeRepo) AddConversion(_ context.Context, c CanonicalConversion) (Result, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.eventConversion[c.EventID]; ok {
		if old != c {
			return Result{}, ErrConflict
		}
		return Result{Status: "duplicate"}, nil
	}
	key := c.ExposureEventID + "/" + c.Goal
	if _, ok := r.goalConversion[key]; ok {
		return Result{Status: "duplicate"}, nil
	}
	r.eventConversion[c.EventID] = c
	r.goalConversion[key] = c
	return Result{Status: "recorded"}, nil
}
func demo(t *testing.T) (*Service, *fakeRepo) {
	t.Helper()
	doc := config.Document{Projects: []config.Project{{ID: "p", PublicKey: "pk", Experiments: []assignment.Experiment{{Key: "checkout", AssignmentID: "cohort", Revision: 1, Status: "active", TrafficBps: 10000, Variants: []assignment.Variant{{Key: "A", WeightBps: 5000}, {Key: "B", WeightBps: 5000}}}}}}}
	s, err := config.FromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	repo := freshRepo()
	now := time.Date(2026, 10, 8, 9, 0, 0, 0, time.UTC)
	return &Service{Repo: repo, Resolver: s, Now: func() time.Time { return now }}, repo
}
func TestExposureAndConversionIdempotency(t *testing.T) {
	svc, repo := demo(t)
	ctx := context.Background()
	now := svc.Now()
	decisions, _ := svc.Resolver.(*config.Snapshot).Assign("pk", "visitor", []string{"checkout"})
	exp := Exposure{EventID: "e1", ProjectKey: "pk", ExperimentKey: "checkout", AssignmentID: "cohort", VisitorID: "visitor", VariantKey: decisions[0].VariantKey, OccurredAt: now}
	v, err := svc.RecordExposure(ctx, exp)
	if err != nil || v.Status != "recorded" {
		t.Fatalf("first exposure: %v %v", v, err)
	}
	v, err = svc.RecordExposure(ctx, exp)
	if err != nil || v.Status != "duplicate" {
		t.Fatalf("repeat exposure: %v %v", v, err)
	}
	another := exp
	another.EventID = "e2"
	v, err = svc.RecordExposure(ctx, another)
	if err != nil || v.Status != "duplicate" {
		t.Fatalf("semantic duplicate: %v %v", v, err)
	}
	bad := exp
	bad.VisitorID = "different"
	_, err = svc.RecordExposure(ctx, bad)
	if !errors.Is(err, ErrMismatch) && !errors.Is(err, ErrConflict) {
		t.Fatalf("expected rejected conflicting retry, got %v", err)
	}
	conversion := Conversion{EventID: "c1", ProjectKey: "pk", ExperimentKey: "checkout", AssignmentID: "cohort", VisitorID: "visitor", Goal: "purchase", OccurredAt: now.Add(time.Minute)}
	v, err = svc.RecordConversion(ctx, conversion)
	if err != nil || v.Status != "recorded" {
		t.Fatalf("first conversion: %v %v", v, err)
	}
	v, err = svc.RecordConversion(ctx, conversion)
	if err != nil || v.Status != "duplicate" {
		t.Fatalf("duplicate conversion: %v %v", v, err)
	}
	conversion.EventID = "c2"
	v, err = svc.RecordConversion(ctx, conversion)
	if err != nil || v.Status != "duplicate" {
		t.Fatalf("semantic duplicate conversion: %v %v", v, err)
	}
	conversion.Goal = "signup"
	v, err = svc.RecordConversion(ctx, conversion)
	if err != nil || v.Status != "recorded" {
		t.Fatalf("different goal: %v %v", v, err)
	}
	if len(repo.visitorExposure) != 1 || len(repo.goalConversion) != 2 {
		t.Fatalf("wrong counts: %d %d", len(repo.visitorExposure), len(repo.goalConversion))
	}
}
func TestOutOfOrderAndInvalidConversion(t *testing.T) {
	svc, _ := demo(t)
	ctx := context.Background()
	now := svc.Now()
	conv := Conversion{EventID: "c1", ProjectKey: "pk", ExperimentKey: "checkout", AssignmentID: "cohort", VisitorID: "visitor", Goal: "purchase", OccurredAt: now}
	if _, err := svc.RecordConversion(ctx, conv); !errors.Is(err, ErrMissingExposure) {
		t.Fatalf("expected missing exposure, got %v", err)
	}
	decisions, _ := svc.Resolver.(*config.Snapshot).Assign("pk", "visitor", []string{"checkout"})
	_, err := svc.RecordExposure(ctx, Exposure{EventID: "e1", ProjectKey: "pk", ExperimentKey: "checkout", AssignmentID: "cohort", VisitorID: "visitor", VariantKey: decisions[0].VariantKey, OccurredAt: now.Add(time.Minute)})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.RecordConversion(ctx, conv); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected pre-exposure conversion conflict, got %v", err)
	}
}
func TestConcurrentExposureRequests(t *testing.T) {
	svc, repo := demo(t)
	ctx := context.Background()
	now := svc.Now()
	decisions, _ := svc.Resolver.(*config.Snapshot).Assign("pk", "visitor", []string{"checkout"})
	var wg sync.WaitGroup
	failures := make(chan error, 50)
	for i := 0; i < 50; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := svc.RecordExposure(ctx, Exposure{EventID: "same-event", ProjectKey: "pk", ExperimentKey: "checkout", AssignmentID: "cohort", VisitorID: "visitor", VariantKey: decisions[0].VariantKey, OccurredAt: now})
			if err != nil {
				failures <- err
			}
		}()
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatal(err)
	}
	if len(repo.visitorExposure) != 1 {
		t.Fatal("concurrent duplicate created extra exposure")
	}
}
