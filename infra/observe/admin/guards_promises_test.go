package admin

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// U-0147 · C2 · nightly gap map core `admin` 4/8：nil 注册表不能注册 / 执行；未知命令报
// ErrCommandNotFound；nil 元数据注册表不能注册。
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
