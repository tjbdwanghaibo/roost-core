package roost

import (
	"bytes"
	"go/format"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLayoutStageMovesTheSyncBlock(t *testing.T) {
	_, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Layout.Move) == 0 || m.Layout.Boundary.Core == "" {
		t.Fatalf("map has no layout stage: %+v", m.Layout)
	}
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "go.mod"), []byte(
		"module example.com/planet\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v1.16.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if needsConsolidation(root) {
		t.Fatal("a project with no Go files was reported as needing consolidation")
	}
	source := filepath.Join(root, "wiring.go")
	if err := os.WriteFile(source, []byte(`package wiring

import (
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/entitysync"
	"github.com/tjbdwanghaibo/roost-core/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/lockstep"
	"github.com/tjbdwanghaibo/roost-core/mirror"
	kitnet "github.com/tjbdwanghaibo/roost-core/nettransport"
	"github.com/tjbdwanghaibo/roost-core/statesync"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
	"github.com/tjbdwanghaibo/roost-core/syncbus/driver"
)

var (
	_ = entity.EntityKindNone
	_ = entitysync.NewManager
	_ = policy.NewGroup
	_ = lockstep.NewRoom
	_ = mirror.New
	_ = kitnet.NewAsyncTransport
	_ = statesync.DefaultLimits
	_ fsyncbus.ISyncBus
	_ = driver.NewNATSSyncBus
)
`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !needsConsolidation(root) {
		t.Fatal("a project importing pre-layout sync paths was not reported as needing consolidation")
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
		`"github.com/tjbdwanghaibo/roost-core/framework/entity"`, // 不在块内，不动
		`"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"`,
		`"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"`,
		`"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep"`,
		`"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror"`,
		`kitnet "github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport"`,
		// 包名变了：文件里仍叫 statesync，所以补显式别名
		`statesync "github.com/tjbdwanghaibo/roost-core/framework/sync/frame"`,
		`fsyncbus "github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus"`,
		`"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"`,
	} {
		if !strings.Contains(text, want) {
			t.Errorf("rewritten file lacks %q:\n%s", want, text)
		}
	}
	for _, stale := range []string{`roost-core/entitysync"`, `roost-core/statesync"`, `roost-core/nettransport"`, `roost-core/lockstep"`, `roost-core/mirror"`, `roost-core/syncbus"`, `roost-core/syncbus/driver"`} {
		if strings.Contains(text, stale) {
			t.Errorf("an import still names the pre-layout path %s:\n%s", stale, text)
		}
	}
	if needsConsolidation(root) {
		t.Fatal("a rewritten project still reports needing consolidation")
	}
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

// 每条 layout 规则一个用例，子包跟着走，不该匹配的不匹配。
func TestLayoutPathRules(t *testing.T) {
	_, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	moved := map[string][2]string{
		"github.com/tjbdwanghaibo/roost-core/entitysync":        {"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync", ""},
		"github.com/tjbdwanghaibo/roost-core/entitysync/policy": {"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy", ""},
		"github.com/tjbdwanghaibo/roost-core/statesync":         {"github.com/tjbdwanghaibo/roost-core/framework/sync/frame", "frame"},
		"github.com/tjbdwanghaibo/roost-core/nettransport":      {"github.com/tjbdwanghaibo/roost-core/framework/sync/nettransport", ""},
		"github.com/tjbdwanghaibo/roost-core/lockstep":          {"github.com/tjbdwanghaibo/roost-core/framework/sync/lockstep", ""},
		"github.com/tjbdwanghaibo/roost-core/syncbus":           {"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus", ""},
		"github.com/tjbdwanghaibo/roost-core/syncbus/driver":    {"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver", ""},
		"github.com/tjbdwanghaibo/roost-core/mirror":            {"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/mirror", ""},
	}
	for from, want := range moved {
		got, pkg, ok := layoutPath(from, m)
		if !ok || got != want[0] || pkg != want[1] {
			t.Errorf("layoutPath(%q) = (%q, %q, %v), want (%q, %q)", from, got, pkg, ok, want[0], want[1])
		}
	}
	for _, untouched := range []string{
		"context",
		"github.com/tjbdwanghaibo/roost-core/framework/entity",
		"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream", // 基建，不在块内
		"github.com/tjbdwanghaibo/roost-core/infra/base/spatial",
		"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync", // 已在新位置
		"github.com/tjbdwanghaibo/roost-core/wiring/syncbus",            // kit 胶水不动
	} {
		if got, _, ok := layoutPath(untouched, m); ok || got != untouched {
			t.Errorf("layoutPath(%q) = %q (moved=%v), want it left alone", untouched, got, ok)
		}
	}
}

func TestConsolidateSplitsASingleLineImportIntoAValidSecondDeclaration(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	m := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ManifestName, string(raw))
	consumer := writeProjectFile(t, root, "internal/consumer/consumer.go",
		"package consumer\n\nimport \"github.com/tjbdwanghaibo/roost-kit/redis\"\n\nvar _ = redis.NewRedisMod\nvar _ = redis.NewClient\n")
	if _, err := ConsolidateProject(root, false, nil); err != nil {
		t.Fatalf("single-line import split rejected: %v", err)
	}
	rewritten, err := os.ReadFile(consumer)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := format.Source(rewritten); err != nil {
		t.Fatalf("rewritten file does not format: %v\n%s", err, rewritten)
	}
	text := string(rewritten)
	for _, want := range []string{
		// The Mod glue stays in kit, and kit is now a directory inside core.
		`"github.com/tjbdwanghaibo/roost-core/wiring/redis"`,
		`coreredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"`,
		"redis.NewRedisMod",
		"coreredis.NewClient",
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("rewritten file lacks %q:\n%s", want, text)
		}
	}
	_ = filepath.Join
}

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
		`"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"`,
		// 第一阶段留在 kit 的，第二阶段搬到 core/kit
		`kitmods "github.com/tjbdwanghaibo/roost-core/wiring/mods"`,
		`svcmail "github.com/tjbdwanghaibo/roost-core/wiring/mail"`,
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
		"github.com/tjbdwanghaibo/roost-kit":                    "github.com/tjbdwanghaibo/roost-core/wiring",
		"github.com/tjbdwanghaibo/roost-kit/mods":               "github.com/tjbdwanghaibo/roost-core/wiring/mods",
		"github.com/tjbdwanghaibo/roost-kit/service/mail":       "github.com/tjbdwanghaibo/roost-core/wiring/mail",
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
		"github.com/tjbdwanghaibo/roost-core/framework/entity",
		"github.com/tjbdwanghaibo/roost-core/wiring/mods", // already there
		"github.com/tjbdwanghaibo/roost-kitchen/sink",     // 前缀相同但不是那个模块
	} {
		if got, ok := singleModulePath(untouched, m); ok || got != untouched {
			t.Errorf("singleModulePath(%q) = %q (moved=%v), want it left alone", untouched, got, ok)
		}
	}
}
