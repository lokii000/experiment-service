package assignment

import (
	"fmt"
	"testing"
)

func spec(weights ...int) Experiment {
	variants := make([]Variant, 0, len(weights))
	for i, w := range weights {
		variants = append(variants, Variant{Key: fmt.Sprintf("v%d", i), WeightBps: w})
	}
	return Experiment{Key: "checkout", AssignmentID: "cohort-checkout-1", Revision: 1, Status: "active", TrafficBps: 10000, Variants: variants}
}
func mustCompile(t *testing.T, e Experiment) *Compiled {
	t.Helper()
	compiled, err := Compile("project-1", e)
	if err != nil {
		t.Fatal(err)
	}
	return compiled
}
func TestStableAcrossInstances(t *testing.T) {
	a := mustCompile(t, spec(5000, 5000))
	b := mustCompile(t, spec(5000, 5000))
	for i := 0; i < 1000; i++ {
		visitor := fmt.Sprintf("user-%d", i)
		want := a.Assign(visitor)
		if got := b.Assign(visitor); got != want {
			t.Fatalf("visitor %q got %+v want %+v", visitor, got, want)
		}
		for j := 0; j < 3; j++ {
			if got := a.Assign(visitor); got != want {
				t.Fatal("not deterministic")
			}
		}
	}
}
func TestBucketEncodingNotAmbiguous(t *testing.T) {
	first := Bucket("ab", "c", "d", "enrollment")
	second := Bucket("a", "bc", "d", "enrollment")
	// Equality would be extremely unlikely for independent inputs, but avoid
	// relying on it: instead confirm the hash domains are distinct in a sample.
	different := 0
	for i := 0; i < 100; i++ {
		id := fmt.Sprintf("user-%d", i)
		if Bucket("ab", "c", id, "enrollment") != Bucket("a", "bc", id, "enrollment") {
			different++
		}
	}
	if different < 95 {
		t.Fatalf("suspicious input encoding: %d differ, sample %d/%d", different, first, second)
	}
}
func TestVariantBoundarySelection(t *testing.T) {
	c := mustCompile(t, spec(5000, 3000, 2000))
	tests := []struct {
		bucket int
		key    string
	}{{0, "v0"}, {4999, "v0"}, {5000, "v1"}, {7999, "v1"}, {8000, "v2"}, {9999, "v2"}}
	for _, tt := range tests {
		got := ""
		for _, v := range c.variants {
			if tt.bucket < v.upper {
				got = v.key
				break
			}
		}
		if got != tt.key {
			t.Errorf("bucket %d got %q want %q", tt.bucket, got, tt.key)
		}
	}
}
func TestDistribution(t *testing.T) {
	cases := []struct {
		name    string
		weights []int
		enroll  int
	}{
		{"50-50", []int{5000, 5000}, 10000},
		{"90-10", []int{9000, 1000}, 10000},
		{"three variants", []int{2000, 3000, 5000}, 10000},
		{"25 pct enrollment", []int{5000, 5000}, 2500},
	}
	const population = 100_000
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			conf := spec(tt.weights...)
			conf.TrafficBps = tt.enroll
			c := mustCompile(t, conf)
			counts := make([]int, len(tt.weights))
			enrolled := 0
			for i := 0; i < population; i++ {
				d := c.Assign(fmt.Sprintf("visitor-%d", i))
				if d.Status == "not_enrolled" {
					continue
				}
				if d.Status != "assigned" {
					t.Fatalf("unexpected %+v", d)
				}
				enrolled++
				var index int
				if _, err := fmt.Sscanf(d.VariantKey, "v%d", &index); err != nil {
					t.Fatal(err)
				}
				counts[index]++
			}
			actualEnrollment := float64(enrolled) / population
			expectedEnrollment := float64(tt.enroll) / BasisPoints
			if delta := actualEnrollment - expectedEnrollment; delta < -0.01 || delta > 0.01 {
				t.Errorf("enrollment %f, want ~%f", actualEnrollment, expectedEnrollment)
			}
			for i, w := range tt.weights {
				got := float64(counts[i]) / float64(enrolled)
				want := float64(w) / BasisPoints
				if delta := got - want; delta < -0.01 || delta > 0.01 {
					t.Errorf("variant %d: got %f want ~%f", i, got, want)
				}
			}
		})
	}
}
func TestEnrollmentEdgeCases(t *testing.T) {
	s := spec(5000, 5000)
	s.TrafficBps = 0
	none := mustCompile(t, s)
	s.TrafficBps = BasisPoints
	all := mustCompile(t, s)
	for i := 0; i < 1000; i++ {
		v := fmt.Sprint("v", i)
		if none.Assign(v).Status != "not_enrolled" {
			t.Fatal("zero traffic enrolled a visitor")
		}
		if all.Assign(v).Status != "assigned" {
			t.Fatal("full traffic excluded a visitor")
		}
	}
}
func TestRevisionDoesNotRebucket(t *testing.T) {
	s := spec(5000, 5000)
	old := mustCompile(t, s)
	s.Revision = 27
	next := mustCompile(t, s)
	if err := old.Compatible(next); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 1000; i++ {
		id := fmt.Sprint("v", i)
		if old.Assign(id).VariantKey != next.Assign(id).VariantKey {
			t.Fatalf("revision changed assignment: %s", id)
		}
	}
}
func TestRampUpRetainsPriorAssignments(t *testing.T) {
	s := spec(5000, 5000)
	s.TrafficBps = 1000
	before := mustCompile(t, s)
	s.TrafficBps = 4000
	s.Revision = 2
	after := mustCompile(t, s)
	if err := before.Compatible(after); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3000; i++ {
		id := fmt.Sprint("v", i)
		d := before.Assign(id)
		if d.Status == "assigned" {
			next := after.Assign(id)
			if next.Status != "assigned" || next.VariantKey != d.VariantKey {
				t.Fatal("ramp up changed existing assignment")
			}
		}
	}
}
func TestBadSpecsRejected(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Experiment)
	}{
		{"negative traffic", func(e *Experiment) { e.TrafficBps = -1 }},
		{"traffic over cap", func(e *Experiment) { e.TrafficBps = 10001 }},
		{"weights under", func(e *Experiment) { e.Variants[0].WeightBps = 4999 }},
		{"weights over", func(e *Experiment) { e.Variants[0].WeightBps = 5001 }},
		{"duplicate variant", func(e *Experiment) { e.Variants[1].Key = e.Variants[0].Key }},
		{"duplicate zero weight", func(e *Experiment) { e.Variants[1].WeightBps = 0 }},
		{"missing ID", func(e *Experiment) { e.AssignmentID = "" }},
		{"bad status", func(e *Experiment) { e.Status = "retired" }},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			s := spec(5000, 5000)
			tt.mutate(&s)
			if _, err := Compile("project-1", s); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
}

// Stable golden vectors were computed independently from the documented
// length-prefixed SHA256-v1 wire format. They catch accidental hash changes.
func TestBucketGoldenVectors(t *testing.T) {
	tests := []struct {
		visitor    string
		enrollment int
		variant    int
	}{
		{"visitor-123", 4588, 4007},
		{"user-123", 7381, 2232},
		{"user-999", 996, 1440},
	}
	for _, tt := range tests {
		for _, item := range []struct {
			purpose  string
			expected int
		}{
			{"enrollment", tt.enrollment}, {"variant", tt.variant},
		} {
			if got := Bucket("demo-project-001", "cohort-checkout-button-001", tt.visitor, item.purpose); got != item.expected {
				t.Errorf("%s/%s: got %d expected %d", tt.visitor, item.purpose, got, item.expected)
			}
		}
	}
}

func TestChangedTrafficNeedsNewRevision(t *testing.T) {
	before := spec(5000, 5000)
	before.TrafficBps = 1000
	after := before
	after.TrafficBps = 2000
	old := mustCompile(t, before)
	next := mustCompile(t, after)
	if err := old.Compatible(next); err == nil {
		t.Fatal("revision must advance on traffic change")
	}
}
