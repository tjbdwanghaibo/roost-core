package entity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// U-0162 · C2 · RR-20260909-06：同一个包里两个实体,再显式给 -output 指定单个文件。
//
// 循环对每个实体用同一个输出路径,后者覆盖前者,连同伴随的守卫测试文件;工具 exit 0 并两次
// 报告"生成同一路径",而消费者编译失败 —— 活下来的那个文件不是按名排序的第一个,包级
// RegisterEntity 随被覆盖的文件一起消失,报 undefined: RegisterEntity。
//
// -output 是给 go:generate 的单文件模式用的,它和"一个包多个实体各占一个文件"在语义上冲突。
// 所以在写任何文件之前拒绝,并说清要拿掉哪个参数;单实体配 -output 仍然照常工作。
func TestExplicitOutputRefusesMoreThanOneEntityInAPackage(t *testing.T) {
	dir := t.TempDir()
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	player := string(source)
	npc := strings.NewReplacer(
		"Player", "Npc", "player", "npc",
		"EntityCategory = 1", "EntityCategory = 2",
		"EntityKind     = 1", "EntityKind     = 2",
		"CompTypeBag    entity.ComponentType = 1", "CompTypeBag    entity.ComponentType = 11",
		"CompTypeBattle entity.ComponentType = 2", "CompTypeBattle entity.ComponentType = 12",
		"MailDao", "NpcMailDao", `"mails"`, `"npc_mails"`,
	).Replace(player)
	npc = strings.Replace(npc, "var _ = registerEntityKinds()", "var _ = registerNpcEntityKinds()", 1)
	npc = strings.Replace(npc, "func registerEntityKinds() struct{} {", "func registerNpcEntityKinds() struct{} {", 1)
	for _, f := range []struct{ name, body string }{{"player.go", player}, {"npc.go", npc}} {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte(f.body), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	out := filepath.Join(dir, "entities_gen_wire.go")
	var stdout bytes.Buffer
	err = Run([]string{"-dir", dir, "-output", out}, &stdout)
	if err == nil {
		t.Fatalf("two entities with an explicit -output were accepted; stdout:\n%s", stdout.String())
	}
	for _, want := range []string{"-output", "Npc", "Player"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Error("the refused run still wrote the output file; it must refuse before writing anything")
	}

	// The single-entity go:generate mode is what -output exists for.
	single := t.TempDir()
	if err := os.WriteFile(filepath.Join(single, "player.go"), []byte(player), 0o644); err != nil {
		t.Fatal(err)
	}
	singleOut := filepath.Join(single, "player_gen_wire.go")
	if err := Run([]string{"-dir", single, "-output", singleOut}, &stdout); err != nil {
		t.Fatalf("one entity with -output must still work: %v", err)
	}
	if _, err := os.Stat(singleOut); err != nil {
		t.Fatalf("single-entity -output produced no file: %v", err)
	}
}
