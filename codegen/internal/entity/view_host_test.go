package entity

import (
	"bytes"
	"flag"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/genutil"
)

var updateGolden = flag.Bool("update", false, "rewrite golden files")

// 2026-11-16 整理轮的 entity 生成器夹具 testdata/viewevent：组件包里的类型号（comp:"bag.CompTypeBag"）、view 的
// <X>Holder 带宿主方法、实体事件监听。生成结果对 testdata/viewevent_golden 比对。

// copyFixture 把 testdata/<name> 复制到临时目录，返回工程根目录。
func copyFixture(t *testing.T, name string) string {
	t.Helper()
	src := filepath.Join("testdata", name)
	root := t.TempDir()
	err := filepath.WalkDir(src, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(root, rel)
		if entry.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, raw, 0o644)
	})
	if err != nil {
		t.Fatal(err)
	}
	return root
}

func writeProjectFile(t *testing.T, root, rel, body string) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readProjectFile(t *testing.T, root, rel string) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// runEntities 对 <root>/game/entities 跑一次生成器，返回输出。
func runEntities(t *testing.T, root string) (string, error) {
	t.Helper()
	var out bytes.Buffer
	err := run([]string{"-dir", filepath.Join(root, "game", "entities")}, &out)
	return out.String(), err
}

// 夹具整体生成一次，逐个比对 golden：view 窄接口、实体接口、宿主方法与编译期断言、事件登记。
// 再生成一次内容不变（顺序确定）。
func TestViewEventFixtureMatchesGolden(t *testing.T) {
	root := copyFixture(t, "viewevent")
	if _, err := runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	files := []string{
		"game/view/holder_entities_gen.go",
		"game/view/entity_player.go",
		"game/view/entity_guild.go",
		"game/entities/player/player_gen_wire.go",
		"game/entities/player/player_event_gen.go",
		"game/entities/guild/guild_gen_wire.go",
		"game/entities/guild/guild_event_gen.go",
	}
	first := map[string]string{}
	for _, rel := range files {
		got := readProjectFile(t, root, rel)
		first[rel] = got
		genutil.AssertGolden(t, filepath.Join("testdata", "viewevent_golden", filepath.Base(rel)+".golden"), []byte(got), *updateGolden)
	}
	if _, err := runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	for _, rel := range files {
		if got := readProjectFile(t, root, rel); got != first[rel] {
			t.Errorf("%s changed on the second run", rel)
		}
	}
}

// comp 标签带包名：getter 返回组件包里的具体类型，不再是 alias.I<X>Component；
// 包名必须在实体源文件里 import 过。不带包名的旧写法（组件在实体包里）照旧。
func TestQualifiedCompTagGetterReturnsTheConcreteType(t *testing.T) {
	root := copyFixture(t, "viewevent")
	if _, err := runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	wire := readProjectFile(t, root, "game/entities/player/player_gen_wire.go")
	for _, want := range []string{
		"func (e *Player) BagComp() *bag.Component { return e.bag }",
		"func (e *Player) QuestComp() *QuestComponent { return e.quest }",
		"entity.CreateComponent(bag.CompTypeBag, e, param)",
		"e.ComponentManager.Set(CompTypeQuest, e.quest)",
		`"example.com/g/game/components/bag"`,
	} {
		if !strings.Contains(wire, want) {
			t.Errorf("wire lacks %q", want)
		}
	}
	if strings.Contains(wire, "IBagComponent") {
		t.Errorf("wire still returns the retired alias.I<X>Component:\n%s", wire)
	}

	entities, _, err := parseDir(filepath.Join(root, "game", "entities", "player"))
	if err != nil {
		t.Fatal(err)
	}
	for _, comp := range entities[0].Components {
		if comp.FieldName == "bag" && (comp.CompPkg != "example.com/g/game/components/bag" || comp.TypePkg != comp.CompPkg || comp.GetterType != "*bag.Component") {
			t.Errorf("bag component = %+v", comp)
		}
		if comp.FieldName == "quest" && (comp.CompPkg != "" || comp.TypePkg != "" || comp.GetterType != "*QuestComponent") {
			t.Errorf("quest component = %+v", comp)
		}
	}

	// 标签里的包名没有 import：生成前报错，点出字段与包名。
	source := readProjectFile(t, root, "game/entities/guild/entity.go")
	writeProjectFile(t, root, "game/entities/guild/entity.go", strings.Replace(source, "`comp:\"activity.CompTypeActivity\"`", "`comp:\"missing.CompTypeActivity\"`", 1))
	if _, err := runEntities(t, root); err == nil || !strings.Contains(err.Error(), "field activity") || !strings.Contains(err.Error(), "package missing") {
		t.Fatalf("an unimported package in the comp tag = %v", err)
	}
}

