package roostcore_test

import (
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

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
//	         可以读 demo 的模板
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
		if target == "demo" {
			return ""
		}
		return "生成器保持独立于它生成的运行时，不得 import " + target + " 层"
	case "demo":
		return ""
	}
	return ""
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
