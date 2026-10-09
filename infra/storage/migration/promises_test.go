package migration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type versionedDoc struct{ version int32 }

func (d *versionedDoc) DataVersion() int32     { return d.version }
func (d *versionedDoc) SetDataVersion(v int32) { d.version = v }

func noopStep(from, to int32) Step {
	return Step{From: from, To: to, Apply: func(context.Context, any) error { return nil }}
}

func expectMigrationErr(t *testing.T, err error, sentinel error, text string) {
	t.Helper()
	if err == nil || !errors.Is(err, sentinel) || !strings.Contains(err.Error(), text) {
		t.Fatalf("error = %v, want %v containing %q", err, sentinel, text)
	}
}

// A migration path is planned from registered steps. Every way a step or a
// request can make the plan ambiguous or lossy is refused; only the happy
// path had a test. A silently accepted downgrade or overshoot would rewrite a
// document's version without running the code that makes it true.
func TestRegistryRefusesEachInvalidStepAndRequest(t *testing.T) {
	r := NewRegistry()
	expectMigrationErr(t, r.Register(noopStep(-1, 1)), ErrStepInvalid, "invalid version -1 -> 1")
	expectMigrationErr(t, r.Register(noopStep(2, 2)), ErrStepInvalid, "invalid version 2 -> 2")
	expectMigrationErr(t, r.Register(noopStep(3, 1)), ErrStepInvalid, "invalid version 3 -> 1")
	expectMigrationErr(t, r.Register(Step{From: 1, To: 2}), ErrStepInvalid, "apply nil 1 -> 2")
	if err := r.Register(noopStep(1, 2)); err != nil {
		t.Fatal(err)
	}
	expectMigrationErr(t, r.Register(noopStep(1, 3)), ErrStepInvalid, "duplicate from 1")
	if err := r.Register(noopStep(2, 4)); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	expectMigrationErr(t, r.RunFrom(ctx, &versionedDoc{version: 4}, 4, 2), ErrStepInvalid, "downgrade 4 -> 2")
	expectMigrationErr(t, r.RunFrom(ctx, &versionedDoc{version: 1}, 1, 3), ErrPathMissing, "step 2_to_4 overshoots target 3")
	expectMigrationErr(t, r.RunFrom(ctx, &versionedDoc{version: 4}, 4, 5), ErrPathMissing, "4 -> 5")
	expectMigrationErr(t, r.Run(ctx, nil, 2), ErrStepInvalid, "data nil")
	doc := &versionedDoc{version: 1}
	if err := r.Run(ctx, doc, 4); err != nil || doc.version != 4 {
		t.Fatalf("legal path = %v, version=%d", err, doc.version)
	}
	// An overshoot refusal leaves the version where the last complete step put it.
	partial := &versionedDoc{version: 1}
	_ = r.RunFrom(ctx, partial, 1, 3)
	if partial.version != 2 {
		t.Fatalf("version after refused overshoot = %d, want 2 (only the first step ran)", partial.version)
	}
}
