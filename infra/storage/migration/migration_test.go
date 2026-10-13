package migration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

type testData struct {
	version int32
	value   int
}

func (d *testData) DataVersion() int32     { return d.version }
func (d *testData) SetDataVersion(v int32) { d.version = v }

func TestRegistryRun(t *testing.T) {
	reg := NewRegistry()
	reg.MustRegister(Step{From: 0, To: 1, Apply: func(_ context.Context, data any) error {
		data.(*testData).value += 10
		return nil
	}})
	reg.MustRegister(Step{From: 1, To: 2, Apply: func(_ context.Context, data any) error {
		data.(*testData).value *= 2
		return nil
	}})
	data := &testData{}
	if err := reg.Run(context.Background(), data, 2); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if data.version != 2 || data.value != 20 {
		t.Fatalf("data = %+v", data)
	}
}

type versionedValue struct{ version int32 }

func (v *versionedValue) DataVersion() int32     { return v.version }
func (v *versionedValue) SetDataVersion(n int32) { v.version = n }

// 通用业务版本步骤仍支持显式执行；nil 注册表和 nil 数据必须拒绝。
func TestRegistriesRefuseNilReceiversAndNilData(t *testing.T) {
	ctx := context.Background()
	var none *Registry
	if err := none.Register(Step{Name: "s", From: 1, To: 2}); !errors.Is(err, ErrStepInvalid) || !strings.Contains(err.Error(), "registry nil") {
		t.Fatalf("Register on a nil registry = %v", err)
	}
	if err := none.RunFrom(ctx, &versionedValue{}, 1, 2); !errors.Is(err, ErrStepInvalid) || !strings.Contains(err.Error(), "registry nil") {
		t.Fatalf("RunFrom on a nil registry = %v", err)
	}
	if err := NewRegistry().RunFrom(ctx, nil, 1, 2); !errors.Is(err, ErrStepInvalid) || !strings.Contains(err.Error(), "data nil") {
		t.Fatalf("RunFrom with nil data = %v", err)
	}
}

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
