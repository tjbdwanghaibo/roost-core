package session

import (
	"errors"
	"strings"
	"testing"
)

func validRun() Run {
	return Run{ID: "run-1", OwnerID: 7, Kind: "dungeon-3", RequestID: "req-1", StartedAtUnix: 100, DeadlineUnix: 200,
		Resources: []Resource{{Kind: "instance", ID: "i-1"}}, Context: map[string]string{"difficulty": "hard"}}
}

func manyContext(n int) map[string]string {
	out := make(map[string]string, n)
	for i := 0; i < n; i++ {
		out[strings.Repeat("k", i+1)] = "v"
	}
	return out
}

// Run.Validate is the shape gate for every run the session service stores;
// the deadline rule is what keeps held resources from being held forever.
// Each rule is pinned by sentinel and message.
func TestRunValidateRefusesEachBrokenField(t *testing.T) {
	if err := validRun().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		mutate   func(*Run)
		sentinel error
		text     string
	}{
		{"blank id", func(r *Run) { r.ID = " " }, ErrRunInvalid, "id is empty"},
		{"owner not positive", func(r *Run) { r.OwnerID = 0 }, ErrRunInvalid, "owner id must be positive"},
		{"blank kind", func(r *Run) { r.Kind = "" }, ErrRunInvalid, "kind is empty"},
		{"blank request id", func(r *Run) { r.RequestID = "  " }, ErrRequestInvalid, "an idempotency key is required"},
		{"no deadline", func(r *Run) { r.DeadlineUnix = 0 }, ErrRunInvalid, "a run must have a deadline"},
		{"deadline not after start", func(r *Run) { r.DeadlineUnix = r.StartedAtUnix }, ErrRunInvalid, "is not after the start"},
		{"too much context", func(r *Run) { r.Context = manyContext(MaxContextEntries + 1) }, ErrRunInvalid, "context has 65 entries, limit 64"},
		{"too many resources", func(r *Run) {
			r.Resources = nil
			for i := 0; i <= MaxResourceEntries; i++ {
				r.Resources = append(r.Resources, Resource{Kind: "instance", ID: strings.Repeat("i", i+1)})
			}
		}, ErrRunInvalid, "run holds 9 resources, limit 8"},
		{"resource without kind", func(r *Run) { r.Resources = []Resource{{ID: "i-1"}} }, ErrRunInvalid, "resource kind is empty"},
		{"resource without id", func(r *Run) { r.Resources = []Resource{{Kind: "instance"}} }, ErrRunInvalid, "resource id is empty"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := validRun()
			tc.mutate(&r)
			err := r.Validate()
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want %v containing %q", err, tc.sentinel, tc.text)
			}
		})
	}
}

// EnterRequest.validate is the admission gate: without an idempotency key a
// retried enter allocates a second run and a second set of resources.
func TestEnterRequestValidateRefusesEachBrokenField(t *testing.T) {
	good := EnterRequest{Kind: "dungeon-3", RequestID: "req-1"}
	if err := good.validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name     string
		request  EnterRequest
		sentinel error
		text     string
	}{
		{"blank kind", EnterRequest{Kind: " ", RequestID: "req-1"}, ErrRunInvalid, "kind is empty"},
		{"blank request id", EnterRequest{Kind: "dungeon-3"}, ErrRequestInvalid, "an idempotency key is required; without one a retry allocates a second run"},
		{"too much context", EnterRequest{Kind: "dungeon-3", RequestID: "req-1", Context: manyContext(MaxContextEntries + 1)}, ErrRunInvalid, "context has 65 entries, limit 64"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.request.validate()
			if !errors.Is(err, tc.sentinel) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("validate = %v, want %v containing %q", err, tc.sentinel, tc.text)
			}
		})
	}
}
