package entity

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeEntitySource(t *testing.T, content string) (string, string) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "player.go")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir, path
}

func expectParseError(t *testing.T, dir string, wants ...string) {
	t.Helper()
	entities, _, err := parseDir(dir)
	if err == nil {
		t.Fatalf("parseDir accepted the source and produced %d entities; want an error mentioning %q", len(entities), wants)
	}
	for _, want := range wants {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error must mention %q; got %v", want, err)
		}
	}
}

// 后面没有结构体的标记（标记与类型之间隔了文档注释，或标记放在接口上）必须按位置报错，不能静默消失。
func TestParseDirRejectsMarkerNotAttachedToStruct(t *testing.T) {
	dir, path := writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer\n// Player is the aggregate.\n// It has a long doc comment.\n// Long enough to push the struct away.\ntype Player struct{}\n")
	expectParseError(t, dir, filepath.Base(path)+":3", "not followed by a struct")
}

func TestParseDirRejectsMarkerAboveNonStruct(t *testing.T) {
	dir, path := writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer\ntype Player interface{}\n")
	expectParseError(t, dir, filepath.Base(path)+":3", "not followed by a struct")
}

// Typos in parameter names were the same as omitting them: `remot=managed`
// produced a local entity, `noPersist` without `=true` produced a persisted
// one. Unknown keys and bare tokens are errors that list the known keys.
func TestParseDirRejectsUnknownMarkerParameters(t *testing.T) {
	dir, _ := writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer remot=managed\ntype Player struct{}\n")
	expectParseError(t, dir, "remot", "unknown marker option", "remote")

	dir, _ = writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer noPersist\ntype Player struct{}\n")
	expectParseError(t, dir, "noPersist", "key=value")
}

// Values outside the accepted spellings fell back to defaults: remote=bogus
// generated a local entity, lifetime=forever a persisted one, sync=ture a
// non-synced one. Each is an error naming the parameter and the value.
func TestParseDirRejectsInvalidMarkerValues(t *testing.T) {
	cases := []struct{ marker, want string }{
		{"//roost:entity entityKind=EntityKindPlayer remote=bogus", `remote="bogus"`},
		{"//roost:entity entityKind=EntityKindPlayer lifetime=forever", `lifetime="forever"`},
		{"//roost:entity entityKind=EntityKindPlayer sync=ture", `sync="ture"`},
		{"//roost:entity entityKind=EntityKindPlayer noPersist=maybe", `noPersist="maybe"`},
	}
	for _, tc := range cases {
		dir, _ := writeEntitySource(t, "package game\n\n"+tc.marker+"\ntype Player struct{}\n")
		expectParseError(t, dir, tc.want)
	}
}

// Every spelling the parsers accept today stays accepted, including the id=
// parameter written by `roost add entity`.
func TestParseDirAcceptsEveryDocumentedMarkerForm(t *testing.T) {
	for _, marker := range []string{
		"//roost:entity id=1 entityKind=EntityKindPlayer",
		"//roost:entity entityKind=EntityKindPlayer remote=managed sync=true",
		"//roost:entity entityKind=EntityKindPlayer noPersist=true lifetime=ephemeral",
		// subjectPacker 是唯一工厂标记。
		"//roost:entity entityKind=EntityKindPlayer sync=on syncNamespace=\"player\" subjectPacker=clientsync.PlayerSubjectPacker",
		"//roost:entity entityKind=EntityKindPlayer sync=on syncNamespace=\"player\" subjectPacker=clientsync.PlayerPacker",
		"//roost:entity entityKind=EntityKindPlayer remote=no lifetime=hot_cold",
	} {
		dir, _ := writeEntitySource(t, "package game\n\n"+marker+"\ntype Player struct{}\n")
		entities, _, err := parseDir(dir)
		if err != nil {
			t.Fatalf("%s rejected: %v", marker, err)
		}
		if len(entities) != 1 || entities[0].Name != "Player" {
			t.Fatalf("%s produced %+v", marker, entities)
		}
	}
}

// remote=managed 却没有内嵌 *entity.RemoteEntityBase 时，generate 在写任何 wire 文件之前拒绝：内嵌普通 base 的
// managed 实体能编译，却永远不参与远程提交。
func TestGenerateRefusesManagedRemoteWithoutRemoteBase(t *testing.T) {
	source, err := os.ReadFile(filepath.Join("testdata", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	text := strings.Replace(string(source),
		"//roost:entity entityKind=EntityKindPlayer sync=true",
		"//roost:entity entityKind=EntityKindPlayer remote=managed sync=true", 1)
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "player.go"), []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
	entities, pkg, err := parseDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entities) != 1 || entities[0].RemoteBase {
		t.Fatalf("fixture must embed the plain base: %+v", entities)
	}
	out := filepath.Join(dir, "player_gen_wire.go")
	written, err := generate(entities[0], pkg, out, true)
	if err == nil || !strings.Contains(err.Error(), "remote=managed requires embedding *entity.RemoteEntityBase") {
		t.Fatalf("generate = %v, %v; want the RemoteEntityBase refusal", written, err)
	}
	if _, statErr := os.Stat(out); statErr == nil {
		t.Fatal("refused generate still wrote the wire file")
	}
}

// A marker key given twice must be refused rather than last-one-wins.
func TestParseDirRejectsMarkerParameterGivenTwice(t *testing.T) {
	dir, _ := writeEntitySource(t, "package game\n\n//roost:entity entityKind=EntityKindPlayer sync=true sync=false\ntype Player struct{}\n")
	expectParseError(t, dir, `duplicate marker option "sync"`)
}

// 同一个包里有多个实体时显式 -output 单文件与“一个实体一个文件”语义冲突（后写的覆盖先写的，连同守卫测试，消费者编译失败）：
// 在写任何文件之前拒绝，并说清要拿掉哪个参数；单实体配 -output 照常工作。
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
