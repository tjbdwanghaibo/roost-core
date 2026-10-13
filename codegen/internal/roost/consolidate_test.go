package roost

import (
	"bytes"
	"go/ast"
	"go/format"
	"go/parser"
	"go/token"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func writeProjectFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const legacyGoMod = `module example.com/planet

go 1.27.0

require (
	github.com/tjbdwanghaibo/roost-core v1.12.0
	github.com/tjbdwanghaibo/roost-kit v1.12.6
	github.com/tjbdwanghaibo/roost-skill v1.10.3
	github.com/tjbdwanghaibo/roost-service v1.5.4
)
`

// The upgrader must move every relocated import, decide split packages per
// symbol, keep the identifier a file already uses when the package name
// changes, and leave files on the new layout alone. It maps package paths
// only: a symbol that changed home or name inside the merge (nats.Permanent,
// syncstream.HealthOptions) is left for the compiler to report — there is no
// per-symbol table for the first two stages (TROUBLESHOOTING T-45). The
// layout stage is different: v1.16.x projects use symbols that were deleted
// or moved package, so it keeps a removed-symbol table and upgrade fails with
// file:line and a guide (RR-20260926-24 复核残留,
// TestConsolidationReportsRemovedSymbolsInsteadOfSucceeding).
func TestConsolidateProjectRewritesImportsGoModAndManifest(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	m := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	m.Versions.Skill = "v1.10.3"
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ManifestName, string(raw))
	mixed := writeProjectFile(t, root, "internal/wiring/wiring.go", `package wiring

import (
	"github.com/tjbdwanghaibo/roost-kit/dataengine"
	kitnats "github.com/tjbdwanghaibo/roost-kit/nats"
	kitredis "github.com/tjbdwanghaibo/roost-kit/redis"
	"github.com/tjbdwanghaibo/roost-kit/syncstream"
	"github.com/tjbdwanghaibo/roost-service/servicemods"
	"github.com/tjbdwanghaibo/roost-skill/skill"
)

var (
	_ = skill.Program{}
	_ = kitredis.NewRedisMod
	_ = kitredis.NewClient
	_ = kitnats.NewClient
	_ = kitnats.Permanent
	_ = dataengine.NewEntityRepository
	_ = syncstream.HealthOptions{}
	_ = servicemods.ModMail
)
`)
	// Stage one left this file alone (mods stayed in kit); stage two moves it,
	// because kit itself is no longer a module (三仓合一仓).
	staged := writeProjectFile(t, root, "internal/fresh/fresh.go", `package fresh

import (
	"github.com/tjbdwanghaibo/roost-core/gameplay/skill"
	kitmods "github.com/tjbdwanghaibo/roost-kit/mods"
)

var (
	_ = skill.Program{}
	_ = kitmods.ModBus
)
`)

	var stdout bytes.Buffer
	result, err := ConsolidateProject(root, false, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 2 || result.Files[0] != "internal/fresh/fresh.go" || result.Files[1] != "internal/wiring/wiring.go" || !result.GoMod || !result.Manifest {
		t.Fatalf("result = %+v", result)
	}
	rewritten, _ := os.ReadFile(mixed)
	for _, want := range []string{
		`dataengine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"`, // package name changes: keep the identifier
		`kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"`,                  // Mod glue stays in kit — and kit itself moved
		`coreredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis/driver"`,   // moved symbols get a second import (driver subpackage)
		`_ = coreredis.NewClient`,
		`_ = kitredis.NewRedisMod`,
		`"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"`,
		`syncstream.HealthOptions{}`,                                    // no symbol table: the compiler reports the new name
		`servicemods "github.com/tjbdwanghaibo/roost-core/wiring/mods"`, // folded package: keep the identifier
		`"github.com/tjbdwanghaibo/roost-core/gameplay/skill"`,
		`kitnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"`, // whole import moves to the driver
		`_ = kitnats.Permanent`,                                                   // contract symbol: left for the compiler, no second import is invented
		`_ = kitnats.NewClient`,
	} {
		if !strings.Contains(string(rewritten), want) {
			t.Errorf("rewritten file missing %q:\n%s", want, rewritten)
		}
	}
	for _, bad := range []string{"roost-skill", "roost-service", "roost-kit/dataengine", "roost-kit/syncstream", "natscontract", "PublisherHealthOptions",
		// After stage two nothing may still name the kit module.
		"tjbdwanghaibo/roost-kit"} {
		if strings.Contains(string(rewritten), bad) {
			t.Errorf("rewritten file still mentions %q:\n%s", bad, rewritten)
		}
	}
	// Stage two moved this one: its only pre-consolidation import was the Mod
	// glue package, which stage one left in kit.
	staged4, _ := os.ReadFile(staged)
	if !strings.Contains(string(staged4), `kitmods "github.com/tjbdwanghaibo/roost-core/wiring/mods"`) {
		t.Errorf("the kit-only file was not moved to the single module:\n%s", staged4)
	}
	if !strings.Contains(string(staged4), `"github.com/tjbdwanghaibo/roost-core/gameplay/skill"`) {
		t.Errorf("a path already on the final layout was disturbed:\n%s", staged4)
	}
	goMod, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	for _, bad := range []string{"roost-skill", "roost-service", "roost-kit"} {
		if strings.Contains(string(goMod), bad) {
			t.Errorf("go.mod still requires %s:\n%s", bad, goMod)
		}
	}
	// Versions are left to the dependency resolution step: writing an
	// unpublished boundary release here would break the very go get that
	// follows (upgrade-compat caught exactly that).
	if want := "roost-core v1.12.0"; !strings.Contains(string(goMod), want) {
		t.Errorf("go.mod versions must be untouched (%q):\n%s", want, goMod)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Versions.Skill != "" || manifest.Versions.Service != "" {
		t.Fatalf("manifest kept the removed module policies: %+v", manifest.Versions)
	}
	if !strings.Contains(stdout.String(), "rewrote 2 Go file(s), go.mod, roost.yaml") {
		t.Fatalf("report = %q", stdout.String())
	}

	// Idempotent: a second run finds nothing to do.
	again, err := ConsolidateProject(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Files) != 0 || again.GoMod || again.Manifest {
		t.Fatalf("second run was not a no-op: %+v", again)
	}
}

// --dry-run reports the same plan without touching a single file.
func TestConsolidateProjectDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	file := writeProjectFile(t, root, "x.go", "package x\n\nimport \"github.com/tjbdwanghaibo/roost-skill/skill\"\n\nvar _ = skill.Program{}\n")
	before, _ := os.ReadFile(file)
	result, err := ConsolidateProject(root, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || !result.GoMod {
		t.Fatalf("dry run plan = %+v", result)
	}
	after, _ := os.ReadFile(file)
	goMod, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	if !bytes.Equal(before, after) || string(goMod) != legacyGoMod {
		t.Fatal("dry run modified the project")
	}
}

// An import on a removed module that the map does not know is an error, not
// a silent leftover that fails at go build time.
func TestConsolidateProjectRefusesUnmappedRemovedImports(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	writeProjectFile(t, root, "x.go", "package x\n\nimport \"github.com/tjbdwanghaibo/roost-skill/internal/wire\"\n\nvar _ = wire.Thing\n")
	_, err := ConsolidateProject(root, false, nil)
	if err == nil || !strings.Contains(err.Error(), "roost-skill/internal/wire") {
		t.Fatalf("unmapped removed import accepted: %v", err)
	}
}

// The embedded relocation map itself must stay coherent: every rename points
// at a mapped package, and its boundary sits at or below the generator floor.
//
// The two used to be required equal, and that stopped being true: the
// boundary is the version where the two-module layout arrived (below it, a
// project needs `upgrade --consolidate` to rewrite imports), while the floor
// is the oldest framework the CURRENT generated code compiles against. The
// generators moved on; the layout did not. What must hold is the direction —
// a floor below the boundary would promise support for a layout this
// generator can no longer emit for.
func TestConsolidationMapMatchesTheGeneratorFloor(t *testing.T) {
	table, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		name            string
		boundary, floor string
	}{
		{"core", m.Boundary.Core, minimumVersions.Core},
		{"kit", m.Boundary.Kit, minimumVersions.Kit},
		// The codegen boundary has no floor of its own any more: the
		// generator runs at versions.core (RR-20261006-57).
		{"codegen", m.Boundary.Codegen, minimumVersions.Core},
	} {
		major, minor, patch, ok := releaseVersion(pair.boundary)
		if !ok {
			t.Fatalf("%s boundary %q is not a release version", pair.name, pair.boundary)
		}
		if !versionAtLeast(pair.floor, major, minor, patch) {
			t.Fatalf("%s floor %s is below the consolidation boundary %s", pair.name, pair.floor, pair.boundary)
		}
	}
	for _, old := range []string{"github.com/tjbdwanghaibo/roost-skill/skill", "github.com/tjbdwanghaibo/roost-service/mail", "github.com/tjbdwanghaibo/roost-kit/nestwal", "github.com/tjbdwanghaibo/roost-kit/redis"} {
		if _, ok := table[old]; !ok {
			t.Errorf("map lacks %s", old)
		}
	}
}

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
	_ = lockstep.NewMatch
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

func TestConsolidationMapSyncBusLegacyPaths(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	file := writeProjectFile(t, root, "wiring.go", `package wiring
import (
 oldkit "github.com/tjbdwanghaibo/roost-core/wiring/room"
 olddriver "github.com/tjbdwanghaibo/roost-core/room"
)
var _ = oldkit.NewRoomMod
var _ oldkit.RoomMod
var _ = olddriver.NewNatsSyncBus
`)
	_, err := ConsolidateProject(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"roost-core/wiring/syncbus", "roost-core/framework/sync/syncbus/driver", "oldkit.NewSyncBusMod", "oldkit.SyncBusMod"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
}

// 改写后包名变化的 import（roost-kit/room → kit/syncbus，包名 room → syncbus），原来不带别名的要补上文件在用的别名，
// 否则 `room.NewSyncBusMod` 变成 `undefined: room`。
func TestConsolidationKeepsTheFilesNameForARenamedKitPackage(t *testing.T) {
	cases := map[string]struct {
		src  string
		want []string
	}{
		"oldkit-kitonly-unaliased": {
			src: `package wiring
import "github.com/tjbdwanghaibo/roost-kit/room"
var _ = room.NewRoomMod(0)
`,
			want: []string{`room "github.com/tjbdwanghaibo/roost-core/wiring/syncbus"`, "room.NewSyncBusMod(0)"},
		},
		"oldkit-mixed-unaliased": {
			src: `package wiring
import (
	"github.com/tjbdwanghaibo/roost-kit/room"
)
var _ = room.NewRoomMod(0)
var _ = room.NewNatsSyncBus
`,
			want: []string{`room "github.com/tjbdwanghaibo/roost-core/wiring/syncbus"`, "room.NewSyncBusMod(0)", `"github.com/tjbdwanghaibo/roost-core/framework/sync/syncbus/driver"`},
		},
		"corekitroom-unaliased": {
			src: `package wiring
import "github.com/tjbdwanghaibo/roost-core/wiring/room"
var _ = room.NewRoomMod(0)
`,
			want: []string{`room "github.com/tjbdwanghaibo/roost-core/wiring/syncbus"`, "room.NewSyncBusMod(0)"},
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			writeProjectFile(t, root, "go.mod", legacyGoMod)
			file := writeProjectFile(t, root, "wiring.go", tc.src)
			if _, err := ConsolidateProject(root, false, nil); err != nil {
				t.Fatal(err)
			}
			raw, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			assertPackageQualifiersResolve(t, raw)
			for _, want := range tc.want {
				if !strings.Contains(string(raw), want) {
					t.Fatalf("missing %s:\n%s", want, raw)
				}
			}
		})
	}
}

// assertPackageQualifiersResolve 检查每个 `X.Sel` 的包限定符 X 都由某个 import 声明：
// 显式别名，或该路径的真实包名（迁移表 layout 阶段登记的 package，否则取路径末段）。
func assertPackageQualifiersResolve(t *testing.T, src []byte) {
	t.Helper()
	_, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	packageNames := map[string]string{}
	for _, move := range m.Layout.Move {
		if move.Package != "" {
			packageNames[move.To] = move.Package
		}
	}
	file, err := parser.ParseFile(token.NewFileSet(), "wiring.go", src, 0)
	if err != nil {
		t.Fatalf("rewritten file does not parse: %v\n%s", err, src)
	}
	declared := map[string]bool{}
	for _, imp := range file.Imports {
		importPath, _ := strconv.Unquote(imp.Path.Value)
		switch {
		case imp.Name != nil:
			declared[imp.Name.Name] = true
		case packageNames[importPath] != "":
			declared[packageNames[importPath]] = true
		default:
			declared[path.Base(importPath)] = true
		}
	}
	ast.Inspect(file, func(n ast.Node) bool {
		sel, ok := n.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		if id, ok := sel.X.(*ast.Ident); ok && id.Obj == nil && !declared[id.Name] {
			t.Errorf("undefined: %s (in %s.%s)\n%s", id.Name, id.Name, sel.Sel.Name, src)
		}
		return true
	})
}

// 无法机械改写的符号（已删除 / 改名 / 换包，如 spatial.InterestEvent、statesync.Reassembler）由 upgrade 自己列出
// 文件:行 与迁移指引并以错误退出，不能改完 import 就以 0 退出、留给编译器报 `undefined`；用户改完重跑即通过。
func TestConsolidationReportsRemovedSymbolsInsteadOfSucceeding(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", "module example.com/planet\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v1.16.1\n")
	writeProjectFile(t, root, "game/scene/scene.go", `package scene

import "github.com/tjbdwanghaibo/roost-core/infra/base/spatial"

type Scene struct {
	events []spatial.InterestEvent
	point  spatial.Vec2
}
`)
	writeProjectFile(t, root, "cmd/loadtest/main.go", `package main

import (
	corestate "github.com/tjbdwanghaibo/roost-core/statesync"
	"github.com/tjbdwanghaibo/roost-core/room"
)

var reassembler = corestate.NewReassembler
var session corestate.SessionID

func main() { _, _ = room.DecodeRoomWireFrame(nil) }
`)
	var stdout strings.Builder
	_, err := ConsolidateProject(root, false, &stdout)
	if err == nil {
		t.Fatalf("upgrade reported success although the project still uses removed symbols:\n%s", stdout.String())
	}
	for _, want := range []string{
		"game/scene/scene.go:6: spatial.InterestEvent",
		"cmd/loadtest/main.go:8: corestate.NewReassembler",
		"cmd/loadtest/main.go:9: corestate.SessionID",
		"cmd/loadtest/main.go:11: room.DecodeRoomWireFrame",
		"policy",       // spatial → sync/entitysync/policy
		"nettransport", // statesync.SessionID → sync/nettransport
		"entitysync.DecodeFrame",
		"CHANGELOG",
	} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("report does not mention %q:\n%v", want, err)
		}
	}
	if strings.Contains(err.Error(), "spatial.Vec2") {
		t.Errorf("a symbol that still exists was reported:\n%v", err)
	}
	// The mechanical part is done either way, so a rerun only has the manual
	// part left.
	raw, err := os.ReadFile(filepath.Join(root, "cmd/loadtest/main.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"`) {
		t.Fatalf("imports were not rewritten:\n%s", raw)
	}
	// The user follows the guide; the next run succeeds.
	writeProjectFile(t, root, "cmd/loadtest/main.go", `package main

import "github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"

func main() { _ = entitysync.DecodeFrame }
`)
	writeProjectFile(t, root, "game/scene/scene.go", `package scene

import (
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
)

type Scene struct {
	events []policy.InterestEvent
	point  spatial.Vec2
}
`)
	if _, err := ConsolidateProject(root, false, nil); err != nil {
		t.Fatalf("a project with no removed symbols left still fails: %v", err)
	}
}

// 迁移表里每个“已删除”符号在它登记的新路径上确实不存在，且每组都有指引——表不能列出
// 仍然存在的符号（会误报），也不能是一句空话。
func TestConsolidationRemovedSymbolsAreReallyGone(t *testing.T) {
	_, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Layout.Removed) == 0 {
		t.Fatal("the layout stage lists no removed symbols")
	}
	for _, group := range m.Layout.Removed {
		if strings.TrimSpace(group.Guide) == "" || len(group.Symbols) == 0 {
			t.Errorf("%s: a removed-symbol group needs a guide and symbols", group.Package)
		}
		dir := filepath.Join("..", "..", "..", filepath.FromSlash(strings.TrimPrefix(group.Package, "github.com/tjbdwanghaibo/roost-core/")))
		exported := exportedTopLevel(t, dir)
		for _, symbol := range group.Symbols {
			if exported[symbol] {
				t.Errorf("%s.%s is listed as removed but still exists", group.Package, symbol)
			}
		}
	}
}

func exportedTopLevel(t *testing.T, dir string) map[string]bool {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatalf("%s: %v", dir, err)
	}
	out := map[string]bool{}
	fset := token.NewFileSet()
	for _, entry := range entries {
		name := entry.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, parser.SkipObjectResolution)
		if err != nil {
			t.Fatal(err)
		}
		for _, decl := range file.Decls {
			switch decl := decl.(type) {
			case *ast.FuncDecl:
				if decl.Recv == nil {
					out[decl.Name.Name] = true
				}
			case *ast.GenDecl:
				for _, spec := range decl.Specs {
					switch spec := spec.(type) {
					case *ast.TypeSpec:
						out[spec.Name.Name] = true
					case *ast.ValueSpec:
						for _, id := range spec.Names {
							out[id.Name] = true
						}
					}
				}
			}
		}
	}
	return out
}
