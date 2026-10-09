package entity

import (
	"errors"
	"strings"
	"testing"
)

// U-0148 · C2 · gap map core `entity` 6/20：NormalizeID 拒绝 none 种类；未注册的组件类型不能创建；
// 非远端托管实体不接受远端恢复状态；管理器的 id 生成器缺失 / 已有实体后不能换；TryAdd 拒绝 nil
// 实体与无 Base 的实体。
func TestEntityGuardsRefuseNoneKindMissingFactoriesAndLateGeneratorChanges(t *testing.T) {
	if err := (&EntityCreateParam{}).NormalizeID(EntityKindNone); err == nil || !strings.Contains(err.Error(), "kind must not be none") {
		t.Fatalf("NormalizeID with no kind anywhere = %v", err)
	}
	if _, err := CreateComponent(ComponentType(250), nil, &EntityCreateParam{}); err == nil || !strings.Contains(err.Error(), "no component factory registered") {
		t.Fatalf("CreateComponent for an unregistered type = %v", err)
	}
	if _, err := BuildEntity(&EntityCreateParam{IsCreate: true, Kind: testEntityKind, UniqueID: 77, RemoteRestore: &RemoteVersionVector{}}); err == nil || !strings.Contains(err.Error(), "is not remote-managed") {
		t.Fatalf("BuildEntity with remote restore state on a local kind = %v", err)
	}

	var none *EntityManager
	generator := func() (uint64, error) { return 1, nil }
	if err := none.ConfigureIDGenerator(generator); err == nil || !strings.Contains(err.Error(), "id generator is required") {
		t.Fatalf("ConfigureIDGenerator on a nil manager = %v", err)
	}
	manager := NewEntityManager()
	if err := manager.ConfigureIDGenerator(nil); err == nil || !strings.Contains(err.Error(), "id generator is required") {
		t.Fatalf("ConfigureIDGenerator(nil) = %v", err)
	}
	if err := manager.ConfigureIDGenerator(generator); err != nil {
		t.Fatal(err)
	}
	if err := manager.TryAdd(nil); !errors.Is(err, ErrEntityNil) {
		t.Fatalf("TryAdd(nil) = %v", err)
	}
	if err := manager.TryAdd(&mgrTestEntity{}); !errors.Is(err, ErrEntityNil) {
		t.Fatalf("TryAdd(entity without a base) = %v", err)
	}
	if err := manager.TryAdd(newMgrTestEntity(1001, testEntityCategoryPlayer)); err != nil {
		t.Fatal(err)
	}
	if err := manager.ConfigureIDGenerator(generator); err == nil || !strings.Contains(err.Error(), "cannot change after entity publication") {
		t.Fatalf("ConfigureIDGenerator after an entity was published = %v", err)
	}
}
