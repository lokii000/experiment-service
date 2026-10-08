//go:build pgx

package storage

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/lokii000/experiment-service/internal/assignment"
	"github.com/lokii000/experiment-service/internal/config"
	"github.com/lokii000/experiment-service/internal/tracking"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Run against an expendable PostgreSQL instance:
// TEST_DATABASE_URL=postgres://... go test -tags pgx ./internal/storage -run Integration -v
// Every test creates a schema and drops it at the end. MaxOpenConns=1 keeps the
// session-local search_path on the same connection. Additional multi-node
// stress testing against an isolated database is a separate production task.
func integrationDB(t *testing.T) *sql.DB {
	t.Helper()
	dsn := os.Getenv("TEST_DATABASE_URL")
	if dsn == "" {
		t.Skip("set TEST_DATABASE_URL to run PostgreSQL integration tests")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	schemaName := fmt.Sprintf("t_experiment_%d", time.Now().UnixNano())
	if _, err = db.ExecContext(ctx, `CREATE SCHEMA `+schemaName); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, `SET search_path TO `+schemaName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Exec(`DROP SCHEMA ` + schemaName + ` CASCADE`); db.Close() })
	return db
}
func integrationDocument() config.Document {
	return config.Document{Projects: []config.Project{{ID: "project-1", PublicKey: "pk-demo", Experiments: []assignment.Experiment{{Key: "checkout", AssignmentID: "cohort-1", Revision: 1, Status: "active", TrafficBps: 10000, Variants: []assignment.Variant{{Key: "control", WeightBps: 5000}, {Key: "treatment", WeightBps: 5000}}}}}}}
}
func TestIntegrationConfigPublishingAndEventCounting(t *testing.T) {
	db := integrationDB(t)
	ctx := context.Background()
	configs := &ConfigRepository{DB: db}
	if err := configs.Migrate(ctx); err != nil {
		t.Fatal(err)
	}
	doc := integrationDocument()
	rev, err := configs.Publish(ctx, doc, 0)
	if err != nil || rev != 1 {
		t.Fatalf("initial publish %d %v", rev, err)
	}
	_, err = configs.Publish(ctx, doc, 0)
	if !errors.Is(err, ErrRevisionConflict) {
		t.Fatalf("stale revision accepted: %v", err)
	}
	revisionDoc, loadedRev, err := configs.Load(ctx)
	if err != nil || loadedRev != 1 {
		t.Fatalf("load %d %v", loadedRev, err)
	}
	snap, err := config.FromDocument(revisionDoc)
	if err != nil {
		t.Fatal(err)
	}
	repo := &TrackingRepository{DB: db}
	srv := &tracking.Service{Repo: repo, Resolver: snap}
	now := time.Now().UTC().Truncate(time.Microsecond)
	visitor := "v123"
	assigned, err := snap.Assign("pk-demo", visitor, []string{"checkout"})
	if err != nil {
		t.Fatal(err)
	}
	exposure := tracking.Exposure{EventID: "event-exposure-1", ProjectKey: "pk-demo", ExperimentKey: "checkout", AssignmentID: "cohort-1", VisitorID: visitor, VariantKey: assigned[0].VariantKey, OccurredAt: now}
	for i := 0; i < 2; i++ {
		result, err := srv.RecordExposure(ctx, exposure)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 && result.Status != "recorded" {
			t.Fatal(result)
		}
		if i == 1 && result.Status != "duplicate" {
			t.Fatal(result)
		}
	}
	exposure.EventID = "event-exposure-2"
	result, err := srv.RecordExposure(ctx, exposure)
	if err != nil || result.Status != "duplicate" {
		t.Fatalf("semantic retry: %+v %v", result, err)
	}
	conversion := tracking.Conversion{EventID: "event-conversion-1", ProjectKey: "pk-demo", ExperimentKey: "checkout", AssignmentID: "cohort-1", VisitorID: visitor, Goal: "checkout_completed", OccurredAt: now.Add(time.Second)}
	for i := 0; i < 2; i++ {
		_, err = srv.RecordConversion(ctx, conversion)
		if err != nil {
			t.Fatal(err)
		}
	}
	converted, err := repo.Results(ctx, "project-1", "cohort-1", "checkout_completed")
	if err != nil {
		t.Fatal(err)
	}
	if len(converted) != 1 || converted[0].Exposures != 1 || converted[0].Conversions != 1 {
		t.Fatalf("wrong results %+v", converted)
	}
	// Allocation mutation cannot be published in-place even when revision increases.
	updated := integrationDocument()
	updated.Projects[0].Experiments[0].Revision = 2
	updated.Projects[0].Experiments[0].Variants[0].WeightBps = 9000
	updated.Projects[0].Experiments[0].Variants[1].WeightBps = 1000
	_, err = configs.Publish(ctx, updated, 1)
	if !errors.Is(err, ErrInvalidConfiguration) {
		t.Fatalf("allocation mutated: %v", err)
	}
	// Monotonic traffic increase and status/revision change are supported.
	updated = integrationDocument()
	updated.Projects[0].Experiments[0].Revision = 2
	updated.Projects[0].Experiments[0].Status = "paused"
	rev, err = configs.Publish(ctx, updated, 1)
	if err != nil || rev != 2 {
		t.Fatalf("status publish %d %v", rev, err)
	}
	loaded, rev, err := configs.Load(ctx)
	if err != nil || rev != 2 || !strings.EqualFold(loaded.Projects[0].Experiments[0].Status, "paused") {
		t.Fatalf("persisted config %d %v", rev, err)
	}
}
