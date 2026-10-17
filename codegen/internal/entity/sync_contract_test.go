package entity

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncNamespaceRejectsABareIdentifier(t *testing.T) {
	for name, value := range map[string]string{
		"same package constant": "SyncNamespacePlayer",
		"exported and short":    "Topic",
	} {
		if err := validateSyncNamespaceParam(value); err == nil {
			t.Errorf("%s: syncNamespace=%s was accepted; it would be written out as the string %q", name, value, value)
		}
	}
}

// The two unambiguous spellings keep working, and a qualified constant is
// still emitted as a reference rather than quoted.
func TestSyncNamespaceAcceptsLiteralsAndQualifiedConstants(t *testing.T) {
	for name, test := range map[string]struct {
		value string
		want  string
	}{
		"lower case literal": {"player", `"player"`},
		"quoted literal":     {`"Player"`, `"Player"`},
		"qualified constant": {"clientsync.PlayerTopic", "clientsync.PlayerTopic"},
		"empty":              {"", `""`},
	} {
		if err := validateSyncNamespaceParam(test.value); err != nil {
			t.Errorf("%s: syncNamespace=%s was rejected: %v", name, test.value, err)
			continue
		}
		if got := syncNamespaceExpr(test.value); got != test.want {
			t.Errorf("%s: syncNamespaceExpr(%q) = %s, want %s", name, test.value, got, test.want)
		}
	}
}

func writeSyncMarkerSource(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGeneratedSyncBlockOnlyNamesFieldsCoreHas(t *testing.T) {
	for name, marker := range map[string]string{
		"current spelling": "//roost:entity entityKind=EntityKindAvatar sync=true syncNamespace=\"avatar\" subjectPacker=AvatarPacker",
		"no packer":        "//roost:entity entityKind=EntityKindAvatar sync=true syncNamespace=\"avatar\"",
	} {
		dir := t.TempDir()
		writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const EntityKindAvatar entity.EntityKind = 141
const SyncNamespaceAvatar = "avatar"

func AvatarPacker(entity.IThreadSafeEntity) entity.SubjectSyncPacker { return nil }

`+marker+`
type Avatar struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}
`)
		if err := Run([]string{"-dir", dir, "-force"}, os.Stdout); err != nil {
			t.Fatalf("%s: generate: %v", name, err)
		}
		raw, err := os.ReadFile(filepath.Join(dir, "avatar_gen_wire.go"))
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		generated := string(raw)
		for _, gone := range []string{"FlushPolicy", "SyncFlushOnEntityRelease", "SubjectPackerFactory"} {
			if strings.Contains(generated, gone) {
				t.Errorf("%s: generated wiring still names %q, which entity.EntitySyncBuilderParam does not have:\n%s", name, gone, generated)
			}
		}
		// gofmt aligns the struct literal, so compare with the padding
		// collapsed rather than against one exact spelling.
		collapsed := strings.Join(strings.Fields(generated), " ")
		if !strings.Contains(collapsed, "Enabled: true") || !strings.Contains(collapsed, `Namespace: "avatar"`) {
			t.Errorf("%s: sync block lost its enabled/namespace wiring:\n%s", name, generated)
		}
		wantFactory := strings.Contains(marker, "Packer=")
		if got := strings.Contains(collapsed, "PackerFactory: AvatarPacker"); got != wantFactory {
			t.Errorf("%s: PackerFactory present = %v, want %v:\n%s", name, got, wantFactory, generated)
		}
	}
}

// 旧标记不能被静默忽略，否则生成实体会丢失业务 packer。
func TestRetiredPackerMarkerIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const EntityKindAvatar entity.EntityKind = 141

func AvatarPacker(entity.IThreadSafeEntity) entity.SubjectSyncPacker { return nil }

//roost:entity entityKind=EntityKindAvatar sync=true syncPacker=AvatarPacker
type Avatar struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}
`)
	err := Run([]string{"-dir", dir, "-force"}, os.Stdout)
	if err == nil {
		t.Fatal("retired packer marker was accepted")
	}
	if !strings.Contains(err.Error(), "use subjectPacker") {
		t.Errorf("refusal does not say what to do: %v", err)
	}
}

