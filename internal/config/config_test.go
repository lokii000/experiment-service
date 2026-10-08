package config

import (
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/lokii000/experiment-service/internal/assignment"
)

func document(weights int, traffic int, status string, rev int) Document {
	return Document{Projects: []Project{{ID: "project-id-1", PublicKey: "pk_demo", Experiments: []assignment.Experiment{{
		Key: "checkout", AssignmentID: "cohort-1", Revision: rev, Status: status, TrafficBps: traffic,
		Variants: []assignment.Variant{{Key: "A", WeightBps: weights}, {Key: "B", WeightBps: 10_000 - weights}},
	}}}}}
}
func mustSnapshot(t *testing.T, d Document) *Snapshot {
	t.Helper()
	s, err := FromDocument(d)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestRejectsPublishedAllocationChangeAndKeepsOldSnapshot(t *testing.T) {
	original := mustSnapshot(t, document(5000, 10000, "active", 1))
	store := NewStore(original)
	next := mustSnapshot(t, document(9000, 10000, "active", 2))
	if err := store.Publish(next); err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("expected immutability error, got %v", err)
	}
	if store.Current() != original {
		t.Fatal("rejected update replaced snapshot")
	}
}
func TestRejectsTrafficDecrease(t *testing.T) {
	store := NewStore(mustSnapshot(t, document(5000, 9000, "active", 1)))
	err := store.Publish(mustSnapshot(t, document(5000, 1000, "active", 2)))
	if err == nil {
		t.Fatal("traffic decrease accepted")
	}
}
func TestPublishesPauseAndRevisionAtomically(t *testing.T) {
	store := NewStore(mustSnapshot(t, document(5000, 10000, "active", 1)))
	next := mustSnapshot(t, document(5000, 10000, "paused", 2))
	if err := store.Publish(next); err != nil {
		t.Fatal(err)
	}
	ds, err := store.Current().Assign("pk_demo", "v123", []string{"checkout"})
	if err != nil || ds[0].Status != "inactive" {
		t.Fatalf("unexpected %+v err=%v", ds, err)
	}
}
func TestConcurrentSnapshotReaders(t *testing.T) {
	store := NewStore(mustSnapshot(t, document(5000, 1000, "active", 1)))
	var wg sync.WaitGroup
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			for j := 0; j < 500; j++ {
				s := store.Current()
				if s == nil {
					t.Error("nil snapshot")
					return
				}
				_, err := s.Assign("pk_demo", fmt.Sprintf("visitor-%d-%d", index, j), []string{"checkout"})
				if err != nil {
					t.Error(err)
					return
				}
			}
		}(i)
	}
	if err := store.Publish(mustSnapshot(t, document(5000, 5000, "active", 2))); err != nil {
		t.Fatal(err)
	}
	wg.Wait()
}
func TestRejectsDuplicateExperimentAndIdentity(t *testing.T) {
	d := document(5000, 10000, "active", 1)
	d.Projects[0].Experiments = append(d.Projects[0].Experiments, d.Projects[0].Experiments[0])
	if _, err := FromDocument(d); err == nil {
		t.Fatal("duplicate experiment accepted")
	}
	d.Projects[0].Experiments[1].Key = "different"
	if _, err := FromDocument(d); err == nil {
		t.Fatal("duplicate assignment identity accepted")
	}
}

func TestRejectsDuplicateProjectIdentity(t *testing.T) {
	d := document(5000, 10000, "active", 1)
	d.Projects = append(d.Projects, Project{ID: d.Projects[0].ID, PublicKey: "different-public-key"})
	if _, err := FromDocument(d); err == nil {
		t.Fatal("same project ID accepted under different keys")
	}
}
