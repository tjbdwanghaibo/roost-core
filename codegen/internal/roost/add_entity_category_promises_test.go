package roost

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M-04 · 脚手架生成的实体归 entity.EntityCategoryOther,不再自铸一个 category 常量。
//
// category 就是这个 kind 的锁档。每个实体各铸一个 "= 1" 的话,它们全都占住留给远程托管
// 实体的那一档,于是被排在所有东西之前,而且彼此同档、互相不能叠锁。Other 是安全默认:
// 持有它之后什么都锁不了,不会打乱别人依赖的顺序。
func TestAddEntityScaffoldsTheOtherCategoryInsteadOfMintingOne(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "entity", Name: "Player"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "entity", Name: "Guild"}); err != nil {
		t.Fatal(err)
	}
	// The lifecycle scaffold used to reference the per-entity category
	// constant the entity scaffold minted. That constant is gone, so anything
	// still naming it would leave the project unbuildable.
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	lifecyclePaths, err := Add(root, AddOptions{Kind: "lifecycle", Name: "Player", Entity: "Player", Service: "game"})
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range lifecyclePaths {
		raw, err := os.ReadFile(filepath.Join(root, path))
		if err != nil {
			continue
		}
		if strings.Contains(string(raw), "EntityCategoryPlayer") {
			t.Errorf("%s still names the removed per-entity category constant:\n%s", path, raw)
		}
	}

	for _, name := range []string{"player", "guild"} {
		raw, err := os.ReadFile(filepath.Join(root, "game", "entities", name, "entity.go"))
		if err != nil {
			t.Fatal(err)
		}
		body := string(raw)
		if !strings.Contains(body, "category=entity.EntityCategoryOther") {
			t.Errorf("%s does not declare its category on the marker:\n%s", name, body)
		}
		if strings.Contains(body, "MustRegisterEntityKindCategory") {
			t.Errorf("%s still hand-registers its category, which reintroduces the ordering trap:\n%s", name, body)
		}
		if strings.Contains(body, "entity.EntityCategory = 1") {
			t.Errorf("%s still mints its own category constant, which claims the remote lock rank:\n%s", name, body)
		}
		if strings.Contains(body, "EntityCategory"+strings.ToUpper(name[:1])+name[1:]) {
			t.Errorf("%s still declares a per-entity category constant:\n%s", name, body)
		}
	}
}
