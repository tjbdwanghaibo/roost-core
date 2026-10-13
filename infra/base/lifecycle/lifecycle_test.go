package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
)

func TestEmitOrder(t *testing.T) {
	reg := NewRegistry()
	var got []int
	_ = reg.Register(Hook{Phase: PhaseAppInit, Order: 2, Handler: func(context.Context, Event) error {
		got = append(got, 2)
		return nil
	}})
	_ = reg.Register(Hook{Phase: PhaseAppInit, Order: 1, Handler: func(context.Context, Event) error {
		got = append(got, 1)
		return nil
	}})
	if err := reg.Emit(context.Background(), Event{Phase: PhaseAppInit}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Fatalf("order = %v", got)
	}
}

func TestEmitAllContinuesAfterFailure(t *testing.T) {
	reg := NewRegistry()
	cause := errors.New("cleanup failed")
	called := false
	_ = reg.Register(Hook{Name: "bad", Phase: PhaseServiceStopping, Order: 1, Handler: func(context.Context, Event) error { return cause }})
	_ = reg.Register(Hook{Name: "later", Phase: PhaseServiceStopping, Order: 2, Handler: func(context.Context, Event) error { called = true; return nil }})
	err := reg.EmitAll(context.Background(), Event{Phase: PhaseServiceStopping})
	if !errors.Is(err, cause) || !called {
		t.Fatalf("EmitAll err=%v later_called=%v", err, called)
	}
}

func TestRegisterReplacesSamePhaseAndName(t *testing.T) {
	reg := NewRegistry()
	var got []string
	if err := reg.Register(Hook{Name: "ready", Phase: PhaseServiceStarted, Order: 1, Handler: func(context.Context, Event) error {
		got = append(got, "old")
		return nil
	}}); err != nil {
		t.Fatalf("Register old: %v", err)
	}
	if err := reg.Register(Hook{Name: "ready", Phase: PhaseServiceStarted, Order: 2, Handler: func(context.Context, Event) error {
		got = append(got, "new")
		return nil
	}}); err != nil {
		t.Fatalf("Register new: %v", err)
	}

	if err := reg.Emit(context.Background(), Event{Phase: PhaseServiceStarted}); err != nil {
		t.Fatalf("Emit: %v", err)
	}
	if len(got) != 1 || got[0] != "new" {
		t.Fatalf("hooks executed = %v, want only replacement", got)
	}
}

func TestEmitRecoversHookPanic(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(Hook{Name: "panic-hook", Phase: PhaseServiceStarted, Handler: func(context.Context, Event) error {
		panic("boom")
	}}); err != nil {
		t.Fatalf("Register panic hook: %v", err)
	}

	err := reg.Emit(context.Background(), Event{Phase: PhaseServiceStarted})
	if err == nil {
		t.Fatal("Emit error = nil, want panic error")
	}
	if !strings.Contains(err.Error(), "service.started/panic-hook") || !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("Emit error = %v, want phase/name panic context", err)
	}
}

// EmitAllWatched 在每个 hook 开始前报出它的名字（按 Order），前一个失败也不跳过后面的报告。
func TestEmitAllWatchedReportsEachHookBeforeItRuns(t *testing.T) {
	reg := NewRegistry()
	var events []string
	_ = reg.Register(Hook{Name: "second", Phase: PhaseServiceStopping, Order: 2, Handler: func(context.Context, Event) error {
		events = append(events, "run second")
		return nil
	}})
	_ = reg.Register(Hook{Name: "first", Phase: PhaseServiceStopping, Order: 1, Handler: func(context.Context, Event) error {
		events = append(events, "run first")
		return errors.New("first failed")
	}})
	err := reg.EmitAllWatched(context.Background(), Event{Phase: PhaseServiceStopping}, func(hook string) {
		events = append(events, "start "+hook)
	})
	if err == nil || !strings.Contains(err.Error(), "first failed") {
		t.Fatalf("EmitAllWatched = %v, want the first hook's error", err)
	}
	if got, want := strings.Join(events, ","), "start first,run first,start second,run second"; got != want {
		t.Fatalf("events = %s, want %s", got, want)
	}
}

// 钩子注册要求 registry / 阶段 / 处理器；管理器组只能从 New 进 Init、从 Initialized 进 Start，nil 组拒绝。
func TestRegistryAndManagerGroupRefuseInvalidRegistrationsAndTransitions(t *testing.T) {
	handler := func(context.Context, Event) error { return nil }
	var none *Registry
	if err := none.Register(Hook{Phase: PhaseServiceStarted, Handler: handler}); err == nil || !strings.Contains(err.Error(), "registry nil") {
		t.Fatalf("Register on a nil registry = %v", err)
	}
	registry := NewRegistry()
	if err := registry.Register(Hook{Handler: handler}); err == nil || !strings.Contains(err.Error(), "phase required") {
		t.Fatalf("Register without a phase = %v", err)
	}
	if err := registry.Register(Hook{Phase: PhaseServiceStarted}); err == nil || !strings.Contains(err.Error(), "handler required") {
		t.Fatalf("Register without a handler = %v", err)
	}
	if err := registry.Register(Hook{Name: "ok", Phase: PhaseServiceStarted, Handler: handler}); err != nil {
		t.Fatalf("valid Register = %v", err)
	}

	var noGroup *ManagerGroup[string, string]
	if err := noGroup.Init("ctx", "reason"); !errors.Is(err, ErrManagerGroupState) || !strings.Contains(err.Error(), "group is nil") {
		t.Fatalf("Init on a nil group = %v", err)
	}
	if err := noGroup.Start("reason"); !errors.Is(err, ErrManagerGroupState) || !strings.Contains(err.Error(), "group is nil") {
		t.Fatalf("Start on a nil group = %v", err)
	}
	events := []string{}
	group, err := NewManagerGroup[string, string](&groupTestManager{name: "one", events: &events})
	if err != nil {
		t.Fatal(err)
	}
	if err := group.Start("reason"); !errors.Is(err, ErrManagerGroupState) || !strings.Contains(err.Error(), "start from") {
		t.Fatalf("Start before Init = %v", err)
	}
	if err := group.Init("ctx", "reason"); err != nil {
		t.Fatal(err)
	}
	if err := group.Init("ctx", "reason"); !errors.Is(err, ErrManagerGroupState) || !strings.Contains(err.Error(), "init from") {
		t.Fatalf("second Init = %v", err)
	}
	if err := group.Start("reason"); err != nil {
		t.Fatal(err)
	}
	if err := group.Start("reason"); !errors.Is(err, ErrManagerGroupState) || !strings.Contains(err.Error(), "start from") {
		t.Fatalf("second Start = %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("refused transitions ran managers: %v", events)
	}
}