// 纯 DAO 交接的匹配规则：同名 → 组件名 + 字段名（只对结构体）→ 按类型找唯一字段（只对结构体）；
// 基础类型只认完全同名；有歧义或找不到交给手写并在断言注释里写明原因；已经手写的不生成。
func TestDaoHandoffMatching(t *testing.T) {
	fields := []daoFieldInfo{
		{DaoType: "PlayerDao", Name: "Bag", Key: "*m/db.BagState", Struct: true},
		{DaoType: "PlayerDao", Name: "AttributeBase", Key: "*m/db.AttributeValues", Struct: true},
		{DaoType: "PlayerDao", Name: "Gear", Key: "*m/db.AttributeValues", Struct: true},
		{DaoType: "PlayerDao", Name: "Final", Key: "*m/db.AttributeValues", Struct: true},
		{DaoType: "PlayerDao", Name: "Level", Key: "int32"},
		{DaoType: "PlayerDao", Name: "Exp", Key: "int64"},
		{DaoType: "GuildDao", Name: "Board", Key: "*m/db.NoteBook", Struct: true},
		{DaoType: "GuildDao", Name: "Archive", Key: "*m/db.NoteBook", Struct: true},
	}
	for _, tc := range []struct {
		host      viewHostMethod
		component string
		want      string // 选中的字段，空表示不生成
		note      string
	}{
		{viewHostMethod{Name: "AttributeBase", Result: "*m/db.AttributeValues", Struct: true}, "Attribute", "AttributeBase", ""},
		{viewHostMethod{Name: "AttributeGear", Result: "*m/db.AttributeValues", Struct: true}, "Attribute", "Gear", ""},
		{viewHostMethod{Name: "AttributeFinal", Result: "*m/db.AttributeValues", Struct: true}, "Attribute", "Final", ""},
		{viewHostMethod{Name: "AttributeTotal", Result: "*m/db.AttributeValues", Struct: true}, "Attribute", "", "DAO 里有多个这个类型的字段"},
		{viewHostMethod{Name: "BagState", Result: "*m/db.BagState", Struct: true}, "Bag", "Bag", ""},
		{viewHostMethod{Name: "Notes", Result: "*m/db.NoteBook", Struct: true}, "Activity", "", "GuildDao.Board、GuildDao.Archive"},
		{viewHostMethod{Name: "Level", Result: "int32"}, "Profile", "Level", ""},
		{viewHostMethod{Name: "ProfileLevel", Result: "int32"}, "Profile", "", "基础类型只认完全同名"},
		{viewHostMethod{Name: "Exp", Result: "int32"}, "Profile", "", ""},
		{viewHostMethod{Name: "WalletSpent"}, "Wallet", "", ""},
	} {
		got, note := matchDaoHandoff(tc.host, tc.component, fields)
		name := ""
		if got != nil {
			name = got.Name
		}
		if name != tc.want || !strings.Contains(note, tc.note) {
			t.Errorf("%s: field %q note %q, want field %q note containing %q", tc.host.Name, name, note, tc.want, tc.note)
		}
	}
}

// view 的 Holder：访问器以外的方法都是宿主方法；缺访问器、嵌入接口、未导出的宿主方法报错；
// 手写的类型与生成的 I<X>Entity 重名时报错。
func TestViewHolderShapeIsValidated(t *testing.T) {
	for _, tc := range []struct {
		source, want string
	}{
		{"package view\n\ntype Bag interface{}\n\ntype BagHolder interface{ BagState() int64 }\n", "must declare the component accessor Bag()"},
		{"package view\n\ntype Bag interface{}\n\ntype BagHolder interface{ Bag() int64 }\n", "Bag is the component accessor"},
		{"package view\n\ntype Bag interface{}\n\ntype Other interface{}\n\ntype BagHolder interface{ Other; Bag() Bag }\n", "BagHolder embeds Other"},
		{"package view\n\ntype Bag interface{}\n\ntype BagHolder interface{ Bag() Bag; state() int64 }\n", "host method BagHolder.state must be exported"},
		{"package view\n\ntype Bag interface{}\n\ntype BagHolder interface{ Bag() Bag }\n\ntype IBagEntity interface{}\n", "already declares IBagEntity"},
	} {
		root := t.TempDir()
		writeProjectFile(t, root, "go.mod", "module example.com/g\n\ngo 1.24\n")
		writeProjectFile(t, root, "game/view/component_bag.go", tc.source)
		writeProjectFile(t, root, "game/entities/player/player.go", "package player\n\nimport \"github.com/tjbdwanghaibo/roost-core/framework/entity\"\n\nconst EntityKindPlayer = 1\n\n//roost:entity entityKind=EntityKindPlayer\ntype Player struct {\n\t*entity.EntityBase\n}\n")
		if _, err := runEntities(t, root); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%q: err = %v, want %q", tc.source, err, tc.want)
		}
	}
}

