package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// ARCH-12 S3（M-17）：第三阶段——sync 块收进 sync/。它跑在前两个阶段的结果上，
// 一个已经在单模块布局上的工程只因为老的 sync 路径也要被改写；statesync 改名
// frame 时文件里的标识符保持不变（加显式别名），改了名的符号留给编译器指出。
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
	"github.com/tjbdwanghaibo/roost-core/entity"
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
		`"github.com/tjbdwanghaibo/roost-core/entity"`, // 不在块内，不动
		`"github.com/tjbdwanghaibo/roost-core/sync/entitysync"`,
		`"github.com/tjbdwanghaibo/roost-core/sync/entitysync/policy"`,
		`"github.com/tjbdwanghaibo/roost-core/sync/lockstep"`,
		`"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror"`,
		`kitnet "github.com/tjbdwanghaibo/roost-core/sync/nettransport"`,
		// 包名变了：文件里仍叫 statesync，所以补显式别名
		`statesync "github.com/tjbdwanghaibo/roost-core/sync/frame"`,
		`fsyncbus "github.com/tjbdwanghaibo/roost-core/sync/syncbus"`,
		`"github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver"`,
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
		"github.com/tjbdwanghaibo/roost-core/entitysync":        {"github.com/tjbdwanghaibo/roost-core/sync/entitysync", ""},
		"github.com/tjbdwanghaibo/roost-core/entitysync/policy": {"github.com/tjbdwanghaibo/roost-core/sync/entitysync/policy", ""},
		"github.com/tjbdwanghaibo/roost-core/statesync":         {"github.com/tjbdwanghaibo/roost-core/sync/frame", "frame"},
		"github.com/tjbdwanghaibo/roost-core/nettransport":      {"github.com/tjbdwanghaibo/roost-core/sync/nettransport", ""},
		"github.com/tjbdwanghaibo/roost-core/lockstep":          {"github.com/tjbdwanghaibo/roost-core/sync/lockstep", ""},
		"github.com/tjbdwanghaibo/roost-core/syncbus":           {"github.com/tjbdwanghaibo/roost-core/sync/syncbus", ""},
		"github.com/tjbdwanghaibo/roost-core/syncbus/driver":    {"github.com/tjbdwanghaibo/roost-core/sync/syncbus/driver", ""},
		"github.com/tjbdwanghaibo/roost-core/mirror":            {"github.com/tjbdwanghaibo/roost-core/sync/syncbus/mirror", ""},
	}
	for from, want := range moved {
		got, pkg, ok := layoutPath(from, m)
		if !ok || got != want[0] || pkg != want[1] {
			t.Errorf("layoutPath(%q) = (%q, %q, %v), want (%q, %q)", from, got, pkg, ok, want[0], want[1])
		}
	}
	for _, untouched := range []string{
		"context",
		"github.com/tjbdwanghaibo/roost-core/entity",
		"github.com/tjbdwanghaibo/roost-core/syncstream", // 基建，不在块内
		"github.com/tjbdwanghaibo/roost-core/spatial",
		"github.com/tjbdwanghaibo/roost-core/sync/entitysync", // 已在新位置
		"github.com/tjbdwanghaibo/roost-core/kit/syncbus",     // kit 胶水不动
	} {
		if got, _, ok := layoutPath(untouched, m); ok || got != untouched {
			t.Errorf("layoutPath(%q) = %q (moved=%v), want it left alone", untouched, got, ok)
		}
	}
}
