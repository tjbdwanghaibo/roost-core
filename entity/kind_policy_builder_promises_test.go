package entity

import (
	"strings"
	"sync"
	"testing"
)

// RR-20260926-71：kind 的 Remote 策略以注册表为准（GetEntityKindRemotePolicy），手写 builder 省略 RemotePolicy 时
// 构建类型校验与默认生命周期也要按 kind 的实际策略，与 RR-20260926-60 一致。

const (
	kindDefManagedPlainKind    EntityKind = 211 // kind 定义 managed；builder 省略策略，实体不实现 IThreadSafeRemoteEntity
	kindDefManagedLifetimeKind EntityKind = 212 // kind 定义 managed；builder 省略策略与生命周期
	kindDefMirrorLifetimeKind  EntityKind = 213 // kind 定义 mirror；builder 省略策略与生命周期
	lateManagedKind            EntityKind = 214 // builder 先注册（none、默认生命周期），kind 定义随后声明 managed
)

// 注册表是进程级的：每个 kind 只注册一次，-count>1 重复运行时沿用第一次的注册。
var kindPolicyBuilderOnce [4]sync.Once

func registerOnce(slot int, register func()) { kindPolicyBuilderOnce[slot].Do(register) }

func plainKindBuilder(kind EntityKind) *EntityBuilderParam {
	return &EntityBuilderParam{
		Category: 1, Kind: kind, NoPersist: true,
		Builder: func(param *EntityCreateParam) (IThreadSafeEntity, error) {
			return &testEntity{EntityBase: NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
		},
	}
}

// REPRO-2026-09-26-06 §7 第二个探针改写（entity_factory.go validateBuiltEntityPolicy）：修前构建成功。
func TestBuiltEntityPolicyUsesKindPolicy(t *testing.T) {
	registerOnce(0, func() {
		MustRegisterEntityKindDefs(EntityKindDef{Kind: kindDefManagedPlainKind, Category: 1, RemotePolicy: RemotePolicyManaged})
		RegisterEntityBuilder(plainKindBuilder(kindDefManagedPlainKind))
	})
	id, err := BuildEntityID(9973, kindDefManagedPlainKind)
	if err != nil {
		t.Fatal(err)
	}
	e, err := BuildEntity(&EntityCreateParam{IsCreate: true, Category: 1, Kind: kindDefManagedPlainKind, Id: id})
	if err == nil {
		t.Fatalf("kind %d is remote-managed in the registry, but %T (not IThreadSafeRemoteEntity) was built without error", kindDefManagedPlainKind, e)
	}
	if !strings.Contains(err.Error(), "does not implement IThreadSafeRemoteEntity") {
		t.Fatalf("err=%v, want the remote=managed type check", err)
	}
}

// entity_factory.go normalizeBuilderPolicy 的默认生命周期同源：修前按 builder 字段（none）得到 ephemeral / persisted。
func TestBuilderDefaultLifetimeFollowsKindPolicy(t *testing.T) {
	for i, tc := range []struct {
		kind   EntityKind
		policy RemotePolicy
		want   EntityLifetime
	}{
		{kindDefManagedLifetimeKind, RemotePolicyManaged, EntityLifetimeRemoteManaged},
		{kindDefMirrorLifetimeKind, RemotePolicyMirror, EntityLifetimeMirrorCache},
	} {
		registerOnce(1+i, func() {
			MustRegisterEntityKindDefs(EntityKindDef{Kind: tc.kind, Category: 1, RemotePolicy: tc.policy})
			RegisterEntityBuilder(plainKindBuilder(tc.kind))
		})
		if got := GetEntityBuilderParam(tc.kind).Lifetime; got != tc.want {
			t.Errorf("kind %d (registry policy %d, builder omits it): default lifetime=%d, want %d", tc.kind, tc.policy, got, tc.want)
		}
	}
}

// 反方向的注册顺序：builder 先按 none 注册并取得默认生命周期，kind 定义随后把策略升级为 managed。
// 生命周期与新策略矛盾，注册必须被拒绝（修前接受，kind 变成 managed 而 builder 的生命周期仍是 none 时的默认值）。
func TestLateKindPolicyUpgradeConflictingWithBuilderIsRefused(t *testing.T) {
	registerOnce(3, func() { RegisterEntityBuilder(plainKindBuilder(lateManagedKind)) })
	err := RegisterEntityKindDefs(EntityKindDef{Kind: lateManagedKind, Category: 1, RemotePolicy: RemotePolicyManaged})
	if err == nil {
		t.Fatalf("kind definition upgraded kind %d to managed after its builder was registered with lifetime %d", lateManagedKind, GetEntityBuilderParam(lateManagedKind).Lifetime)
	}
	if got := GetEntityKindRemotePolicy(lateManagedKind); got != RemotePolicyNone {
		t.Fatalf("policy after the refused upgrade = %d, want none", got)
	}
}
