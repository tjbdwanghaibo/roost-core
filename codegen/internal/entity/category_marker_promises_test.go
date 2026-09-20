package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// M-05 · 标记上的 category= 直接进生成物,不再运行期查注册表。
//
// 生成的接线原来写 entity.MustEntityCategoryOfKind(kind):一次运行期查表,查不到就 panic。
// 于是"业务文件里那句手写的 MustRegisterEntityKindCategory 必须先跑"成了隐式前置,而这个先后
// 顺序只由手写的聚合文件第一行保证。category 是这个 kind 的静态事实,标记里写清楚就能直接生成,
// 前置条件随之消失。
func TestEntityMarkerCategoryIsGeneratedDirectly(t *testing.T) {
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := strings.Replace(string(source),
		"//roost:entity entityKind=EntityKindPlayer",
		"//roost:entity entityKind=EntityKindPlayer category=entity.EntityCategoryPlayer", 1)
	if body == string(source) {
		t.Fatal("fixture marker not found; the replacement above did nothing")
	}
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatalf("parseDir with category=: %v", err)
	}
	if len(entities) != 1 {
		t.Fatalf("entities = %d", len(entities))
	}
	if entities[0].Category != "entity.EntityCategoryPlayer" {
		t.Fatalf("parsed category = %q, want the marker's value", entities[0].Category)
	}

	out := filepath.Join(dir, "player_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	generated := string(raw)
	if !strings.Contains(generated, "Category: entity.EntityCategoryPlayer") {
		t.Errorf("generated wiring does not carry the declared category:\n%s", generated)
	}
	if strings.Contains(generated, "MustEntityCategoryOfKind") {
		t.Errorf("generated wiring still resolves the category at run time:\n%s", generated)
	}
}

// Without category= the generated wiring keeps the run-time lookup, so a
// project that has not moved its markers over is unaffected.
func TestEntityMarkerWithoutCategoryKeepsTheRuntimeLookup(t *testing.T) {
	entities, pkg, err := parseDir(filepath.Join("testdata"))
	if err != nil {
		t.Fatal(err)
	}
	if entities[0].Category != "" {
		t.Fatalf("fixture without category= parsed category %q", entities[0].Category)
	}
	out := filepath.Join(t.TempDir(), "player_gen_wire.go")
	if _, err := generate(entities[0], pkg, out, true); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(out)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "MustEntityCategoryOfKind") {
		t.Error("a marker without category= must keep resolving at run time")
	}
}

// A category= value has to look like a category constant expression, so a typo
// fails the generator instead of the consumer's compiler.
func TestEntityMarkerRefusesAMalformedCategory(t *testing.T) {
	for _, bad := range []string{"1", "\"player\"", "entity.", "Player Category"} {
		if err := validateMarkerValues(map[string]string{"category": bad}); err == nil {
			t.Errorf("category=%q was accepted", bad)
		}
	}
	for _, good := range []string{"entity.EntityCategoryOther", "view.EntityCategoryPlayer", "EntityCategoryWorld"} {
		if err := validateMarkerValues(map[string]string{"category": good}); err != nil {
			t.Errorf("category=%q must be accepted: %v", good, err)
		}
	}
}
