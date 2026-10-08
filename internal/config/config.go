package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/lokii000/experiment-service/internal/assignment"
)

type Project struct {
	ID          string                  `json:"id"`
	PublicKey   string                  `json:"public_key"`
	Experiments []assignment.Experiment `json:"experiments"`
}

type Document struct {
	Projects []Project `json:"projects"`
}

type compiledProject struct {
	id          string
	experiments map[string]*assignment.Compiled
}

type Snapshot struct {
	projects map[string]*compiledProject
}

func FromDocument(doc Document) (*Snapshot, error) {
	if len(doc.Projects) == 0 {
		return nil, errors.New("at least one project required")
	}
	result := &Snapshot{projects: make(map[string]*compiledProject, len(doc.Projects))}
	projectIDs := make(map[string]bool, len(doc.Projects))
	for _, p := range doc.Projects {
		if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.PublicKey) == "" {
			return nil, errors.New("project ID and public key required")
		}
		if _, exists := result.projects[p.PublicKey]; exists {
			return nil, fmt.Errorf("duplicate project key %q", p.PublicKey)
		}
		if projectIDs[p.ID] {
			return nil, fmt.Errorf("duplicate project ID %q", p.ID)
		}
		projectIDs[p.ID] = true
		cp := &compiledProject{id: p.ID, experiments: map[string]*assignment.Compiled{}}
		identities := map[string]bool{}
		for _, exp := range p.Experiments {
			if _, exists := cp.experiments[exp.Key]; exists {
				return nil, fmt.Errorf("duplicate experiment %q", exp.Key)
			}
			if identities[exp.AssignmentID] {
				return nil, fmt.Errorf("duplicate assignment identity %q", exp.AssignmentID)
			}
			identities[exp.AssignmentID] = true
			compiled, err := assignment.Compile(p.ID, exp)
			if err != nil {
				return nil, fmt.Errorf("experiment %q: %w", exp.Key, err)
			}
			cp.experiments[exp.Key] = compiled
		}
		result.projects[p.PublicKey] = cp
	}
	return result, nil
}

func LoadFile(path string) (*Snapshot, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc Document
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	return FromDocument(doc)
}

func (s *Snapshot) Assign(projectKey, visitorID string, keys []string) ([]assignment.Decision, error) {
	project, exists := s.projects[projectKey]
	if !exists {
		return nil, errors.New("unknown project")
	}
	out := make([]assignment.Decision, 0, len(keys))
	for _, key := range keys {
		exp, exists := project.experiments[key]
		if !exists {
			out = append(out, assignment.Decision{ExperimentKey: key, Status: "unknown_experiment"})
			continue
		}
		out = append(out, exp.Assign(visitorID))
	}
	return out, nil
}

// Store atomically publishes fully validated snapshots. Its projects/maps are
// private and never mutated after construction. Read requests are lock-free.
type Store struct {
	mu      sync.Mutex
	current atomic.Pointer[Snapshot]
}

func NewStore(first *Snapshot) *Store {
	s := &Store{}
	s.current.Store(first)
	return s
}

func (s *Store) Current() *Snapshot { return s.current.Load() }

// Publish prevents known reassignments during live updates. A DB-backed control
// plane must ALSO persist and enforce these rules across process restarts.
func (s *Store) Publish(next *Snapshot) error {
	if next == nil {
		return errors.New("snapshot cannot be nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.current.Load()
	if old != nil {
		for key, prevProject := range old.projects {
			nextProject, ok := next.projects[key]
			if !ok || prevProject.id != nextProject.id {
				return fmt.Errorf("project %q cannot be removed or reassigned", key)
			}
			for expKey, oldExp := range prevProject.experiments {
				newExp, ok := nextProject.experiments[expKey]
				if !ok {
					return fmt.Errorf("published experiment %q cannot be removed: pause instead", expKey)
				}
				if err := oldExp.Compatible(newExp); err != nil {
					return fmt.Errorf("experiment %q: %w", expKey, err)
				}
			}
		}
	}
	s.current.Store(next)
	return nil
}

// ResolveExposure provides server-side verification against immutable assignment
// identity and published weights. This does not authenticate browser intent.
func (s *Snapshot) ResolveExposure(projectKey, experimentKey, assignmentID, visitorID, variantKey string) (string, bool) {
	project, ok := s.projects[projectKey]
	if !ok {
		return "", false
	}
	exp, ok := project.experiments[experimentKey]
	if !ok || exp.AssignmentID() != assignmentID {
		return "", false
	}
	expected, enrolled := exp.ExpectedVariant(visitorID)
	return project.id, enrolled && expected == variantKey
}
func (s *Snapshot) ResolveCohort(projectKey, experimentKey, assignmentID string) (string, bool) {
	project, ok := s.projects[projectKey]
	if !ok {
		return "", false
	}
	exp, ok := project.experiments[experimentKey]
	if !ok || exp.AssignmentID() != assignmentID {
		return "", false
	}
	return project.id, true
}
func (s *Store) ResolveExposure(projectKey, experimentKey, assignmentID, visitorID, variantKey string) (string, bool) {
	snap := s.Current()
	if snap == nil {
		return "", false
	}
	return snap.ResolveExposure(projectKey, experimentKey, assignmentID, visitorID, variantKey)
}
func (s *Store) ResolveCohort(projectKey, experimentKey, assignmentID string) (string, bool) {
	snap := s.Current()
	if snap == nil {
		return "", false
	}
	return snap.ResolveCohort(projectKey, experimentKey, assignmentID)
}

// ResultsMetadata returns the canonical variant list including variants with
// zero exposures, so reports don't silently omit unsuccessful treatments.
func (s *Snapshot) ResultsMetadata(projectKey, experimentKey string) (string, string, []string, bool) {
	project, ok := s.projects[projectKey]
	if !ok {
		return "", "", nil, false
	}
	exp, ok := project.experiments[experimentKey]
	if !ok {
		return "", "", nil, false
	}
	variants := make([]string, len(exp.VariantKeys()))
	copy(variants, exp.VariantKeys())
	return project.id, exp.AssignmentID(), variants, true
}
