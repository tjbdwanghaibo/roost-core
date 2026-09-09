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

// U-0149 · C2 · gap map core `migration` 5/20：nil 注册表不能注册 / 运行；RunFrom 拒绝 nil 数据；nil
// DAO 注册表不能注册 / 迁移。
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
	var noDAO *DAORegistry
	if err := noDAO.RegisterDAO(DAOStep{Name: "s", Collection: "c", From: 1, To: 2}); !errors.Is(err, ErrStepInvalid) || !strings.Contains(err.Error(), "dao registry nil") {
		t.Fatalf("RegisterDAO on a nil registry = %v", err)
	}
	if _, from, err := noDAO.MigrateDAO(ctx, "c", []byte("{}"), 1, 2); !errors.Is(err, ErrStepInvalid) || from != 1 {
		t.Fatalf("MigrateDAO on a nil registry = (%d, %v)", from, err)
	}
}
