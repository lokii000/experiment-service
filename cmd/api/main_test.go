package main

import (
	"testing"

	"github.com/lokii000/experiment-service/internal/config"
	"github.com/lokii000/experiment-service/internal/httpapi"
	"net/http"
	"net/http/httptest"
)

func TestCommaSeparatedOrigins(t *testing.T) {
	server := httpapi.New(config.NewStore(nil), parseAllowedOrigins("https://one.example,https://two.example"))
	for _, origin := range []string{"https://one.example", "https://two.example"} {
		req := httptest.NewRequest("OPTIONS", "/v1/assignments", nil)
		req.Header.Set("Origin", origin)
		w := httptest.NewRecorder()
		server.Handler().ServeHTTP(w, req)
		if w.Code != http.StatusNoContent || w.Header().Get("Access-Control-Allow-Origin") != origin {
			t.Fatalf("CORS failed for %s: code=%d, allow-origin=%q", origin, w.Code, w.Header().Get("Access-Control-Allow-Origin"))
		}
	}
}

func TestRateLimitEnvironment(t *testing.T) {
	t.Setenv("ASSIGN_RATE_RPS", "250")
	t.Setenv("TRACK_RATE_BURST", "15")
	limits, err := rateLimitsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if limits.AssignmentRPS != 250 || limits.TrackingBurst != 15 {
		t.Fatalf("rate limit environment not applied: %+v", limits)
	}
	t.Setenv("ADMIN_RATE_RPS", "0")
	if _, err := rateLimitsFromEnv(); err == nil {
		t.Fatal("zero rate must fail startup validation")
	}
}
