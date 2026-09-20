//go:build daoruntime

// This file is NOT compiled by `go test ./...`: roost-codegen has no runtime
// dependency, and a testdata directory is invisible to the go tool anyway.
// scripts/dao-golden-runtime.sh copies it next to the dao goldens inside a
// throwaway module that depends on roost-core and the Mongo driver, strips
// the build tag, and runs it. It is the check the text goldens cannot be: the
// generated code compiled and pushed through the real codec (U-0224 was a
// generated nested struct whose fields never reached Mongo, and every text
// test was green).
package testdata

import (
	"strings"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// newHero fills a HeroDao without setters (they need an open Nest
// transaction); this is the same package, so the storage is reachable.
func newHero() *HeroDao {
	d := NewHeroDao()
	d.name = "hero"
	d.level = 7
	d.pos.x, d.pos.y = 3, 4
	equips := map[int64]*EquipInfo{}
	for i := int64(1); i <= 2; i++ {
		e := &EquipInfo{level: 5, star: 2}
		gems := map[int32]*GemInfo{}
		for g := int32(1); g <= 3; g++ {
			gems[g] = &GemInfo{id: g, level: 10 * g}
		}
		e.setGemsRawMap(gems)
		equips[i] = e
	}
	d.setEquipsRawMap(equips)
	d.Init()
	return d
}

// Every nested level survives the commit-state document and comes back
// through Unmarshal, and the hook is not in the document.
func TestNestedValuesSurviveTheCommitDocument(t *testing.T) {
	raw, err := newHero().marshalCommitState()
	if err != nil {
		t.Fatal(err)
	}
	text := bson.Raw(raw).String()
	if strings.Contains(text, "dirtyhook") {
		t.Fatalf("DirtyHook leaked into the document: %s", text)
	}
	for _, want := range []string{`"pos": {"x": {"$numberInt":"3"},"y": {"$numberInt":"4"}}`, `"level": {"$numberInt":"5"}`, `"id": {"$numberInt":"3"}`} {
		if !strings.Contains(text, want) {
			t.Fatalf("document lacks %s:\n%s", want, text)
		}
	}
	d := &HeroDao{}
	if err := d.Unmarshal(raw); err != nil {
		t.Fatal(err)
	}
	if d.GetPos().GetX() != 3 || d.GetPos().GetY() != 4 {
		t.Fatalf("pos lost: (%d,%d)", d.GetPos().GetX(), d.GetPos().GetY())
	}
	e, ok := d.GetEquips(2)
	if !ok || e.GetLevel() != 5 || e.GetStar() != 2 || e.GemsLen() != 3 {
		t.Fatalf("equips lost: ok=%v", ok)
	}
	gem, ok := e.GetGems(3)
	if !ok || gem.GetID() != 3 || gem.GetLevel() != 30 {
		t.Fatalf("gem lost: ok=%v", ok)
	}
}

// The rollback snapshot is the other document the DAO writes; it must round
// trip the same way.
func TestNestedValuesSurviveTheRollbackSnapshot(t *testing.T) {
	raw, err := newHero().CaptureRollbackState()
	if err != nil {
		t.Fatal(err)
	}
	d := &HeroDao{}
	if err := d.RestoreRollbackState(raw); err != nil {
		t.Fatal(err)
	}
	if d.GetPos().GetX() != 3 {
		t.Fatalf("pos lost through the rollback snapshot")
	}
	e, ok := d.GetEquips(1)
	if !ok || e.GemsLen() != 3 {
		t.Fatalf("equips lost through the rollback snapshot: ok=%v", ok)
	}
}

// A nil pointer element stays null rather than becoming an empty object.
func TestNilNestedPointersStayNull(t *testing.T) {
	d := NewHeroDao()
	d.setEquipsRawMap(map[int64]*EquipInfo{1: nil})
	d.Init()
	raw, err := d.marshalCommitState()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(bson.Raw(raw).String(), `"equips": {"1": null}`) {
		t.Fatalf("nil element not preserved: %s", bson.Raw(raw).String())
	}
}

// U-0232 · C2 · RR-20260917-05（Wanted-02 转入）：嵌套结构的第二层也要往上通知。
// 旧行为：DAO 会把自己的嵌套字段接到 markXDirty（template_dao.go），但一个嵌套结构
// 内部的嵌套（EquipInfo 的 gems、Position 这类）从不 SetNotify，于是业务
// GetGems(1).SetLevel(...) 改了 child、父级 EquipInfo 不脏、DAO 顶层也拿不到补丁——
// 数据在内存里变了，落库时不在 patch 里。三个入口都要接：公开 setter、raw 恢复、BSON 恢复。

// countingParent installs a counter on a nested struct's dirty hook so a test
// can see whether a child's change reached it.
func countingParent(equip *EquipInfo) *int {
	marks := 0
	equip.SetNotify(func() { marks++ })
	return &marks
}

func TestNestedChildChangeMarksItsParentThroughEveryEntry(t *testing.T) {
	for name, build := range map[string]func() *EquipInfo{
		"raw restore": func() *EquipInfo {
			equip := &EquipInfo{level: 1}
			equip.setGemsRawMap(map[int32]*GemInfo{1: {id: 1, level: 5}})
			return equip
		},
		"public setter": func() *EquipInfo {
			equip := &EquipInfo{level: 1}
			equip.SetGems(map[int32]*GemInfo{1: {id: 1, level: 5}})
			return equip
		},
		"bson round trip": func() *EquipInfo {
			source := &EquipInfo{level: 1}
			source.setGemsRawMap(map[int32]*GemInfo{1: {id: 1, level: 5}})
			raw, err := bson.Marshal(source)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			var restored EquipInfo
			if err := bson.Unmarshal(raw, &restored); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			return &restored
		},
	} {
		equip := build()
		marks := countingParent(equip)
		gem, ok := equip.GetGems(1)
		if !ok || gem == nil {
			t.Fatalf("%s: the gem is not there", name)
		}
		gem.SetLevel(99)
		if *marks == 0 {
			t.Errorf("%s: changing a child gem did not mark its EquipInfo; the DAO cannot see the change", name)
		}
		// The control: changing the parent itself always marked it.
		before := *marks
		equip.SetStar(3)
		if *marks == before {
			t.Errorf("%s: changing the parent did not mark it either", name)
		}
	}
}

// A child that has been replaced must not keep marking its old parent: a
// detached object reporting into a live one is a dirty flag nobody can
// explain.
func TestReplacedChildStopsMarkingTheOldParent(t *testing.T) {
	equip := &EquipInfo{level: 1}
	old := &GemInfo{id: 1, level: 5}
	equip.setGemsRawMap(map[int32]*GemInfo{1: old})
	marks := countingParent(equip)

	equip.SetGems(map[int32]*GemInfo{1: {id: 1, level: 7}})
	after := *marks
	old.SetLevel(42)
	if *marks != after {
		t.Errorf("a replaced child still marks its old parent")
	}
}
