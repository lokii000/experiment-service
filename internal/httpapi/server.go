package httpapi

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/lokii000/experiment-service/internal/config"
	"github.com/lokii000/experiment-service/internal/storage"
	"github.com/lokii000/experiment-service/internal/tracking"
)

type Server struct {
	store           *config.Store
	allowed         map[string]bool
	adminTokenHash  [32]byte
	adminConfigured bool
	configs         *storage.ConfigRepository
	events          *tracking.Service
	reports         *storage.TrackingRepository
	assignLimit     *ipLimiter
	trackingLimit   *ipLimiter
	adminLimit      *ipLimiter
}

func New(store *config.Store, allowedOrigins []string) *Server {
	s := &Server{store: store, allowed: map[string]bool{}}
	for _, o := range allowedOrigins {
		if o = strings.TrimSpace(o); o != "" {
			s.allowed[o] = true
		}
	}
	return s.WithRateLimits(DefaultRateLimits())
}

// WithRateLimits is intended for startup configuration, before Handler is used.
func (s *Server) WithRateLimits(limits RateLimits) *Server {
	s.assignLimit = newIPLimiter(limits.AssignmentRPS, limits.AssignmentBurst)
	s.trackingLimit = newIPLimiter(limits.TrackingRPS, limits.TrackingBurst)
	s.adminLimit = newIPLimiter(limits.AdminRPS, limits.AdminBurst)
	return s
}

func (s *Server) WithDatabase(cfg *storage.ConfigRepository, events *tracking.Service, reports *storage.TrackingRepository, adminToken string) *Server {
	s.configs = cfg
	s.events = events
	s.reports = reports
	if len(adminToken) >= 16 {
		s.adminTokenHash = sha256.Sum256([]byte(adminToken))
		s.adminConfigured = true
	}
	return s
}
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, map[string]string{"status": "ok"}) })
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, r *http.Request) {
		if s.store.Current() == nil {
			writeError(w, 503, "configuration_unavailable")
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ready"})
	})
	mux.HandleFunc("POST /v1/assignments", s.assignLimit.wrap(s.assign))
	mux.HandleFunc("OPTIONS /v1/assignments", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("POST /v1/events/exposure", s.trackingLimit.wrap(s.exposure))
	mux.HandleFunc("POST /v1/events/conversion", s.trackingLimit.wrap(s.conversion))
	mux.HandleFunc("OPTIONS /v1/events/exposure", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("OPTIONS /v1/events/conversion", func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) })
	mux.HandleFunc("GET /v1/admin/config", s.adminLimit.wrap(s.authorize(s.getConfig)))
	mux.HandleFunc("POST /v1/admin/config", s.adminLimit.wrap(s.authorize(s.publishConfig)))
	mux.HandleFunc("GET /v1/admin/results", s.adminLimit.wrap(s.authorize(s.results)))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("Referrer-Policy", "no-referrer")
		if origin := r.Header.Get("Origin"); origin != "" {
			// Admin endpoints cannot be called from third-party browser origins.
			if strings.HasPrefix(r.URL.Path, "/v1/admin/") {
				writeError(w, 403, "origin_not_allowed")
				return
			}
			if !s.allowed[origin] {
				writeError(w, 403, "origin_not_allowed")
				return
			}
			w.Header().Set("Access-Control-Allow-Origin", origin)
			w.Header().Set("Vary", "Origin")
			w.Header().Set("Access-Control-Allow-Methods", "POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
		}
		mux.ServeHTTP(w, r)
	})
}
func decode(w http.ResponseWriter, r *http.Request, dst any) error {
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("trailing JSON")
	}
	return nil
}
func dbContext(r *http.Request) (context.Context, context.CancelFunc) {
	return context.WithTimeout(r.Context(), 2*time.Second)
}
func (s *Server) authorize(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		provided := r.Header.Get("Authorization")
		const prefix = "Bearer "
		if !s.adminConfigured || !strings.HasPrefix(provided, prefix) {
			writeError(w, 401, "unauthorized")
			return
		}
		digest := sha256.Sum256([]byte(strings.TrimPrefix(provided, prefix)))
		if subtle.ConstantTimeCompare(digest[:], s.adminTokenHash[:]) != 1 {
			writeError(w, 401, "unauthorized")
			return
		}
		next(w, r)
	}
}

type assignRequest struct {
	ProjectKey     string   `json:"project_key"`
	VisitorID      string   `json:"visitor_id"`
	ExperimentKeys []string `json:"experiment_keys"`
}

