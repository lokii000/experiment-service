package httpapi

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lokii000/experiment-service/internal/assignment"
	"github.com/lokii000/experiment-service/internal/config"
)

func TestAdminAndMissingStorage(t *testing.T) {
	doc := config.Document{Projects: []config.Project{{ID: "p", PublicKey: "pk", Experiments: []assignment.Experiment{{Key: "exp", AssignmentID: "id", Revision: 1, Status: "active", TrafficBps: 10000, Variants: []assignment.Variant{{Key: "a", WeightBps: 5000}, {Key: "b", WeightBps: 5000}}}}}}}
	snap, err := config.FromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	handler := New(config.NewStore(snap), []string{"https://shop.example"}).Handler()
	for _, route := range []string{"/v1/admin/config", "/v1/admin/results?project_key=pk&experiment_key=exp&goal=purchase"} {
		r := httptest.NewRequest("GET", route, nil)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != http.StatusUnauthorized {
			t.Fatalf("route %s returned %d", route, w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/v1/events/exposure", bytes.NewBufferString(`{}`))
	w := httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("missing storage should return 503, got %d", w.Code)
	}
	r = httptest.NewRequest("POST", "/v1/assignments", bytes.NewBufferString(`{"project_key":"pk","visitor_id":"u","experiment_keys":["exp"]}`))
	r.Header.Set("Origin", "https://evil.example")
	w = httptest.NewRecorder()
	handler.ServeHTTP(w, r)
	if w.Code != http.StatusForbidden {
		t.Fatalf("unexpected CORS allowed: %d", w.Code)
	}
}
