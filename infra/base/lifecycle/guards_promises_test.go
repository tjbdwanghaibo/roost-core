package lifecycle

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0147 · C2 · nightly gap map core `lifecycle` 7/9：钩子注册要求 registry / 阶段 / 处理器；
// 管理器组只能从 New 进 Init、从 Initialized 进 Start，nil 组拒绝。
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