func (s *Server) assign(w http.ResponseWriter, r *http.Request) {
	var input assignRequest
	if decode(w, r, &input) != nil {
		writeError(w, 400, "invalid_request")
		return
	}
	if !valid(input.ProjectKey, 128) || !valid(input.VisitorID, 256) || len(input.ExperimentKeys) == 0 || len(input.ExperimentKeys) > 20 {
		writeError(w, 400, "invalid_request")
		return
	}
	for _, key := range input.ExperimentKeys {
		if !valid(key, 128) {
			writeError(w, 400, "invalid_request")
			return
		}
	}
	snap := s.store.Current()
	if snap == nil {
		writeError(w, 503, "configuration_unavailable")
		return
	}
	decisions, err := snap.Assign(input.ProjectKey, input.VisitorID, input.ExperimentKeys)
	if err != nil {
		writeError(w, 404, "unknown_project")
		return
	}
	writeJSON(w, 200, map[string]any{"assignments": decisions})
}
func valid(s string, max int) bool { return len(s) > 0 && len(s) <= max }
func (s *Server) exposure(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeError(w, 503, "tracking_unavailable")
		return
	}
	var input tracking.Exposure
	if decode(w, r, &input) != nil {
		writeError(w, 400, "invalid_request")
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	result, err := s.events.RecordExposure(ctx, input)
	trackingResponse(w, result, err)
}
func (s *Server) conversion(w http.ResponseWriter, r *http.Request) {
	if s.events == nil {
		writeError(w, 503, "tracking_unavailable")
		return
	}
	var input tracking.Conversion
	if decode(w, r, &input) != nil {
		writeError(w, 400, "invalid_request")
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	result, err := s.events.RecordConversion(ctx, input)
	trackingResponse(w, result, err)
}
func trackingResponse(w http.ResponseWriter, result tracking.Result, err error) {
	if err == nil {
		if result.Status == "recorded" {
			writeJSON(w, 201, result)
		} else {
			writeJSON(w, 200, result)
		}
		return
	}
	switch {
	case errors.Is(err, tracking.ErrInvalid):
		writeError(w, 400, "invalid_event")
	case errors.Is(err, tracking.ErrMismatch):
		writeError(w, 409, "assignment_mismatch")
	case errors.Is(err, tracking.ErrConflict):
		writeError(w, 409, "event_conflict")
	case errors.Is(err, tracking.ErrMissingExposure):
		writeError(w, 409, "exposure_not_found")
	default:
		slog.Error("tracking storage failure", "error", err)
		writeError(w, 503, "tracking_unavailable")
	}
}

type publishRequest struct {
	ExpectedRevision int64           `json:"expected_revision"`
	Configuration    config.Document `json:"configuration"`
}

func (s *Server) getConfig(w http.ResponseWriter, r *http.Request) {
	if s.configs == nil {
		writeError(w, 503, "config_database_unavailable")
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	doc, rev, err := s.configs.Load(ctx)
	if err != nil {
		slog.Error("config load failed", "error", err)
		writeError(w, 503, "config_database_unavailable")
		return
	}
	writeJSON(w, 200, map[string]any{"revision": rev, "configuration": doc})
}
func (s *Server) publishConfig(w http.ResponseWriter, r *http.Request) {
	if s.configs == nil {
		writeError(w, 503, "config_database_unavailable")
		return
	}
	var input publishRequest
	if decode(w, r, &input) != nil || input.ExpectedRevision < 0 {
		writeError(w, 400, "invalid_request")
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	rev, err := s.configs.Publish(ctx, input.Configuration, input.ExpectedRevision)
	if err != nil {
		switch {
		case errors.Is(err, storage.ErrRevisionConflict):
			writeError(w, 409, "stale_configuration_revision")
		case errors.Is(err, storage.ErrInvalidConfiguration):
			writeError(w, 400, "configuration_invalid")
		default:
			slog.Error("config publish failed", "error", err)
			writeError(w, 503, "config_database_unavailable")
		}
		return
	}
	// Load authoritative latest state: a concurrent writer may already have
	// published again while this process was updating its local cache.
	doc, _, err := s.configs.Load(ctx)
	if err == nil {
		if next, e := config.FromDocument(doc); e == nil {
			if e = s.store.Publish(next); e != nil {
				slog.Warn("cached snapshot refresh deferred", "error", e)
			}
		}
	}
	writeJSON(w, 200, map[string]int64{"revision": rev})
}
func (s *Server) results(w http.ResponseWriter, r *http.Request) {
	if s.reports == nil {
		writeError(w, 503, "results_unavailable")
		return
	}
	projectKey := r.URL.Query().Get("project_key")
	experimentKey := r.URL.Query().Get("experiment_key")
	goal := r.URL.Query().Get("goal")
	if !valid(projectKey, 128) || !valid(experimentKey, 128) || !valid(goal, 128) {
		writeError(w, 400, "invalid_request")
		return
	}
	snap := s.store.Current()
	if snap == nil {
		writeError(w, 503, "configuration_unavailable")
		return
	}
	projectID, assignmentID, keys, ok := snap.ResultsMetadata(projectKey, experimentKey)
	if !ok {
		writeError(w, 404, "unknown_experiment")
		return
	}
	ctx, cancel := dbContext(r)
	defer cancel()
	rows, err := s.reports.Results(ctx, projectID, assignmentID, goal)
	if err != nil {
		slog.Error("results query failed", "error", err)
		writeError(w, 503, "results_unavailable")
		return
	}
	// Include variants with zero exposures, in published order.
	indexed := make(map[string]storage.VariantResult, len(rows))
	for _, v := range rows {
		indexed[v.VariantKey] = v
	}
	all := make([]storage.VariantResult, 0, len(keys))
	for _, key := range keys {
		v, ok := indexed[key]
		if !ok {
			v = storage.VariantResult{VariantKey: key}
		}
		all = append(all, v)
	}
	writeJSON(w, 200, map[string]any{"experiment_key": experimentKey, "assignment_id": assignmentID, "goal": goal, "variants": all})
}
func writeError(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]string{"error": code})
}
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
