package migration

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
