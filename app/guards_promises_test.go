package app

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/lifecycle"
)

// U-0136 · C2（空洞测试）· nightly gap map core `app` 9/12。
//
// 反向停机在上下文已结束时要立刻带着 ctx 错误返回、不再去停任何 Mod；生命周期
// 事件在没有 registry 或 lifecycle 能力类型不对时报错而不是解引用；Mod 排序拒绝
// nil 条目、空名字、重名；配置校验拒绝 nil viper；能力批量注册对空名字与批内重名
// 整批拒绝、不发布任何一项。

// enteredStopMod reports the moment any stop entry point is entered; the
// stop itself runs on a goroutine, so a flag read right after the call
// could miss it — the test waits instead.
type enteredStopMod struct {
	entered chan struct{}
	once    sync.Once
}

func (m *enteredStopMod) Name() ModName           { return "entered_stop" }
func (m *enteredStopMod) Init(*viper.Viper) error { return nil }
func (m *enteredStopMod) Provide(*Registry) error { return nil }
func (m *enteredStopMod) Start() error            { return nil }
func (m *enteredStopMod) Stop()                   { m.once.Do(func() { close(m.entered) }) }
func (m *enteredStopMod) StopWithContext(context.Context) error {
	m.Stop()
	return nil
}

func TestStopModsReverseStopsNothingOnceTheContextIsDone(t *testing.T) {
	mod := &enteredStopMod{entered: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := stopModsReverseWithContext(ctx, []Mod{mod}, "test stop")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("stop with a finished context = %v, want context.Canceled", err)
	}
	select {
	case <-mod.entered:
		t.Fatal("a mod was stopped after the stop context had already ended")
	case <-time.After(50 * time.Millisecond):
	}
}

func TestEmitLifecycleRequiresARegistryWithTheLifecycleCapability(t *testing.T) {
	ctx := context.Background()
	event := lifecycle.Event{Phase: lifecycle.PhaseServiceStarted, Service: "game"}
	var none *App
	if err := none.emitLifecycle(ctx, event); err == nil || !strings.Contains(err.Error(), "lifecycle registry unavailable") {
		t.Fatalf("emitLifecycle on a nil app = %v", err)
	}
	if err := (&App{}).emitLifecycle(ctx, event); err == nil || !strings.Contains(err.Error(), "lifecycle registry unavailable") {
		t.Fatalf("emitLifecycle without a registry = %v", err)
	}
	wrongType := &App{registry: &Registry{store: map[ModName]any{ModLifecycle: "not a lifecycle registry"}}}
	if err := wrongType.emitLifecycle(ctx, event); err == nil || !strings.Contains(err.Error(), "not found or wrong type") {
		t.Fatalf("emitLifecycle with a wrong-typed capability = %v", err)
	}
	missing := &App{registry: &Registry{store: map[ModName]any{}}}
	if err := missing.emitLifecycle(ctx, event); err == nil || !strings.Contains(err.Error(), "not found or wrong type") {
		t.Fatalf("emitLifecycle without the capability = %v", err)
	}
	if err := (&App{registry: NewRegistry(viper.New())}).emitLifecycle(ctx, event); err != nil {
		t.Fatalf("emitLifecycle with a real registry = %v", err)
	}
}

func TestSortModsRefusesNilUnnamedAndDuplicateMods(t *testing.T) {
	a := &orderedTestMod{name: "a"}
	if _, err := sortMods([]Mod{a, nil}, nil); err == nil || !strings.Contains(err.Error(), "mod entry 1 is nil") {
		t.Fatalf("sortMods with a nil entry = %v", err)
	}
	if _, err := sortMods([]Mod{a, &orderedTestMod{}}, nil); err == nil || !strings.Contains(err.Error(), "mod entry 1 has empty name") {
		t.Fatalf("sortMods with an unnamed mod = %v", err)
	}
	if _, err := sortMods([]Mod{a, &orderedTestMod{name: "a"}}, nil); err == nil || !strings.Contains(err.Error(), `duplicate mod "a"`) {
		t.Fatalf("sortMods with a duplicate name = %v", err)
	}
	if got, err := sortMods([]Mod{a, &orderedTestMod{name: "b", hard: []ModName{"a"}}}, nil); err != nil || len(got) != 2 {
		t.Fatalf("sortMods with a valid pair = (%v, %v)", modNames(got), err)
	}
}

func TestValidateServiceConfigAndRegisterBatchRefuseMissingInputs(t *testing.T) {
	if err := ValidateServiceConfig(nil); err == nil || !strings.Contains(err.Error(), "viper is nil") {
		t.Fatalf("ValidateServiceConfig(nil) = %v", err)
	}
	registry := NewRegistry(viper.New())
	if err := registry.RegisterBatch(Capability{Name: "first", Value: 1}, Capability{Name: "", Value: 2}); err == nil || !strings.Contains(err.Error(), "capability name is empty") {
		t.Fatalf("RegisterBatch with an unnamed capability = %v", err)
	}
	if err := registry.RegisterBatch(Capability{Name: "first", Value: 1}, Capability{Name: "first", Value: 2}); err == nil || !strings.Contains(err.Error(), "appears more than once in batch") {
		t.Fatalf("RegisterBatch with a duplicate in the batch = %v", err)
	}
	if _, ok := registry.Get("first"); ok {
		t.Fatal("a refused batch published one of its capabilities")
	}
}