// A packer without sync=true is a marker that does nothing; saying so beats
// generating an entity whose factory is never used.
func TestPackerWithoutSyncIsRefused(t *testing.T) {
	dir := t.TempDir()
	writeSyncMarkerSource(t, dir, "avatar.go", `package avatar

import "github.com/tjbdwanghaibo/roost-core/framework/entity"

const EntityKindAvatar entity.EntityKind = 141

func AvatarPacker(entity.IThreadSafeEntity) entity.SubjectSyncPacker { return nil }

//roost:entity entityKind=EntityKindAvatar subjectPacker=AvatarPacker
type Avatar struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
}
`)
	if err := Run([]string{"-dir", dir, "-force"}, os.Stdout); err == nil {
		t.Fatal("a packer marker without sync=true was accepted")
	}
}

// access.player.self_sync 的默认 packer（-self-sync）：开关打开且标记没写 subjectPacker= 时生成
// player_sync_packer_gen.go 并让 wire 指向它；写了 subjectPacker= 或开关关闭时不生成、删掉以前生成的那份，
// 业务自己的同名文件不动；标记缺省按 sync=true。
func TestSelfSyncDefaultPackerFollowsTheMarker(t *testing.T) {
	root := t.TempDir()
	write := func(rel, body string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("go.mod", "module example.com/planet\n\ngo 1.24\n")
	write("db/player_dao_gen.go", "package db\n\ntype PlayerDao struct{}\n\nfunc (d *PlayerDao) MarshalSync(uint64) []byte { return nil }\n")
	entitySource := func(marker string, daoFields string) string {
		return `package player

import (
	db "example.com/planet/db"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

const EntityKindPlayer entity.EntityKind = 1

func CustomPacker(entity.IThreadSafeEntity) entity.SubjectSyncPacker { return nil }

var _ db.PlayerDao

` + marker + `
type Player struct {
	*entity.EntityBase
	entity.ComponentManager
	entity.DaoManager
` + daoFields + `
}
`
	}
	const oneDao = "\tdao *db.PlayerDao `dao:\"player\"`"
	dir := filepath.Join(root, "game", "entities", "player")
	packerPath := filepath.Join(dir, "player_sync_packer_gen.go")
	wirePath := filepath.Join(dir, "player_gen_wire.go")
	run := func(args ...string) (string, error) {
		t.Helper()
		var out strings.Builder
		err := Run(append([]string{"-dir", filepath.Join(root, "game")}, args...), &out)
		return out.String(), err
	}
	wire := func() string {
		t.Helper()
		raw, err := os.ReadFile(wirePath)
		if err != nil {
			t.Fatal(err)
		}
		return strings.Join(strings.Fields(string(raw)), " ")
	}
	exists := func(path string) bool {
		_, err := os.Stat(path)
		return err == nil
	}

	// 只开开关：标记没写 sync / subjectPacker，DAO 还没有 sync 字段。
	write("game/entities/player/entity.go", entitySource("//roost:entity id=1 entityKind=EntityKindPlayer", oneDao))
	out, err := run("-self-sync", "Player")
	if err != nil {
		t.Fatalf("self_sync with a bare marker: %v\n%s", err, out)
	}
	packer, err := os.ReadFile(packerPath)
	if err != nil {
		t.Fatalf("default packer not generated: %v\n%s", err, out)
	}
	for _, want := range []string{
		"// Code generated by tool/entity. DO NOT EDIT.",
		"package player",
		`db "example.com/planet/db"`,
		"func NewPlayerSyncPacker(value entity.IThreadSafeEntity) entity.SubjectSyncPacker",
		"entity.NamedSyncView{Profile: policy.SelfProfile, Fields: []string{\"*\"}}",
		"any((*db.PlayerDao)(nil)).(interface {",
		"SyncFields() []dataengine.SyncFieldMeta",
		"entity.ErrSyncViewUnknown",
		"packer.owner.Dao()",
	} {
		if !strings.Contains(string(packer), want) {
			t.Errorf("default packer lacks %q:\n%s", want, packer)
		}
	}
	for _, want := range []string{"Enabled: true", `Namespace: "player"`, "PackerFactory: NewPlayerSyncPacker"} {
		if !strings.Contains(wire(), want) {
			t.Errorf("wire lacks %q with the default packer:\n%s", want, wire())
		}
	}
	if !strings.Contains(out, "warning: db.PlayerDao has no field tagged sync") || !strings.Contains(out, `dao:"persist,sync"`) {
		t.Errorf("no warning for a DAO without sync fields:\n%s", out)
	}
	// 标了 sync 字段（生成的 DAO 带 SyncFields）后不再警告，packer 内容不变（字段表运行期取）。
	write("db/player_dao_sync_gen.go", "package db\n\nimport \"github.com/tjbdwanghaibo/roost-core/framework/dataengine\"\n\nfunc PlayerDaoSyncFields() []dataengine.SyncFieldMeta { return nil }\n")
	if out, err = run("-self-sync", "Player"); err != nil || strings.Contains(out, "warning:") {
		t.Fatalf("with sync fields: err=%v\n%s", err, out)
	}
	if again, _ := os.ReadFile(packerPath); string(again) != string(packer) {
		t.Errorf("default packer changed when the DAO gained sync fields")
	}

	// 业务写了 subjectPacker=（不必再写 sync=true）：用它，生成的那份删掉。
	write("game/entities/player/entity.go", entitySource("//roost:entity id=1 entityKind=EntityKindPlayer subjectPacker=CustomPacker", oneDao))
	if out, err = run("-self-sync", "Player"); err != nil {
		t.Fatalf("custom packer: %v\n%s", err, out)
	}
	if exists(packerPath) {
		t.Errorf("default packer kept after the marker named subjectPacker=CustomPacker")
	}
	if w := wire(); !strings.Contains(w, "PackerFactory: CustomPacker") || !strings.Contains(w, "Enabled: true") {
		t.Errorf("wire does not use the business packer:\n%s", w)
	}

	// 再删掉 subjectPacker=：生成的那份回来。
	write("game/entities/player/entity.go", entitySource("//roost:entity id=1 entityKind=EntityKindPlayer", oneDao))
	if out, err = run("-self-sync", "Player"); err != nil || !exists(packerPath) {
		t.Fatalf("back to the default packer: err=%v exists=%v\n%s", err, exists(packerPath), out)
	}

	// 开关关闭：不生成、删掉；标记照原样（没写 sync 就没有 Sync 块）。
	if out, err = run(); err != nil {
		t.Fatalf("self_sync off: %v\n%s", err, out)
	}
	if exists(packerPath) {
		t.Errorf("default packer kept with self_sync off")
	}
	if w := wire(); strings.Contains(w, "EntitySyncBuilderParam") || strings.Contains(w, "PackerFactory") {
		t.Errorf("self_sync off still turns Entity Sync on:\n%s", w)
	}

	// 业务自己的同名文件（没有生成头）：开关关闭时不删，需要生成时拒绝覆盖并说出改法。
	write("game/entities/player/player_sync_packer_gen.go", "package player\n")
	if out, err = run(); err != nil || !exists(packerPath) {
		t.Fatalf("a business file with the packer's name: err=%v exists=%v\n%s", err, exists(packerPath), out)
	}
	if _, err = run("-self-sync", "Player"); err == nil || !strings.Contains(err.Error(), "is not generated") || !strings.Contains(err.Error(), "subjectPacker=NewPlayerSyncPacker") {
		t.Fatalf("overwrote or silently kept a business file: %v", err)
	}
	if raw, _ := os.ReadFile(packerPath); string(raw) != "package player\n" {
		t.Fatalf("business file changed: %q", raw)
	}
	if err := os.Remove(packerPath); err != nil {
		t.Fatal(err)
	}

	// 拒绝：显式 sync=false、没有 DAO、多个 DAO、实体不存在。
	for name, test := range map[string]struct {
		marker, fields, flag, want string
	}{
		"sync=false":    {"//roost:entity id=1 entityKind=EntityKindPlayer sync=false", oneDao, "Player", "drop sync=false"},
		"no DAO":        {"//roost:entity id=1 entityKind=EntityKindPlayer", "", "Player", "roost add dao Player --entity Player"},
		"two DAOs":      {"//roost:entity id=1 entityKind=EntityKindPlayer", oneDao + "\n\tbag *db.PlayerDao `dao:\"bag\"`", "Player", "subjectPacker=NewPlayerSyncPacker"},
		"absent entity": {"//roost:entity id=1 entityKind=EntityKindPlayer", oneDao, "Hero", "no //roost:entity struct named Hero"},
	} {
		write("game/entities/player/entity.go", entitySource(test.marker, test.fields))
		if _, err := run("-self-sync", test.flag); err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: %v, want a refusal naming %q", name, err, test.want)
		}
	}
}
