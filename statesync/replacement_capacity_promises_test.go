package statesync

import (
	"reflect"
	"testing"
)

// U-0204 · C8 · RR-20260915-01:合法的前后快照经 BuildDelta → EncodeFrame → DecodeFrame → ApplyDelta
// 必须能还原目标,与标识排序无关。三个阶段对"上限"的不变量不一致:NewSnapshot 校验的是最终集合,
// diff 按标识有序合并、编码允许增删操作数大于最终存量,而 ApplyDelta 却在**每一步操作之后**用最终
// 存量上限检查暂存 map——MaxObjects=1 时 ID 2 → 1 先 Create(1) 再 Remove(2),第一步暂存两个对象就
// ErrObjectLimit;组件层同理(MaxComponentsPerObject=1,TypeID 2 → 1)。改成 3(先删后加)就成功。
// 承诺:存量上限只约束最终集合;单帧的临时占用由解码侧的每帧操作数上限约束。

func TestApplyDeltaPromiseFullCapacityReplacementIsOrderIndependent(t *testing.T) {
	for _, mode := range []string{"object_lower", "object_higher", "component_lower", "component_higher", "schema_change", "archetype_change"} {
		t.Run(mode, func(t *testing.T) {
			limits := DefaultLimits()
			limits.MaxObjects = 1
			limits.MaxComponentsPerObject = 1
			old := ObjectState{Ref: ObjectRef{ID: 2, Generation: 1}, Archetype: 1, Components: []ComponentState{{TypeID: 2, SchemaVersion: 1, Data: []byte{1}}}}
			cur := cloneObject(old)
			switch mode {
			case "object_lower":
				cur.Ref.ID = 1
			case "object_higher":
				cur.Ref.ID = 3
			case "component_lower":
				cur.Components[0].TypeID = 1
			case "component_higher":
				cur.Components[0].TypeID = 3
			case "schema_change":
				cur.Components[0].SchemaVersion = 2
			case "archetype_change":
				cur.Archetype = 2
			}
			base, err := NewSnapshot(testMeta(1), []ObjectState{old}, limits)
			if err != nil {
				t.Fatal(err)
			}
			next, err := NewSnapshot(testMeta(2), []ObjectState{cur}, limits)
			if err != nil {
				t.Fatal(err)
			}
			frame, err := BuildDelta(&base, next)
			if err != nil {
				t.Fatal(err)
			}
			raw, err := EncodeFrame(frame, limits)
			if err != nil {
				t.Fatal(err)
			}
			frame, err = DecodeFrame(raw, limits)
			if err != nil {
				t.Fatal(err)
			}
			got, err := ApplyDelta(&base, frame, limits)
			if err != nil {
				t.Fatalf("valid before/after snapshots rejected: %v operations=%+v", err, frame.Objects)
			}
			if !reflect.DeepEqual(got, next) {
				t.Fatalf("roundtrip mismatch got=%+v want=%+v", got, next)
			}
		})
	}
}

// 上限没有被取消:最终集合超限仍然拒绝,对象与组件两层都是。
func TestApplyDeltaPromiseFinalSetStillBounded(t *testing.T) {
	limits := DefaultLimits()
	limits.MaxObjects = 1
	limits.MaxComponentsPerObject = 1
	base, err := NewSnapshot(testMeta(1), []ObjectState{{Ref: ObjectRef{ID: 1, Generation: 1}, Archetype: 1}}, limits)
	if err != nil {
		t.Fatal(err)
	}
	tooMany := DeltaFrame{SnapshotMeta: testMeta(2), Kind: FrameDelta, BaseTick: 1, Objects: []ObjectDelta{
		{Operation: ObjectCreate, Ref: ObjectRef{ID: 2, Generation: 1}, Archetype: 1},
	}}
	if _, err := ApplyDelta(&base, tooMany, limits); err != ErrObjectLimit {
		t.Fatalf("final object set over the limit must be refused: %v", err)
	}
	tooManyComponents := DeltaFrame{SnapshotMeta: testMeta(2), Kind: FrameDelta, BaseTick: 1, Objects: []ObjectDelta{
		{Operation: ObjectUpdate, Ref: ObjectRef{ID: 1, Generation: 1}, Components: []ComponentDelta{
			{Operation: ComponentSet, TypeID: 1, SchemaVersion: 1, Data: []byte{1}},
			{Operation: ComponentSet, TypeID: 2, SchemaVersion: 1, Data: []byte{2}},
		}},
	}}
	if _, err := ApplyDelta(&base, tooManyComponents, limits); err != ErrComponentLimit {
		t.Fatalf("final component set over the limit must be refused: %v", err)
	}
}
