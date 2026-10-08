package httpapi

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/lokii000/experiment-service/internal/assignment"
	"github.com/lokii000/experiment-service/internal/config"
)

func handler(t *testing.T) http.Handler {
	t.Helper()
	doc := config.Document{Projects: []config.Project{{ID: "project-1", PublicKey: "pk_demo", Experiments: []assignment.Experiment{{
		Key: "checkout", AssignmentID: "cohort-1", Revision: 1, Status: "active", TrafficBps: 10000,
		Variants: []assignment.Variant{{Key: "control", WeightBps: 5000}, {Key: "treatment", WeightBps: 5000}},
	}}}}}
	s, err := config.FromDocument(doc)
	if err != nil {
		t.Fatal(err)
	}
	return New(config.NewStore(s), []string{"https://store.example"}).Handler()
}
func TestHTTPMultiExperimentAndRepeat(t *testing.T) {
	h := handler(t)
	request := []byte(`{"project_key":"pk_demo","visitor_id":"user1","experiment_keys":["checkout","missing"]}`)
	var first map[string][]assignment.Decision
	for i := 0; i < 2; i++ {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/assignments", bytes.NewReader(request)))
		if w.Code != http.StatusOK {
			t.Fatalf("status %d body %s", w.Code, w.Body.String())
		}
		var body map[string][]assignment.Decision
		if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if len(body["assignments"]) != 2 || body["assignments"][0].Status != "assigned" || body["assignments"][1].Status != "unknown_experiment" {
			t.Fatalf("unexpected %+v", body)
		}
		if i == 0 {
			first = body
		} else if body["assignments"][0] != first["assignments"][0] {
			t.Fatal("nonsticky assignment")
		}
	}
}
func TestHTTPCorsAndInvalidBody(t *testing.T) {
	h := handler(t)
	request := []byte(`{"project_key":"pk_demo","visitor_id":"v","experiment_keys":["checkout"]}`)
	w := httptest.NewRecorder()
	r := httptest.NewRequest("POST", "/v1/assignments", bytes.NewReader(request))
	r.Header.Set("Origin", "https://store.example")
	h.ServeHTTP(w, r)
	if w.Header().Get("Access-Control-Allow-Origin") != "https://store.example" {
		t.Fatal("expected allowed CORS origin")
	}
	w = httptest.NewRecorder()
	h.ServeHTTP(w, httptest.NewRequest("POST", "/v1/assignments", bytes.NewReader([]byte(`{"project_key":"pk_demo","visitor_id":"v","experiment_keys":["checkout"],"unknown":true}`))))
	if w.Code != 400 {
		t.Fatalf("unknown fields accepted: %d", w.Code)
	}
}
