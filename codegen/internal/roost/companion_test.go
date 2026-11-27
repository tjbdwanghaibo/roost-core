package roost

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 两个普通的本地实体；主业务代码故意依赖子包。上层若将关系注册进子包，生成工程编译即出现导入环。
func TestGenerateLocalCompanion(t *testing.T) {
	checkGeneratedCompanion(t, "")
}

// 分别验证只有主端、只有子端 Remote；测试只运行生成的类型与内存 manager，不启动 Remote 服务。
func TestGenerateRemoteCompanion(t *testing.T) {
	for _, remoteKind := range []string{"Archive", "Journal"} {
		t.Run(remoteKind, func(t *testing.T) { checkGeneratedCompanion(t, remoteKind) })
	}
}

func checkGeneratedCompanion(t *testing.T, remoteKind string) {
	t.Helper()
	mods := []string{"nest"}
	if remoteKind != "" {
		mods = append(mods, "remote_entity")
	}
	const module = "example.com/companions"
	_, root, err := NewProject(NewOptions{Name: "companions", Module: module, Out: filepath.Join(t.TempDir(), "companions"), Services: []string{"game"}, Mods: mods, Features: []string{"entity", "nest"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"Journal", "Archive"} {
		if _, err := Add(root, AddOptions{Kind: "entity", Name: name}); err != nil {
			t.Fatal(err)
		}
	}
	write := func(rel, src string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(root, filepath.FromSlash(rel)), []byte(src), 0644); err != nil {
			t.Fatal(err)
		}
	}
	if remoteKind != "" {
		rel := "game/entities/" + strings.ToLower(remoteKind) + "/entity.go"
		source := readProjectFile(t, root, rel)
		source = strings.Replace(source, "category=entity.EntityCategoryOther", "category=entity.EntityCategoryRemote remote=managed", 1)
		write(rel, strings.Replace(source, "*entity.EntityBase", "*entity.RemoteEntityBase", 1))
	}
	write("game/entities/archive/business.go", `package archive
import "example.com/companions/game/entities/journal"
func (*Archive) ChildKindName(*journal.Journal) string { return "journal" }
`)
	generate := func() {
		t.Helper()
		if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
			t.Fatal(err)
		}
	}
	snapshot := func() map[string][32]byte {
		t.Helper()
		snap, err := snapshotGenerated(root)
		if err != nil {
			t.Fatal(err)
		}
		return snap
	}
	generate()
	base := snapshot()
	const childSource = "game/entities/journal/entity.go"
	original := readProjectFile(t, root, childSource)
	marked := strings.Replace(original, "//roost:entity ", "//roost:entity companionOf=Archive ", 1)
	if marked == original {
		t.Fatal("missing entity marker")
	}
	write(childSource, marked)
	generate()
	enabled := snapshot()
	changed := diffSnapshot(base, enabled)
	if len(changed) != 2 || !strings.Contains(strings.Join(changed, "\n"), "game/entities/journal/journal_gen_wire.go") || !strings.Contains(strings.Join(changed, "\n"), "internal/registry/generated.go") {
		t.Fatalf("enabling a relation must change only the child wire and the aggregate: %v", changed)
	}
	registry := readProjectFile(t, root, "internal/registry/generated.go")
	registration := "entity.RegisterCompanionKind[*archive.Archive, *journal.Journal](archive.EntityKindArchive, journal.EntityKindJournal)"
	builder, post := strings.Index(registry, "journal.RegisterEntity()"), strings.Index(registry, "// phase: post")
	if builder < 0 || builder >= post || strings.Index(registry, registration) < post {
		t.Fatalf("pair registration must run in post, after builder registration:\n%s", registry)
	}
	generate()
	if delta := diffSnapshot(enabled, snapshot()); len(delta) != 0 {
		t.Fatalf("second generation changed %v", delta)
	}
	if !testing.Short() {
		fixture, err := os.ReadFile("testdata/companion/runtime.go.txt")
		if err != nil {
			t.Fatal(err)
		}
		write("internal/registry/companion_test.go", string(fixture))
		useLocalRoostCore(t, root)
		cmd := exec.Command("go", "test", "-count=1", "./...")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off", "COMPANION_FIXTURE_REMOTE_KIND="+remoteKind)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated project: %v\n%s", err, out)
		}
	}
	write(childSource, original)
	generate()
	if delta := diffSnapshot(base, snapshot()); len(delta) != 0 {
		t.Fatalf("removing the opt-in did not restore the original generated outputs: %v", delta)
	}
}
