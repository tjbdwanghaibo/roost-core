package account

// 承诺（game-demo 接入）：实现了 RegistryBound 的 collaborator 在 Mod.Provide 里拿到
// account 服务进程的 registry，且拿到的时刻早于 Mod 自己去查 Redis；Bind 返回错误则
// Provide 失败并点名是哪一个 collaborator。
//
// 旧行为：collaborator 由 bootstrap 在 app 存在之前构造并传给 NewMod，之后再没有任何
// 入口能拿到进程里的能力。PlayerIDAllocator 的契约要求"计数器必须持久且共享"，
// 却没有一条路能拿到持久共享的东西——一个只能用时间戳或进程内计数凑数的接口。

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

type boundAllocator struct {
	durableAllocator
	got  *app.Registry
	fail error
}

func (a *boundAllocator) BindRegistry(r *app.Registry) error {
	a.got = r
	return a.fail
}

func TestProvideBindsRegistryAwareCollaborators(t *testing.T) {
	allocator := &boundAllocator{}
	mod := NewMod(acceptingVerifier(), allocator, simpleNameRules(), nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	registry := app.NewRegistry(viper.New())
	// Provide fails afterwards for want of Redis; binding must already have
	// happened, because a bound collaborator is what looks Redis up itself.
	_ = mod.Provide(registry)
	if allocator.got != registry {
		t.Fatalf("the allocator was not handed the registry (got %p, want %p)", allocator.got, registry)
	}
}

func TestProvideReportsACollaboratorThatRefusesToBind(t *testing.T) {
	allocator := &boundAllocator{fail: errors.New("counter store missing")}
	mod := NewMod(acceptingVerifier(), allocator, simpleNameRules(), nil)
	if err := mod.Init(modConfig()); err != nil {
		t.Fatal(err)
	}
	err := mod.Provide(app.NewRegistry(viper.New()))
	if err == nil {
		t.Fatal("Provide succeeded although the allocator refused to bind")
	}
	for _, want := range []string{"player id allocator", "counter store missing"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not carry %q", err, want)
		}
	}
}