// 组件字段与 view 访问器同名（Bag *bag.Component 与 Bag()）编译不过：生成前报错，建议改字段名。
func TestComponentFieldNamedLikeTheAccessorIsRefused(t *testing.T) {
	root := copyFixture(t, "viewevent")
	source := readProjectFile(t, root, "game/entities/player/entity.go")
	source = strings.Replace(source, "\tbag       *bag.Component       `comp:\"bag.CompTypeBag\"`", "\tBag *bag.Component `comp:\"bag.CompTypeBag\"`", 1)
	writeProjectFile(t, root, "game/entities/player/entity.go", source)
	if _, err := runEntities(t, root); err == nil || !strings.Contains(err.Error(), "field Bag has the same name as the view accessor Bag()") {
		t.Fatalf("field named like the accessor = %v", err)
	}
}

// 生成代码对着真实的 framework 编译：宿主方法缺了时编译期断言报出缺的方法名；补上手写方法后整个夹具编译通过
// （事件登记、SetEventDispatch、view 窄接口与 DAO 交接都是 core 当前的 API）。
func TestViewEventFixtureCompilesAgainstTheFramework(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a generated project")
	}
	repo, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	root := copyFixture(t, "viewevent")
	goVersion := ""
	for _, line := range strings.Split(readProjectFile(t, repo, "go.mod"), "\n") {
		if version, ok := strings.CutPrefix(line, "go "); ok {
			goVersion = strings.TrimSpace(version)
		}
	}
	writeProjectFile(t, root, "go.mod", "module example.com/g\n\ngo "+goVersion+"\n\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\n\nreplace github.com/tjbdwanghaibo/roost-core => "+repo+"\n")
	if sum, err := os.ReadFile(filepath.Join(repo, "go.sum")); err == nil {
		writeProjectFile(t, root, "go.sum", string(sum))
	}
	daoRun := exec.Command("go", "run", "./codegen/cmd/dao", "-def", filepath.Join(root, "db", "def"), "-out", filepath.Join(root, "db"), "-pkg", "db")
	daoRun.Dir, daoRun.Env = repo, append(os.Environ(), "GOWORK=off")
	if out, err := daoRun.CombinedOutput(); err != nil {
		t.Fatalf("dao generator: %v\n%s", err, out)
	}
	if _, err := runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	build := func() (string, error) {
		cmd := exec.Command("go", "build", "./...")
		cmd.Dir, cmd.Env = root, append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	out, err := build()
	if err == nil {
		t.Fatal("the fixture compiled although Player lacks AttributeLevel and Guild lacks ActivityScope / Notes")
	}
	for _, want := range []string{"*Player does not implement view.PlayerEntity (missing method AttributeLevel)", "*Guild does not implement view.GuildEntity (missing method ActivityScope)"} {
		if !strings.Contains(out, want) {
			t.Errorf("build output lacks %q:\n%s", want, out)
		}
	}
	writeProjectFile(t, root, "game/entities/player/attribute.go", "package player\n\nfunc (e *Player) AttributeLevel() int32 { return e.Profile().Level() }\n")
	writeProjectFile(t, root, "game/entities/guild/activity.go", "package guild\n\nimport db \"example.com/g/db\"\n\nfunc (e *Guild) ActivityScope() string { return \"guild\" }\nfunc (e *Guild) Notes() *db.NoteBook  { return e.Dao().GetBoard() }\n")
	if _, err := runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	if out, err := build(); err != nil {
		t.Fatalf("generated fixture does not compile: %v\n%s", err, out)
	}
	if wire := readProjectFile(t, root, "game/entities/guild/guild_gen_wire.go"); strings.Contains(wire, "还没有实现") {
		t.Errorf("the assertion comment still marks hand-written methods missing:\n%s", wire)
	}
}

// v1.47.0 的 view 文件随实体删除；本轮新增的 holder_entities_gen.go 在 view 里没有 Holder 时删除，
// 实体没有监听了时 <实体>_event_gen.go 删除。
func TestGeneratedViewAndEventFilesAreRetired(t *testing.T) {
	root := copyFixture(t, "viewevent")
	if _, err := runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	// Guild 不再挂 activity：Guild 没有监听，guild_event_gen.go 删掉；Player 的留着。
	source := readProjectFile(t, root, "game/entities/guild/entity.go")
	source = strings.Replace(source, "\tactivity *activity.Component `comp:\"activity.CompTypeActivity\"`\n", "", 1)
	source = strings.Replace(source, "\t\"example.com/g/game/components/activity\"\n", "", 1)
	writeProjectFile(t, root, "game/entities/guild/entity.go", source)
	if _, err := runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "game", "entities", "guild", "guild_event_gen.go")); !os.IsNotExist(err) {
		t.Errorf("guild_event_gen.go survived losing its last listener: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "game", "entities", "player", "player_event_gen.go")); err != nil {
		t.Errorf("player_event_gen.go removed: %v", err)
	}
	// view 里不剩 Holder：holder_entities_gen.go 删掉。
	matches, err := filepath.Glob(filepath.Join(root, "game", "view", "component_*.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range matches {
		if err := os.Remove(path); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := runEntities(t, root); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(root, "game", "view", holderEntitiesFile)); !os.IsNotExist(err) {
		t.Errorf("%s survived the last Holder: %v", holderEntitiesFile, err)
	}
}
