// 管理命令：注册与执行、替换、handler panic 恢复、元数据排序、非法定义与 nil 守卫、执行审计不带载荷。

package admin

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
)

func TestRegistryExecute(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(CommandDef{
		Name: "echo",
		Handler: func(_ context.Context, cmd Command) (Result, error) {
			payload, err := DecodePayload[map[string]string](cmd)
			if err != nil {
				return Result{}, err
			}
			return Result{Data: map[string]any{"value": payload["value"]}}, nil
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}
	result, err := reg.Execute(context.Background(), Command{
		Name:    "echo",
		TraceID: "t1",
		Payload: MustPayload(map[string]string{"value": "ok"}),
	})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if !result.OK || result.TraceID != "t1" || result.Data["value"] != "ok" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRegistryRegisterReplacesExistingCommand(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(CommandDef{
		Name: "echo",
		Handler: func(context.Context, Command) (Result, error) {
			return Result{Data: map[string]any{"value": "old"}}, nil
		},
	}); err != nil {
		t.Fatalf("Register old: %v", err)
	}
	if err := reg.Register(CommandDef{
		Name: "echo",
		Handler: func(context.Context, Command) (Result, error) {
			return Result{Data: map[string]any{"value": "new"}}, nil
		},
	}); err != nil {
		t.Fatalf("Register new: %v", err)
	}

	result, err := reg.Execute(context.Background(), Command{Name: "echo"})
	if err != nil {
		t.Fatalf("Execute: %v", err)
	}
	if result.Data["value"] != "new" {
		t.Fatalf("result = %+v", result)
	}
}

func TestRegistryExecuteRecoversHandlerPanic(t *testing.T) {
	reg := NewRegistry()
	if err := reg.Register(CommandDef{
		Name: "panic",
		Handler: func(context.Context, Command) (Result, error) {
			panic("boom")
		},
	}); err != nil {
		t.Fatalf("Register: %v", err)
	}

	result, err := reg.Execute(context.Background(), Command{Name: "panic", TraceID: "trace-panic"})
	if err == nil {
		t.Fatal("Execute error = nil, want panic error")
	}
	if !strings.Contains(err.Error(), "panic: boom") {
		t.Fatalf("Execute error = %v, want panic reason", err)
	}
	if result.OK || result.Name != "panic" || result.TraceID != "trace-panic" {
		t.Fatalf("result = %+v, want failed panic result with identity", result)
	}
	if result.StartedAt == 0 || result.EndedAt == 0 || result.EndedAt < result.StartedAt {
		t.Fatalf("result timing invalid: %+v", result)
	}
}

func TestMetadataRegistryRegistersAndListsSortedCommands(t *testing.T) {
	reg := NewMetadataRegistry()
	if err := reg.Register(CommandMeta{
		Name:        "feature_flag.set",
		Title:       "Set Feature Flag",
		Description: "set feature flag",
		TargetScope: []string{"game"},
		Risk:        RiskMedium,
		PayloadSchema: map[string]any{
			"type":     "object",
			"required": []string{"name", "enabled"},
		},
	}); err != nil {
		t.Fatalf("Register feature_flag.set: %v", err)
	}
	if err := reg.Register(CommandMeta{
		Name:        "config.reload",
		Title:       "Reload Config",
		TargetScope: []string{"game"},
		Risk:        RiskMedium,
	}); err != nil {
		t.Fatalf("Register config.reload: %v", err)
	}

	got, ok := reg.Get("feature_flag.set")
	if !ok {
		t.Fatal("feature_flag.set metadata not found")
	}
	if got.Title != "Set Feature Flag" || got.Risk != RiskMedium {
		t.Fatalf("metadata = %+v", got)
	}
	got.TargetScope[0] = "mutated"
	again, _ := reg.Get("feature_flag.set")
	if again.TargetScope[0] != "game" {
		t.Fatalf("metadata was not cloned: %+v", again)
	}

	list := reg.List()
	if len(list) != 2 {
		t.Fatalf("list len = %d, want 2", len(list))
	}
	if list[0].Name != "config.reload" || list[1].Name != "feature_flag.set" {
		t.Fatalf("list not sorted: %+v", list)
	}
}

// nil 注册表不能注册 / 执行；未知命令报 ErrCommandNotFound；nil 元数据注册表不能注册。
func TestRegistriesRefuseNilReceiversAndUnknownCommands(t *testing.T) {
	ctx := context.Background()
	handler := func(context.Context, Command) (Result, error) { return Result{OK: true}, nil }
	var none *Registry
	if err := none.Register(CommandDef{Name: "x", Handler: handler}); !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "registry nil") {
		t.Fatalf("Register on a nil registry = %v", err)
	}
	if _, err := none.Execute(ctx, Command{Name: "x"}); !errors.Is(err, ErrCommandInvalid) {
		t.Fatalf("Execute on a nil registry = %v", err)
	}
	registry := NewRegistry()
	if _, err := registry.Execute(ctx, Command{Name: "missing"}); !errors.Is(err, ErrCommandNotFound) {
		t.Fatalf("Execute of an unknown command = %v", err)
	}
	if err := registry.Register(CommandDef{Name: "x", Handler: handler}); err != nil {
		t.Fatal(err)
	}
	if result, err := registry.Execute(ctx, Command{Name: "x"}); err != nil || !result.OK {
		t.Fatalf("Execute of a registered command = (%+v, %v)", result, err)
	}
	var meta *MetadataRegistry
	if err := meta.Register(CommandMeta{Name: "x"}); !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "metadata registry nil") {
		t.Fatalf("Register on a nil metadata registry = %v", err)
	}
}

// Admin commands are operator-facing entry points; a nameless definition or
// a nameless command would register or execute "nothing" without a word.
func TestAdminRegistriesRefuseNamelessAndHandlerlessCommands(t *testing.T) {
	reg := NewRegistry()
	err := reg.Register(CommandDef{Handler: func(context.Context, Command) (Result, error) { return Result{}, nil }})
	if !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "name required") {
		t.Fatalf("nameless definition = %v", err)
	}
	err = reg.Register(CommandDef{Name: "reload"})
	if !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "handler required for reload") {
		t.Fatalf("handlerless definition = %v", err)
	}
	if _, err := reg.Execute(context.Background(), Command{}); !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "name required") {
		t.Fatalf("nameless command = %v", err)
	}
	meta := NewMetadataRegistry()
	if err := meta.Register(CommandMeta{}); !errors.Is(err, ErrCommandInvalid) || !strings.Contains(err.Error(), "metadata name required") {
		t.Fatalf("nameless metadata = %v", err)
	}
}

