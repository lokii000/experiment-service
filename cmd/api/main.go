package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/lokii000/experiment-service/internal/config"
	"github.com/lokii000/experiment-service/internal/httpapi"
	"github.com/lokii000/experiment-service/internal/storage"
	"github.com/lokii000/experiment-service/internal/tracking"
)

func main() {
	if err := run(); err != nil {
		slog.Error("service startup failed", "error", err)
		os.Exit(1)
	}
}
func run() error {
	path := env("CONFIG_PATH", "config/experiments.json")
	seedBytes, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var seed config.Document
	if err = json.Unmarshal(seedBytes, &seed); err != nil {
		return err
	}
	initial, err := config.FromDocument(seed)
	if err != nil {
		return err
	}
	store := config.NewStore(initial)
	if strings.EqualFold(strings.TrimSpace(os.Getenv("REQUIRE_DATABASE")), "true") && strings.TrimSpace(os.Getenv("DATABASE_URL")) == "" {
		return errors.New("REQUIRE_DATABASE is true but DATABASE_URL is empty")
	}
	ctx := context.Background()
	var db *sql.DB
	var repo *storage.ConfigRepository
	var events *tracking.Service
	var trackingRepo *storage.TrackingRepository
	if url := os.Getenv("DATABASE_URL"); url != "" {
		db, err = sql.Open("pgx", url)
		if err != nil {
			return errors.New("PostgreSQL driver not registered: build with -tags pgx (see README)")
		}
		defer db.Close()
		db.SetMaxOpenConns(15)
		db.SetMaxIdleConns(5)
		db.SetConnMaxLifetime(15 * time.Minute)
		startup, cancel := context.WithTimeout(ctx, 8*time.Second)
		defer cancel()
		if err = db.PingContext(startup); err != nil {
			return err
		}
		repo = &storage.ConfigRepository{DB: db}
		if err = repo.Migrate(startup); err != nil {
			return err
		}
		doc, _, loadErr := repo.Load(startup)
		if errors.Is(loadErr, sql.ErrNoRows) {
			if _, err = repo.Publish(startup, seed, 0); err != nil && !errors.Is(err, storage.ErrRevisionConflict) {
				return err
			}
			doc, _, loadErr = repo.Load(startup)
		}
		if loadErr != nil {
			return loadErr
		}
		initial, err = config.FromDocument(doc)
		if err != nil {
			return err
		}
		store = config.NewStore(initial)
		trackingRepo = &storage.TrackingRepository{DB: db}
		events = &tracking.Service{Repo: trackingRepo, Resolver: store}
	}
	if repo != nil && len(os.Getenv("ADMIN_TOKEN")) < 16 {
		return errors.New("ADMIN_TOKEN must be at least 16 characters in database mode")
	}
	limits, err := rateLimitsFromEnv()
	if err != nil {
		return err
	}
	server := httpapi.New(store, parseAllowedOrigins(os.Getenv("ALLOWED_ORIGINS"))).WithRateLimits(limits).WithDatabase(repo, events, trackingRepo, os.Getenv("ADMIN_TOKEN"))
	httpServer := &http.Server{
		Addr: ":" + env("PORT", "8080"), Handler: server.Handler(),
		ReadHeaderTimeout: 2 * time.Second, ReadTimeout: 4 * time.Second,
		WriteTimeout: 4 * time.Second, IdleTimeout: 30 * time.Second,
	}
	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if repo != nil {
		go refreshConfigs(ctx, repo, store)
	}
	runErrors := make(chan error, 1)
	go func() {
		slog.Info("assignment API listening", "address", httpServer.Addr, "database_mode", repo != nil)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			runErrors <- err
		}
	}()
	select {
	case <-ctx.Done():
	case err := <-runErrors:
		return err
	}
	shutdown, cancelShutdown := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelShutdown()
	return httpServer.Shutdown(shutdown)
}
func refreshConfigs(ctx context.Context, repo *storage.ConfigRepository, store *config.Store) {
	ticker := time.NewTicker(2 * time.Second)
	defer ticker.Stop()
	// Readers continue serving the last valid snapshot during DB outages.
	// Pause propagation is eventually consistent; see DESIGN.md.
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			readCtx, cancel := context.WithTimeout(ctx, time.Second)
			doc, _, err := repo.Load(readCtx)
			cancel()
			if err != nil {
				slog.Warn("configuration refresh failed; serving cached snapshot", "error", err)
				continue
			}
			snapshot, err := config.FromDocument(doc)
			if err != nil {
				slog.Error("invalid published config", "error", err)
				continue
			}
			if err = store.Publish(snapshot); err != nil {
				slog.Warn("configuration refresh rejected", "error", err)
			}
		}
	}
}
func env(k, fallback string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return fallback
}

func parseAllowedOrigins(raw string) []string {
	return strings.Split(raw, ",")
}

// Reject invalid limits rather than silently disabling production protection.
func rateLimitsFromEnv() (httpapi.RateLimits, error) {
	limits := httpapi.DefaultRateLimits()
	for _, value := range []struct {
		name   string
		target *int
	}{
		{"ASSIGN_RATE_RPS", &limits.AssignmentRPS},
		{"ASSIGN_RATE_BURST", &limits.AssignmentBurst},
		{"TRACK_RATE_RPS", &limits.TrackingRPS},
		{"TRACK_RATE_BURST", &limits.TrackingBurst},
		{"ADMIN_RATE_RPS", &limits.AdminRPS},
		{"ADMIN_RATE_BURST", &limits.AdminBurst},
	} {
		if raw, ok := os.LookupEnv(value.name); ok {
			n, err := strconv.Atoi(strings.TrimSpace(raw))
			if err != nil || n <= 0 || n > 1000000 {
				return limits, errors.New(value.name + " must be an integer between 1 and 1000000")
			}
			*value.target = n
		}
	}
	return limits, nil
}
