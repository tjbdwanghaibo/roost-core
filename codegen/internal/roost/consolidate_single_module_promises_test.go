package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 三仓合一仓 P4：单模块阶段跑在包映射表之后，不是之前。
//
// 第一阶段（五仓合三仓）把 kit 的大部分实现搬进了 core 本体
// （roost-kit/dataengine → roost-core/dataengine），只有 Mod 胶水与 service/
// 留在 kit。第二阶段搬的是**留下来的那部分**。顺序反过来的话
// roost-kit/dataengine 会被前缀送到 roost-core/kit/dataengine——那里没有这个包。
func TestSingleModuleStageRunsAfterThePackageTable(t *testing.T) {
	_, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	if m.Schema < 2 {
		t.Fatalf("map schema = %d, want >= 2 (single_module section)", m.Schema)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(
		"module example.com/legacy\n\ngo 1.27.0\n\nrequire (\n\tgithub.com/tjbdwanghaibo/roost-core v1.12.0\n\tgithub.com/tjbdwanghaibo/roost-kit v1.12.6\n)\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	source := filepath.Join(root, "wiring.go")
	if err := os.WriteFile(source, []byte(`package wiring

import (
	"github.com/tjbdwanghaibo/roost-kit/dataengine"
	kitmods "github.com/tjbdwanghaibo/roost-kit/mods"
	svcmail "github.com/tjbdwanghaibo/roost-kit/service/mail"
)

var (
	_ = dataengine.NewEntityRepository
	_ = kitmods.ModBus
	_ = svcmail.Mail(nil)
)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ConsolidateProject(root, false, nil); err != nil {
		t.Fatal(err)
	}
	rewritten, err := os.ReadFile(source)
	if err != nil {
		t.Fatal(err)
	}
	text := string(rewritten)
	for _, want := range []string{
		// 第一阶段搬进 core 本体的，第二阶段不许再动它
		`"github.com/tjbdwanghaibo/roost-core/dataengine/engine"`,
		// 第一阶段留在 kit 的，第二阶段搬到 core/kit
		`kitmods "github.com/tjbdwanghaibo/roost-core/kit/mods"`,
		`svcmail "github.com/tjbdwanghaibo/roost-core/kit/service/mail"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rewritten file lacks %q:\n%s", want, text)
		}
	}
	// 顺序错了就会出现这个——前缀先跑，dataengine 被送进 kit
	if strings.Contains(text, "roost-core/kit/dataengine") {
		t.Errorf("the prefix stage ran before the package table:\n%s", text)
	}
	if strings.Contains(text, "tjbdwanghaibo/roost-kit") {
		t.Errorf("an import still names the kit module:\n%s", text)
	}
	goMod, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	if strings.Contains(string(goMod), "roost-kit") {
		t.Errorf("go.mod still requires the kit module:\n%s", goMod)
	}

	// 幂等：已经在最终形态上的工程，再跑一次什么都不动。
	before, _ := os.ReadFile(source)
	result, err := ConsolidateProject(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	after, _ := os.ReadFile(source)
	if len(result.Files) != 0 || !bytes.Equal(before, after) {
		t.Fatalf("a second run changed something: %+v", result)
	}
}

// 每条前缀规则至少一个用例，外加"不该匹配的不要匹配"。
func TestSingleModulePathRules(t *testing.T) {
	_, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	moved := map[string]string{
		"github.com/tjbdwanghaibo/roost-kit":                    "github.com/tjbdwanghaibo/roost-core/kit",
		"github.com/tjbdwanghaibo/roost-kit/mods":               "github.com/tjbdwanghaibo/roost-core/kit/mods",
		"github.com/tjbdwanghaibo/roost-kit/service/mail":       "github.com/tjbdwanghaibo/roost-core/kit/service/mail",
		"github.com/tjbdwanghaibo/roost-codegen/internal/roost": "github.com/tjbdwanghaibo/roost-core/codegen/internal/roost",
		"github.com/tjbdwanghaibo/roost-codegen/cmd/roost":      "github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost",
	}
	for from, want := range moved {
		got, ok := singleModulePath(from, m)
		if !ok || got != want {
			t.Errorf("singleModulePath(%q) = %q (moved=%v), want %q", from, got, ok, want)
		}
	}
	for _, untouched := range []string{
		"context",
		"github.com/tjbdwanghaibo/roost-core/entity",
		"github.com/tjbdwanghaibo/roost-core/kit/mods", // already there
		"github.com/tjbdwanghaibo/roost-kitchen/sink",  // 前缀相同但不是那个模块
	} {
		if got, ok := singleModulePath(untouched, m); ok || got != untouched {
			t.Errorf("singleModulePath(%q) = %q (moved=%v), want it left alone", untouched, got, ok)
		}
	}
}