func TestAdminExecutionWritesCorrelatedAuditWithoutPayload(t *testing.T) {
	for _, kind := range []string{"ok", "error", "panic", "unknown", "missing"} {
		t.Run(kind, func(t *testing.T) {
			var buf bytes.Buffer
			old := slog.Default()
			slog.SetDefault(slog.New(slog.NewJSONHandler(&buf, nil)))
			defer slog.SetDefault(old)
			r := NewRegistry()
			if kind != "missing" {
				_ = r.Register(CommandDef{Name: "b7", Handler: func(context.Context, Command) (Result, error) {
					switch kind {
					case "error":
						return Result{}, errors.New("failed")
					case "panic":
						panic("failed")
					case "unknown":
						return Result{}, context.DeadlineExceeded
					}
					return Result{}, nil
				}})
			}
			result, _ := r.Execute(context.Background(), Command{Name: "b7", TraceID: "trace-b7", Operator: "operator-b7", Source: "source-b7", Payload: []byte(`{"secret":"do-not-log-b7"}`)})
			got := buf.String()
			for _, want := range []string{"admin command started", "admin command finished", "trace-b7", "operator-b7", "source-b7", "outcome", "duration_ms"} {
				if !strings.Contains(got, want) {
					t.Errorf("audit missing %q: %s", want, got)
				}
			}
			if strings.Contains(got, "do-not-log-b7") {
				t.Fatal("audit leaked payload")
			}
			if result.TraceID != "trace-b7" {
				t.Fatalf("failure loses trace: %+v", result)
			}
		})
	}
}
