package roost

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/attribute"
)

// attribute feature 的 runtime.go 是 codegen 受控文件，必须以 canOverwriteGenerated 认的生成标记开头：
// 否则 `project doctor` 的 project-templates 检查把它当成应用自有文件，报 "refusing to overwrite non-generated file"。
func TestAttributeRuntimeFileIsMarkedGenerated(t *testing.T) {
	body := renderAttributeRuntime("attribute")
	if !strings.HasPrefix(body, generatedHeader) {
		t.Fatalf("the attribute runtime file does not start with the generated header:\n%s", body[:min(len(body), 120)])
	}
	if !canOverwriteGenerated("game/gameplay/attribute/runtime.go", []byte(body)) {
		t.Error("doctor would refuse to overwrite the attribute runtime file")
	}
	if !strings.Contains(body, "package attribute") {
		t.Error("the rendered file has no package clause for the requested name")
	}
}

// A generated project with the feature must pass doctor's template check.
func TestGeneratedAttributeProjectPassesTheTemplateCheck(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}, Features: []string{"attribute"}})
	if err != nil {
		t.Fatal(err)
	}
	// This is what `project doctor --strict` runs for the project-templates
	// item: a file codegen owns but cannot recognise as generated fails here.
	var diff bytes.Buffer
	if err := DiffProject(root, &diff); err != nil {
		t.Fatalf("doctor's template check refuses a freshly generated project: %v", err)
	}
	if !strings.Contains(diff.String(), "summary: 0 file(s) would change") {
		t.Errorf("a freshly generated project already has stale templates:\n%s", diff.String())
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// roost add attribute 是属性生成器的上手入口（新用户第七轮：修前 `unsupported artifact kind "attribute"`，只能手改
// roost.yaml 的 features 再 project sync）：开启 feature（写 runtime.go）、写带 dirtyMask 的 profile 骨架并立即生成，
// 生成的包对当前 roost-core 能编译；同名 profile 再 add 拒绝且不改文件。
func TestAddAttributeEnablesFeatureWritesProfileAndGenerates(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the generated attribute package")
	}
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	var out bytes.Buffer
	if err := Run([]string{"add", "attribute", "PlayerProfile", "--root", root}, &out, &out); err != nil {
		t.Fatalf("add attribute: %v\n%s", err, out.String())
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !hasFeature(m, "attribute") {
		t.Errorf("add attribute did not enable the attribute feature: %v", m.Features)
	}
	for _, rel := range []string{
		"game/gameplay/attribute/runtime.go",
		"game/gameplay/attribute/player_profile.go",
		"game/gameplay/attribute/gen_player_profile_attribute.go",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Errorf("add attribute did not write %s: %v\n%s", rel, err, out.String())
		}
	}
	profile := readProjectFile(t, root, "game/gameplay/attribute/player_profile.go")
	for _, want := range []string{"//roost:attribute index=1 max=64", "type PlayerProfile struct", "dirtyMask uint64"} {
		if !strings.Contains(profile, want) {
			t.Errorf("player_profile.go lacks %q:\n%s", want, profile)
		}
	}
	useLocalRoostCore(t, root)
	build := exec.Command("go", "build", "./game/gameplay/attribute")
	build.Dir = root
	build.Env = append(os.Environ(), "GOWORK=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("generated attribute package does not build: %v\n%s", err, output)
	}
	before := readProjectFile(t, root, "game/gameplay/attribute/player_profile.go")
	if err := Run([]string{"add", "attribute", "PlayerProfile", "--root", root}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Errorf("second add attribute PlayerProfile = %v; want already exists", err)
	}
	if after := readProjectFile(t, root, "game/gameplay/attribute/player_profile.go"); after != before {
		t.Error("a refused add attribute changed the profile file")
	}
}

// roost help attribute 的目录与 roost generate 读的一致，示例照抄能被生成器接受（修前目录写 ./game/attribute、
// 示例没有 dirtyMask：`PlayerProfile must declare an unexported `dirtyMask uint64` field`）。
func TestAttributeHelpMatchesTheGenerator(t *testing.T) {
	var topic helpTopic
	for _, candidate := range helpTopics {
		if candidate.Name == "attribute" {
			topic = candidate
		}
	}
	if !strings.Contains(topic.Usage, "./game/gameplay/attribute") || strings.Contains(topic.Usage, "./game/attribute ") {
		t.Errorf("help attribute names a directory roost generate does not read:\n%s", topic.Usage)
	}
	if !strings.Contains(topic.Usage, "roost add attribute") {
		t.Errorf("help attribute does not name the add command:\n%s", topic.Usage)
	}
	dir := t.TempDir()
	source := "package attribute\n\n" + topic.Example + "\n"
	if err := os.WriteFile(filepath.Join(dir, "profile.go"), []byte(source), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := attribute.Run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatalf("the help example is rejected by the attribute generator: %v\n%s", err, source)
	}
}

// feature 打开后只跑 roost generate（没跑 project sync，没有 runtime.go）：报错要指向 project sync，而不是
// 让生成的代码引用 AttrID 等未定义类型、编译时才失败。
func TestGenerateAttributeWithoutRuntimeNamesProjectSync(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	manifestPath := filepath.Join(root, ManifestName)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Features = uniqueSorted(append(m.Features, "attribute"))
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "game", "gameplay", "attribute")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	profile := "package attribute\n\n//roost:attribute index=1 max=64\ntype PlayerProfile struct {\n\tHP int64\n\tdirtyMask uint64\n}\n"
	if err := os.WriteFile(filepath.Join(dir, "player_profile.go"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	err = Generate(root, GenerateOptions{})
	if err == nil || !strings.Contains(err.Error(), "roost project sync") {
		t.Fatalf("generate with the attribute feature but no runtime.go = %v; want an error naming roost project sync", err)
	}
}
