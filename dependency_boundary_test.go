package roostcore_test

import (
	"go/build"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// 结构性约束只验证 import 方向，不替代战斗或空间语义回归。
func TestCombatAndSkillDependencyBoundary(t *testing.T) {
	for _, dir := range []string{"skill/combat", "skill"} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(dir, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range file.Imports {
				name, err := strconv.Unquote(spec.Path.Value)
				if err != nil {
					t.Fatal(err)
				}
				if dir == "skill/combat" {
					pkg, err := build.Default.Import(name, "", build.FindOnly)
					if err != nil || !pkg.Goroot {
						t.Errorf("%s: combat requires standard library only: %s", path, name)
					}
				} else if name == modulePath+"/spatial" || strings.HasPrefix(name, modulePath+"/spatial/") {
					t.Errorf("%s: skill delegates spatial queries to Host: %s", path, name)
				}
			}
		}
	}
}

// Parse all root-module source files, including tests and inactive build tags.
// Nested modules are separate consumers, not part of Core's dependency layer.
func TestCoreDependencyBoundary(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" {
				return filepath.SkipDir
			}
			// testdata is not this module's code. The Go tool does not build
			// it, and under codegen/ it is generated OUTPUT plus the
			// round-trip suites that compile that output against the real
			// runtime — imports there are the generator's product, not its
			// dependencies. A stale path in a golden file is caught by the
			// golden comparison itself, which is the check that owns it.
			if entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				} else if !os.IsNotExist(err) {
					return err
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		layer := moduleLayer(path)
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if forbiddenCoreImport(name) {
				t.Errorf("%s: forbidden Core dependency %s", path, name)
			}
			if reason := layerViolation(layer, name); reason != "" {
				t.Errorf("%s (%s layer): %s: %s", path, layer, reason, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// driverPackages are the client implementations split out of their contract
// packages (mongo, nats, redis, etcd). Contract packages stay free of driver
// dependencies only if nothing inside Core links a driver: assembly happens
// in kit's Mods. Tests may use them freely.
var driverPackages = []string{
	"github.com/tjbdwanghaibo/roost-core/mongo/driver",
	"github.com/tjbdwanghaibo/roost-core/nats/driver",
	"github.com/tjbdwanghaibo/roost-core/redis/driver",
	"github.com/tjbdwanghaibo/roost-core/etcd/driver",
}

// TestCoreContractsDoNotLinkDrivers walks every non-test Go file outside the
// driver packages and refuses an import of a driver package. Keeping the
// contracts light is the reason the drivers live in subpackages at all
// (B-26): a binary that only wants IMongo must not pull in TLS and SCRAM.
//
// The kit layer is exempt, and always was in intent — the sentence above says
// "assembly happens in kit's Mods", and linking a driver is exactly what
// assembly means. While kit is a separate module that exemption is free; once
// it moves into `kit/` this walker would reach it, and every Mod would fail a
// test whose own comment blesses them. Written now, before the move
// (ARCHITECTURE_V3_SINGLE_MODULE_PLAN P1).
func TestCoreContractsDoNotLinkDrivers(t *testing.T) {
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "driver" {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		if moduleLayer(path) == "kit" {
			// Assembly links drivers. That is what it is for.
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			for _, driver := range driverPackages {
				if name == driver {
					t.Errorf("%s: contract-side code links driver package %s; construct clients in kit's Mod instead", path, name)
				}
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// --- 合仓后的层次规则（ARCHITECTURE_V3_SINGLE_MODULE_PLAN §1）---
//
// kit、codegen、demo 会各自作为 roost-core 根目录下的一个顶层目录搬进来。
// 今天"core 不得依赖 kit / codegen"是 Go 模块边界免费保证的；合仓之后没有任何
// 东西保证它，所以这条规则要改由目录前缀表达，**并且要在任何代码搬动之前生效**，
// 否则它在合仓当天就失去执行力。
//
// 这几个目录现在还不存在，所以下面的规则此刻是空转的——这正是它应该被先写下来的
// 原因：等目录出现时判据已经在岗。
const modulePath = "github.com/tjbdwanghaibo/roost-core"

// moduleLayer says which layer a path inside this module belongs to. The path
// may be a file path relative to the module root or an import path's tail.
func moduleLayer(rel string) string {
	rel = strings.TrimPrefix(filepath.ToSlash(rel), "./")
	first := rel
	if index := strings.Index(rel, "/"); index >= 0 {
		first = rel[:index]
	}
	switch first {
	case "kit", "codegen", "demo":
		return first
	default:
		return "core"
	}
}

// layerViolation reports why a file in `layer` may not import `name`, or "" if
// it may. Imports outside this module are not its business.
//
//	core     纯运行时，不知道装配层与工具的存在
//	kit      装配层，可以用 core；不碰生成器
//	codegen  生成器，独立于它生成的那个运行时（这也是 demo 用 .tmpl 的原因之一）；
//	         可以读 demo 的模板；例外只有两个只依赖标准库的叶子包：configdata/rules
//	         （配置表规则的声明与检查只有一份，加载层与 tablegen 共用，B10）与
//	         internal/configschema（服务配置的声明，App、生成器与 doctor 共用，A4 ①），
//	         见 TestSharedConfigRulesStayALeaf
//	demo     模板目录，除 embed 声明外没有可编译代码
func layerViolation(layer, name string) string {
	if name != modulePath && !strings.HasPrefix(name, modulePath+"/") {
		return ""
	}
	target := moduleLayer(strings.TrimPrefix(strings.TrimPrefix(name, modulePath), "/"))
	if target == layer {
		return ""
	}
	switch layer {
	case "core":
		return "core 的包不得 import " + target + " 层（它们是 core 的消费者，不是它的依赖）"
	case "kit":
		if target == "core" {
			return ""
		}
		return "装配层不得 import " + target + " 层"
	case "codegen":
		if target == "demo" || name == sharedConfigRules || name == sharedConfigSchema {
			return ""
		}
		return "生成器保持独立于它生成的运行时，不得 import " + target + " 层"
	case "demo":
		return ""
	}
	return ""
}

// sharedConfigRules is the one core package the generators may import: the
// config rule declaration and check that configdata enforces on every load
// and tablegen runs early (B10). The exception holds only while it is a leaf.
const sharedConfigRules = modulePath + "/configdata/rules"

// sharedConfigSchema is the other one: the service-config declarations every
// Mod writes, which the App checks, the generator renders config sections from
// and doctor checks project configs with (A4 ①). Leaf for the same reason.
const sharedConfigSchema = modulePath + "/internal/configschema"

// TestSharedConfigRulesStayALeaf keeps the codegen exceptions honest: the
// shared rules and schema packages import the standard library only, so
// letting the generator import them does not pull the runtime in.
func TestSharedConfigRulesStayALeaf(t *testing.T) {
	for _, leaf := range []string{sharedConfigRules, sharedConfigSchema} {
		checkStandardLibraryOnly(t, strings.TrimPrefix(leaf, modulePath+"/"))
	}
}

func checkStandardLibraryOnly(t *testing.T, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".go" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			t.Fatal(err)
		}
		for _, spec := range file.Imports {
			name, _ := strconv.Unquote(spec.Path.Value)
			if first, _, _ := strings.Cut(name, "/"); strings.Contains(first, ".") {
				t.Errorf("%s imports %s; %s must stay standard-library only (the codegen layer imports it)", path, name, dir)
			}
		}
	}
}

func forbiddenCoreImport(name string) bool {
	const owner = "github.com/tjbdwanghaibo/"
	if strings.HasPrefix(name, owner+"cube-") {
		return true
	}
	for _, module := range []string{"roost-kit", "roost-skill", "roost-service", "roost-codegen"} {
		if name == owner+module || strings.HasPrefix(name, owner+module+"/") {
			return true
		}
	}
	return false
}

func TestLayerViolation(t *testing.T) {
	const core = modulePath
	refused := []struct{ layer, imported string }{
		// 反向依赖：这四条是这条测试存在的全部理由。
		{"core", core + "/kit/service/mail"},
		{"core", core + "/codegen/internal/roost"},
		{"core", core + "/demo"},
		{"kit", core + "/codegen/internal/roost"},
		// 生成器不依赖它生成的那个运行时。
		{"codegen", core + "/entity"},
		{"codegen", core + "/kit/mods"},
		{"codegen", core + "/configdata"}, // only configdata/rules is shared
	}
	for _, item := range refused {
		if layerViolation(item.layer, item.imported) == "" {
			t.Errorf("%s layer was allowed to import %s", item.layer, item.imported)
		}
	}
	allowed := []struct{ layer, imported string }{
		{"core", core + "/entity"},
		{"core", "context"},
		{"core", "go.mongodb.org/mongo-driver/v2/mongo"},
		{"kit", core + "/entity"},
		{"kit", core + "/dataengine/engine"},
		{"kit", core + "/kit/service/mail"},
		{"codegen", core + "/demo"},
		{"codegen", core + "/codegen/internal/roost"},
		{"codegen", core + "/configdata/rules"},
		{"codegen", "gopkg.in/yaml.v3"},
		{"demo", core + "/entity"},
	}
	for _, item := range allowed {
		if reason := layerViolation(item.layer, item.imported); reason != "" {
			t.Errorf("%s layer was refused %s: %s", item.layer, item.imported, reason)
		}
	}
}

func TestModuleLayer(t *testing.T) {
	for path, want := range map[string]string{
		"entity/entity_base.go":          "core",
		"./dataengine/engine/runtime.go": "core",
		"kit/service/mail/mail.go":       "kit",
		"kit":                            "kit",
		"codegen/cmd/roost/main.go":      "codegen",
		"demo/embed.go":                  "demo",
		"dependency_boundary_test.go":    "core",
		"kitchen/sink.go":                "core", // 前缀相同但不是那个目录
	} {
		if got := moduleLayer(path); got != want {
			t.Errorf("moduleLayer(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestForbiddenCoreImport(t *testing.T) {
	for _, name := range []string{
		// 旧模块路径仍然要被拒绝：合仓之后它是"某个文件漏改了 import"的信号。
		// 新位置 github.com/tjbdwanghaibo/roost-core/kit/... 由 layerViolation 管。
		"github.com/tjbdwanghaibo/roost-kit/mongo/mongotest",
		"github.com/tjbdwanghaibo/roost-service",
		"github.com/tjbdwanghaibo/roost-skill/skill",
		"github.com/tjbdwanghaibo/roost-codegen/cmd/roost",
		"github.com/tjbdwanghaibo/cube-core/entity",
	} {
		if !forbiddenCoreImport(name) {
			t.Errorf("accepted forbidden import %s", name)
		}
	}
	for _, name := range []string{"context", "github.com/tjbdwanghaibo/roost-core/entity", "go.mongodb.org/mongo-driver/v2/mongo"} {
		if forbiddenCoreImport(name) {
			t.Errorf("rejected allowed import %s", name)
		}
	}
}

// --- core 内三大块之间的依赖方向（F00，REFACTOR-2026-10-07-structural-guards §2）---
//
// nest 调度、dataengine、sync 是 core 的三块基础（docs/framework/impl/00-overview.md §1.1 的三个子图）。
// 块内随意；块与块之间只允许下表登记的这些边，表即 00-overview §1.1 依赖图里跨块的那几条（非测试 import）：
//
//	nest 块 → dataengine 块：nest 只认契约根包 dataengine（CommitRecord / Durability），entity 用 cache；
//	                         不碰 engine、nestwal、versionstore 这些实现。
//	dataengine 块 → nest 块：engine 与 nestwal 实现 nest 的提交钩子（import nest、entity），契约根包引用 entity 的键。
//	dataengine 块 → sync 块：只有 cache 经 sync/syncbus 广播失效。
//	sync 块 → nest 块：只有 entitysync 读 entity 的同步数据；sync 不碰调度（nest、lock、actionflow）。
//	sync 块 → dataengine 块：无。
//	nest 块 → sync 块：无。
//
// 新增一条跨块 import 时这里变红：确实需要就在 allowedCrossPillarImports 加一行并写明理由，同时改 00-overview §1；
// 表里某行不再被任何文件用到时也变红，表与现状保持一致。测试文件不受约束——测试本来就跨块装配
// （nest 的测试用 nestwal，nestwal 的测试用 dataengine/engine）。

const (
	pillarNest       = "nest 调度"
	pillarDataEngine = "dataengine"
	pillarSync       = "sync"
)

// pillarPackages 把模块内包路径归到三大块；模式写法同 go list：`x` 只指包 x，`x/...` 指 x 及其子包。
var pillarPackages = []struct{ pattern, pillar string }{
	{"nest/...", pillarNest},
	{"entity/...", pillarNest},
	{"actionflow/...", pillarNest},
	{"lock/...", pillarNest},
	{"dataengine/...", pillarDataEngine},
	{"nestwal/...", pillarDataEngine},
	{"versionstore/...", pillarDataEngine},
	{"cache/...", pillarDataEngine},
	{"sync/...", pillarSync},
	{"syncstream/...", pillarSync},
	{"gateway/...", pillarSync},
}

// allowedCrossPillarImports 是允许的跨块边（from 包 import to 包），模式写法同上。
var allowedCrossPillarImports = []struct{ from, to, why string }{
	{"nest", "dataengine", "提交点交给数据引擎契约（CommitRecord / Durability / 提交钩子接口）"},
	{"entity", "cache", "实体状态层读写缓存"},
	{"dataengine", "entity", "契约根包的记录类型引用实体键"},
	{"dataengine/engine", "entity", "引擎按实体键写回"},
	{"dataengine/engine", "nest", "引擎实现 nest 的提交钩子"},
	{"nestwal", "entity", "WAL 记录引用实体键"},
	{"nestwal", "nest", "WAL 实现 nest 的 pipelined 提交"},
	{"cache", "sync/syncbus/...", "缓存失效经 syncbus 广播"},
	{"sync/entitysync/...", "entity", "实体同步读实体的同步数据"},
}

// matchPackagePattern reports whether the module-relative package rel matches
// pattern (`x` or `x/...`).
func matchPackagePattern(pattern, rel string) bool {
	if base, ok := strings.CutSuffix(pattern, "/..."); ok {
		return rel == base || strings.HasPrefix(rel, base+"/")
	}
	return rel == pattern
}

func pillarOf(rel string) string {
	for _, entry := range pillarPackages {
		if matchPackagePattern(entry.pattern, rel) {
			return entry.pillar
		}
	}
	return ""
}

// crossPillarViolation reports why package `from` may not import package `to`
// (both module-relative), or "" if it may. allowed is the index of the
// allowlist row that permits a cross-pillar edge, or -1.
func crossPillarViolation(from, to string) (reason string, allowed int) {
	fromPillar, toPillar := pillarOf(from), pillarOf(to)
	if fromPillar == "" || toPillar == "" || fromPillar == toPillar {
		return "", -1
	}
	for i, edge := range allowedCrossPillarImports {
		if matchPackagePattern(edge.from, from) && matchPackagePattern(edge.to, to) {
			return "", i
		}
	}
	return fromPillar + " 块的 " + from + " 不得 import " + toPillar + " 块的 " + to +
		"（三大块之间只允许 allowedCrossPillarImports 登记的边，见 docs/framework/impl/00-overview.md §1）", -1
}

func TestCorePillarDependencyDirection(t *testing.T) {
	used := make([]bool, len(allowedCrossPillarImports))
	err := filepath.WalkDir(".", func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if entry.Name() == ".git" || entry.Name() == "vendor" || entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			if path != "." {
				if _, err := os.Stat(filepath.Join(path, "go.mod")); err == nil {
					return filepath.SkipDir
				}
			}
			return nil
		}
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") || moduleLayer(path) != "core" {
			return nil
		}
		from := filepath.ToSlash(filepath.Dir(path))
		if pillarOf(from) == "" {
			return nil
		}
		file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
		if err != nil {
			return err
		}
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			to, inModule := strings.CutPrefix(name, modulePath+"/")
			if !inModule {
				continue
			}
			reason, allowed := crossPillarViolation(from, to)
			if reason != "" {
				t.Errorf("%s: %s", path, reason)
			}
			if allowed >= 0 {
				used[allowed] = true
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i, edge := range allowedCrossPillarImports {
		if !used[i] {
			t.Errorf("allowedCrossPillarImports 的 %s → %s 已没有任何非测试文件用到；删掉这一行并同步 00-overview §1，表只记现状", edge.from, edge.to)
		}
	}
}

func TestCrossPillarViolation(t *testing.T) {
	refused := []struct{ from, to string }{
		{"sync/lockstep", "nest"},     // sync 不碰调度
		{"sync/entitysync", "lock"},   // entitysync 只许用 entity
		{"syncstream", "dataengine"},  // sync → dataengine 无边
		{"nest", "dataengine/engine"}, // nest 只认契约根包
		{"nest", "nestwal"},           // 同上
		{"entity", "sync/syncbus"},    // nest 块 → sync 块无边
		{"versionstore", "nest"},      // dataengine 块里只有 engine / nestwal / 契约根包接 nest 块
		{"cache", "sync/entitysync"},  // cache 只接 syncbus
		{"gateway", "entity"},         // gateway 不读实体
	}
	for _, item := range refused {
		if reason, _ := crossPillarViolation(item.from, item.to); reason == "" {
			t.Errorf("%s was allowed to import %s", item.from, item.to)
		}
	}
	allowed := []struct{ from, to string }{
		{"nest", "dataengine"},
		{"sync/entitysync/policy", "entity"},
		{"cache", "sync/syncbus/mirror"},
		{"nest", "lock"},             // 块内
		{"sync/lockstep", "metrics"}, // 不在三块里
		{"skill", "nest"},            // 建在三块之上的包不受这条约束
	}
	for _, item := range allowed {
		if reason, _ := crossPillarViolation(item.from, item.to); reason != "" {
			t.Errorf("%s was refused %s: %s", item.from, item.to, reason)
		}
	}
	for path, want := range map[string]string{
		"nest": pillarNest, "nestwal": pillarDataEngine, "sync/frame": pillarSync, "syncstream": pillarSync,
		"dataengine/engine": pillarDataEngine, "skill": "", "synctest": "",
	} {
		if got := pillarOf(path); got != want {
			t.Errorf("pillarOf(%q) = %q, want %q", path, got, want)
		}
	}
}
