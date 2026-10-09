package entity

import (
	"errors"
	"strings"
	"testing"
)

// RR-20260930-15（REMAINING §3 N29，维护者 2026-09-30 拍板“契约 + 启动期校验”）：实体实现必须是指针。Guard 的 holding 按实例
// 比较接口值（current == ent，RR-20260926-67），值类型且不可比较的实现（含 func / map / slice 字段）会在运行期 panic
// （"comparing uncomparable type"）；热路径的比较不改，改为在实体进入框架的两个入口——注册的 builder 构建出实体
// （BuildEntity）与实体加入 EntityManager（TryAdd / Add）——用 reflect 校验一次：不是指针的实现直接拒绝并点名类型。
// 旧行为：两个入口都不检查，值类型实体照常发布，直到同 ID 两个实例在同一 Guard 相遇才 panic。

// valueTypeEntity 是值类型、且因 func 字段不可比较的实体实现：嵌入 *EntityBase 让值类型也满足 IThreadSafeEntity。
type valueTypeEntity struct {
	*EntityBase
	onClear func()
}

func (v valueTypeEntity) Base() *EntityBase { return v.EntityBase }

const pointerContractKind EntityKind = 219 // 同包已用 kind 见各 *_test.go；219 未被占用

func TestValueTypeEntityIsRejectedAtRegistrationAndPublish(t *testing.T) {
	isolateEntityRegistry(t)
	id := mustBuildTestEntityID(t, 6801, testEntityCategoryPlayer, pointerContractKind)

	t.Run("manager add", func(t *testing.T) {
		manager := NewEntityManager()
		err := manager.TryAdd(valueTypeEntity{EntityBase: NewEntityBase(id, testEntityCategoryPlayer, false, pointerContractKind)})
		if err == nil {
			t.Fatalf("TryAdd accepted a value-type entity implementation (%T): the guard compares instances and would panic on this type", valueTypeEntity{})
		}
		// 修前红只断言“被拒绝并点名类型”；修后一并钉住哨兵（ErrEntityNotPointer 随修复新增）。
		if !errors.Is(err, ErrEntityNotPointer) || !strings.Contains(err.Error(), "valueTypeEntity") {
			t.Fatalf("rejection must carry ErrEntityNotPointer and name the type: %v", err)
		}
		if manager.Get(id) != nil {
			t.Fatal("the rejected entity was published")
		}
	})

	t.Run("registered builder", func(t *testing.T) {
		RegisterEntityBuilder(&EntityBuilderParam{
			Category: testEntityCategoryPlayer,
			Kind:     pointerContractKind,
			Builder: func(param *EntityCreateParam) (IThreadSafeEntity, error) {
				return valueTypeEntity{EntityBase: NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
			},
		})
		_, err := BuildEntity(&EntityCreateParam{IsCreate: true, Id: id, Category: testEntityCategoryPlayer, Kind: pointerContractKind})
		if err == nil {
			t.Fatalf("BuildEntity accepted a builder that returns a value-type entity (%T)", valueTypeEntity{})
		}
		if !errors.Is(err, ErrEntityNotPointer) || !strings.Contains(err.Error(), "valueTypeEntity") {
			t.Fatalf("rejection must carry ErrEntityNotPointer and name the type: %v", err)
		}
	})

	t.Run("pointer entity still accepted", func(t *testing.T) {
		manager := NewEntityManager()
		if err := manager.TryAdd(newTestEntity(id, testEntityCategoryPlayer)); err != nil {
			t.Fatalf("TryAdd(pointer entity) = %v", err)
		}
	})
}
