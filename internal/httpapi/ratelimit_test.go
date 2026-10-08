package httpapi

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/lokii000/experiment-service/internal/assignment"
	"github.com/lokii000/experiment-service/internal/config"
)

func TestLimiterRefillAndGlobalCap(t *testing.T) {
	l := newIPLimiter(1, 2)
	now := time.Unix(100, 0)
	if !l.allowAt("1.1.1.1", now) || !l.allowAt("1.1.1.1", now) {
		t.Fatal("initial burst should succeed")
	}
	if l.allowAt("1.1.1.1", now) {
		t.Fatal("third request should be limited")
	}
	if !l.allowAt("1.1.1.1", now.Add(time.Second)) {
		t.Fatal("bucket should refill")
	}
	// 10x global capacity: 20 distinct IPs consume 20 tokens.
	global := newIPLimiter(1, 1)
	for i := 0; i < 10; i++ {
		if !global.allowAt(string(rune('a'+i)), now) {
			t.Fatalf("global request %d should pass", i)
		}
	}
	if global.allowAt("eleventh", now) {
		t.Fatal("global capacity must be enforced")
	}
}

func TestLimiterClientIdentityIgnoresSpoofedHeaders(t *testing.T) {
	r := httptest.NewRequest("POST", "/v1/assignments", nil)
	r.RemoteAddr = "192.0.2.10:2500"
	r.Header.Set("X-Forwarded-For", "192.0.2.99")
	if got := requestPeer(r); got != "192.0.2.10" {
		t.Fatalf("trusted spoofed forwarding header: %q", got)
	}
}

func TestRateLimitedAssignmentsReturnRetryAfter(t *testing.T) {
	doc := config.Document{Projects: []config.Project{{ID: "project-1", PublicKey: "pk_demo", Experiments: []assignment.Experiment{{
		Key: "checkout", AssignmentID: "cohort-1", Revision: 1, Status: "active", TrafficBps: 10000,
		Variants: []assignment.Variant{{Key: "control", WeightBps: 5000}, {Key: "treatment", WeightBps: 5000}},
	}}}}}
	snap, err := config.FromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	limits := DefaultRateLimits()
	limits.AssignmentRPS, limits.AssignmentBurst = 1, 2
	h := New(config.NewStore(snap), nil).WithRateLimits(limits).Handler()
	for i := 0; i < 3; i++ {
		r := httptest.NewRequest(http.MethodPost, "/v1/assignments", nil)
		r.RemoteAddr = "192.0.2.10:3000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i < 2 && w.Code == http.StatusTooManyRequests {
			t.Fatalf("request %d unexpectedly limited", i)
		}
		if i == 2 {
			if w.Code != http.StatusTooManyRequests || w.Header().Get("Retry-After") != "1" {
				t.Fatalf("expected 429 Retry-After=1; got %d %v", w.Code, w.Header())
			}
		}
	}
}

func TestTrackingRoutesShareLimit(t *testing.T) {
	// Tracking middleware runs before validation/storage, and exposure and
	// conversion share one budget to bound database write pressure.
	limits := DefaultRateLimits()
	limits.TrackingRPS, limits.TrackingBurst = 1, 1
	h := newTestRateLimitedHandler(t, limits)
	for i, path := range []string{"/v1/events/exposure", "/v1/events/conversion"} {
		r := httptest.NewRequest(http.MethodPost, path, nil)
		r.RemoteAddr = "192.0.2.10:3000"
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if i == 0 && w.Code != http.StatusServiceUnavailable {
			t.Fatalf("first request unexpectedly limited: %d", w.Code)
		}
		if i == 1 && w.Code != http.StatusTooManyRequests {
			t.Fatalf("expected shared limit for conversion: %d", w.Code)
		}
	}
}

func newTestRateLimitedHandler(t *testing.T, limits RateLimits) http.Handler {
	t.Helper()
	doc := config.Document{Projects: []config.Project{{ID: "project-1", PublicKey: "pk_demo", Experiments: []assignment.Experiment{{
		Key: "checkout", AssignmentID: "cohort-1", Revision: 1, Status: "active", TrafficBps: 10000,
		Variants: []assignment.Variant{{Key: "control", WeightBps: 5000}, {Key: "treatment", WeightBps: 5000}},
	}}}}}
	snap, err := config.FromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	return New(config.NewStore(snap), nil).WithRateLimits(limits).Handler()
}

func TestRateLimiterBoundsClientMap(t *testing.T) {
	l := newIPLimiter(100000, 100000)
	now := time.Unix(100, 0)
	for i := 0; i < maxRateLimitClients+500; i++ {
		l.allowAt(strconv.Itoa(i), now)
	}
	if len(l.clients) > maxRateLimitClients+1 {
		t.Fatalf("unbounded client buckets: %d", len(l.clients))
	}
}
