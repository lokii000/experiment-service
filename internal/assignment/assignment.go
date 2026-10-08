// Package assignment implements deterministic experiment enrollment and variant
// selection. Assignment is a pure function of immutable published configuration
// and an opaque stable visitor ID; it never persists visitor-to-variant mappings.
package assignment

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"strings"
)

const (
	BasisPoints = 10_000
	AlgorithmV1 = "sha256-v1"
)

type Variant struct {
	Key       string `json:"key"`
	WeightBps int    `json:"weight_bps"`
}

type Experiment struct {
	Key          string    `json:"key"`
	AssignmentID string    `json:"assignment_id"`
	Revision     int       `json:"revision"` // metadata; NOT hashed
	Status       string    `json:"status"`   // active or paused
	TrafficBps   int       `json:"traffic_bps"`
	Variants     []Variant `json:"variants"` // canonical published order
}

type Decision struct {
	ExperimentKey string `json:"experiment_key"`
	Status        string `json:"status"` // assigned, not_enrolled, inactive, unknown_experiment
	VariantKey    string `json:"variant_key,omitempty"`
	AssignmentID  string `json:"assignment_id,omitempty"`
}

type compiledVariant struct {
	key   string
	upper int
}

type Compiled struct {
	projectID    string
	key          string
	assignmentID string
	revision     int
	status       string
	trafficBps   int
	variants     []compiledVariant
	published    []Variant
}

// Compile validates an experiment and freezes its ordered variant ranges.
func Compile(projectID string, spec Experiment) (*Compiled, error) {
	if strings.TrimSpace(projectID) == "" || strings.TrimSpace(spec.Key) == "" || strings.TrimSpace(spec.AssignmentID) == "" {
		return nil, errors.New("project ID, experiment key, and assignment ID are required")
	}
	if spec.Revision < 1 {
		return nil, errors.New("revision must be positive")
	}
	if spec.Status != "active" && spec.Status != "paused" {
		return nil, errors.New("status must be active or paused")
	}
	if spec.TrafficBps < 0 || spec.TrafficBps > BasisPoints {
		return nil, fmt.Errorf("traffic must be between 0 and %d basis points", BasisPoints)
	}
	if len(spec.Variants) < 2 || len(spec.Variants) > 20 {
		return nil, errors.New("an experiment requires 2 to 20 variants")
	}
	c := &Compiled{
		projectID:    projectID,
		key:          spec.Key,
		assignmentID: spec.AssignmentID,
		revision:     spec.Revision,
		status:       spec.Status,
		trafficBps:   spec.TrafficBps,
		variants:     make([]compiledVariant, 0, len(spec.Variants)),
		published:    make([]Variant, 0, len(spec.Variants)),
	}
	seen := make(map[string]struct{}, len(spec.Variants))
	cumulative := 0
	for _, v := range spec.Variants {
		if strings.TrimSpace(v.Key) == "" || v.WeightBps <= 0 {
			return nil, errors.New("variant key and positive weight required")
		}
		if _, ok := seen[v.Key]; ok {
			return nil, fmt.Errorf("duplicate variant key %q", v.Key)
		}
		seen[v.Key] = struct{}{}
		cumulative += v.WeightBps
		if cumulative > BasisPoints {
			return nil, errors.New("variant weights exceed 10000")
		}
		c.variants = append(c.variants, compiledVariant{key: v.Key, upper: cumulative})
		c.published = append(c.published, v)
	}
	if cumulative != BasisPoints {
		return nil, fmt.Errorf("variant weights must sum to %d, got %d", BasisPoints, cumulative)
	}
	return c, nil
}

// Bucket uses length-prefixed fields to avoid collisions from ambiguous joins.
// Hash input encoding and truncation are part of the persisted assignment contract.
// Purpose separates enrollment and variant selection into different hash domains.
func Bucket(projectID, assignmentID, visitorID, purpose string) int {
	h := sha256.New()
	for _, s := range []string{AlgorithmV1, purpose, projectID, assignmentID, visitorID} {
		var length [4]byte
		binary.BigEndian.PutUint32(length[:], uint32(len(s)))
		_, _ = h.Write(length[:])
		_, _ = h.Write([]byte(s))
	}
	sum := h.Sum(nil)
	return int(binary.BigEndian.Uint64(sum[:8]) % BasisPoints)
}

// ExpectedVariant verifies historic exposures even after a cohort is paused.
// It never bypasses enrollment, variant ranges, or assignment identity.
// In this MVP, a signed assignment receipt would be required to prove that a
// paused cohort actually issued the assignment before it was paused.
func (c *Compiled) ExpectedVariant(visitorID string) (string, bool) {
	if Bucket(c.projectID, c.assignmentID, visitorID, "enrollment") >= c.trafficBps {
		return "", false
	}
	bucket := Bucket(c.projectID, c.assignmentID, visitorID, "variant")
	for _, v := range c.variants {
		if bucket < v.upper {
			return v.key, true
		}
	}
	return "", false
}
func (c *Compiled) AssignmentID() string { return c.assignmentID }
func (c *Compiled) VariantKeys() []string {
	keys := make([]string, len(c.published))
	for i, v := range c.published {
		keys[i] = v.Key
	}
	return keys
}

// Assign performs no network or storage IO. Unknown experiments are handled by
// the containing configuration Snapshot, not by this function.
func (c *Compiled) Assign(visitorID string) Decision {
	result := Decision{ExperimentKey: c.key, AssignmentID: c.assignmentID}
	if c.status != "active" {
		result.Status = "inactive"
		return result
	}
	if Bucket(c.projectID, c.assignmentID, visitorID, "enrollment") >= c.trafficBps {
		result.Status = "not_enrolled"
		return result
	}
	bucket := Bucket(c.projectID, c.assignmentID, visitorID, "variant")
	for _, v := range c.variants {
		if bucket < v.upper {
			result.Status = "assigned"
			result.VariantKey = v.key
			return result
		}
	}
	// Compile enforces a cumulative upper bound of 10,000, so this is unreachable.
	panic("validated variant ranges must cover all buckets")
}

// Compatible checks the invariants required for sticky assignment when a new
// snapshot is published. It permits status/revision changes and traffic ramp-up,
// but rejects allocation/order changes and traffic reductions.
func (c *Compiled) Compatible(next *Compiled) error {
	if c.projectID != next.projectID || c.key != next.key || c.assignmentID != next.assignmentID {
		return errors.New("published assignment identity cannot change")
	}
	if next.revision < c.revision {
		return errors.New("configuration revision cannot decrease")
	}
	if next.revision == c.revision && (next.status != c.status || next.trafficBps != c.trafficBps) {
		return errors.New("status or traffic change requires a new configuration revision")
	}
	if next.trafficBps < c.trafficBps {
		return errors.New("traffic cannot decrease in an existing cohort")
	}
	if len(c.published) != len(next.published) {
		return errors.New("published variants are immutable")
	}
	for i := range c.published {
		if c.published[i] != next.published[i] {
			return errors.New("variant identity, order and weights are immutable")
		}
	}
	return nil
}
