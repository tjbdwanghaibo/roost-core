package kit_test

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/wiring/account"
	"github.com/tjbdwanghaibo/roost-core/wiring/configdata"
	kitconfigdata "github.com/tjbdwanghaibo/roost-core/wiring/configdata"
	"github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/etcd"
	kitetcd "github.com/tjbdwanghaibo/roost-core/wiring/etcd"
	"github.com/tjbdwanghaibo/roost-core/wiring/lock"
	kitlock "github.com/tjbdwanghaibo/roost-core/wiring/lock"
	"github.com/tjbdwanghaibo/roost-core/wiring/manager"
	kitmanager "github.com/tjbdwanghaibo/roost-core/wiring/manager"
	"github.com/tjbdwanghaibo/roost-core/wiring/mongo"
	kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"
	"github.com/tjbdwanghaibo/roost-core/wiring/nats"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	"github.com/tjbdwanghaibo/roost-core/wiring/nest"
	kitnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
	"github.com/tjbdwanghaibo/roost-core/wiring/ops"
	kitops "github.com/tjbdwanghaibo/roost-core/wiring/ops"
	"github.com/tjbdwanghaibo/roost-core/wiring/redis"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"
	"github.com/tjbdwanghaibo/roost-core/wiring/remoteentity"
	kitremoteentity "github.com/tjbdwanghaibo/roost-core/wiring/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/wiring/saga"
	kitsaga "github.com/tjbdwanghaibo/roost-core/wiring/saga"
	"github.com/tjbdwanghaibo/roost-core/wiring/statslog"
	kitstatslog "github.com/tjbdwanghaibo/roost-core/wiring/statslog"
	"github.com/tjbdwanghaibo/roost-core/wiring/syncbus"
	kitsyncbus "github.com/tjbdwanghaibo/roost-core/wiring/syncbus"
)

// P3b: kit Mods configure, publish and forward; they do not orchestrate driver
// internals. The one door into those internals is the drivers' Raw() accessor
// — a Mod that calls it is assembling by hand what core's Assemble* should own.
// Every non-test Go file in the root module is checked. Tests keep their
// freedom: Mod-level integration fixtures may need the raw handle.
func TestKitModsDoNotReachForRawDriverHandles(t *testing.T) {
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
		if filepath.Ext(path) != ".go" || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		source, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		for _, line := range rawHandleCalls(t, path, source) {
			t.Errorf("%s:%d: kit Mod calls Raw(); assembly belongs in core", path, line)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

// rawHandleCalls returns the lines on which source calls a method named Raw
// with no arguments.
func rawHandleCalls(t *testing.T, name string, source []byte) []int {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, name, source, parser.SkipObjectResolution)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	var lines []int
	ast.Inspect(file, func(node ast.Node) bool {
		call, ok := node.(*ast.CallExpr)
		if !ok || len(call.Args) != 0 {
			return true
		}
		if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Raw" {
			lines = append(lines, fset.Position(call.Pos()).Line)
		}
		return true
	})
	return lines
}

func TestRawHandleCallsDetector(t *testing.T) {
	source := []byte("package x\n\nfunc f(c interface{ Raw() int }) int {\n\treturn c.Raw()\n}\n\nfunc g() { _ = raw }\n\nvar raw = 1\n")
	if got := rawHandleCalls(t, "probe.go", source); len(got) != 1 || got[0] != 4 {
		t.Fatalf("detector lines = %v, want [4]", got)
	}
	if got := rawHandleCalls(t, "clean.go", []byte("package x\n\nfunc h(n int) int { return n }\n")); len(got) != 0 {
		t.Fatalf("detector flagged a clean file: %v", got)
	}
}

// Kit is the assembly layer: it may import core and third-party drivers, but
// never codegen (a build-time tool), another framework identity, or the two
// repositories that were folded into core / kit (their old import paths must
// not survive anywhere). Every Go file in the root module is parsed, tests and
// inactive build tags included; nested modules are separate consumers.
func TestKitDependencyBoundary(t *testing.T) {
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
		for _, spec := range file.Imports {
			name, err := strconv.Unquote(spec.Path.Value)
			if err != nil {
				return err
			}
			if forbiddenKitImport(name) {
				t.Errorf("%s: forbidden Kit dependency %s", path, name)
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func forbiddenKitImport(name string) bool {
	const owner = "github.com/tjbdwanghaibo/"
	if strings.HasPrefix(name, owner+"cube-") {
		return true
	}
	for _, module := range []string{"roost-codegen", "roost-skill", "roost-service"} {
		if name == owner+module || strings.HasPrefix(name, owner+module+"/") {
			return true
		}
	}
	return false
}

func TestForbiddenKitImport(t *testing.T) {
	for _, name := range []string{
		"github.com/tjbdwanghaibo/roost-codegen/internal/roost",
		"github.com/tjbdwanghaibo/roost-skill/skill",
		"github.com/tjbdwanghaibo/roost-service/mail",
		"github.com/tjbdwanghaibo/cube-kit/redis",
	} {
		if !forbiddenKitImport(name) {
			t.Errorf("accepted forbidden import %s", name)
		}
	}
	for _, name := range []string{"context", "github.com/tjbdwanghaibo/roost-core/infra/storage/redis", "github.com/tjbdwanghaibo/roost-core/wiring/mods", "github.com/redis/go-redis/v9"} {
		if forbiddenKitImport(name) {
			t.Errorf("rejected allowed import %s", name)
		}
	}
}

// This compile-time list is the lifecycle gate for every infrastructure Mod
// shipped by roost-kit. New built-ins must join it instead of relying on App's
// legacy unbounded Stop fallback.
func TestBuiltInModsImplementContextStop(t *testing.T) {
	implementations := []app.ModStopperWithContext{
		(*kitdataengine.Mod)(nil),
		(*kitmanager.ManagerMod)(nil),
		(*kitconfigdata.Mod)(nil),
		(*kitetcd.EtcdMod)(nil),
		(*kitlock.LockMod)(nil),
		(*kitmongo.MongoMod)(nil),
		(*kitnats.NatsMod)(nil),
		(*kitnest.Mod)(nil),
		(*kitops.OpsMod)(nil),
		(*kitredis.RedisMod)(nil),
		(*kitremoteentity.RemoteEntityMod)(nil),
		(*kitsaga.Mod)(nil),
		(*kitstatslog.StatsLogMod)(nil),
		(*kitsyncbus.SyncBusMod)(nil),
	}
	if len(implementations) != 14 {
		t.Fatalf("lifecycle gate list = %d, want 14", len(implementations))
	}
}

// app 按 Mod 名解析 DependsOn / OptionalDependsOn：依赖写成能力名（"health"、"bus"、"nats.jetstream"）会在启动时报
// unknown mod dependency。kit Mod 依赖的每个名字都必须是某个 kit Mod 的 Name()。
func TestEveryModDependencyNamesAKitMod(t *testing.T) {
	access := entity.NewManagerAccess(entity.NewEntityManager())
	all := []app.Mod{
		configdata.NewConfigDataMod(),
		dataengine.NewMod(dataengine.WithEntityAccess(access), dataengine.WithRemoteProjection(true)),
		etcd.NewEtcdMod(),
		lock.NewLockMod(),
		manager.NewManagerMod(),
		mongo.NewMongoMod(),
		nats.NewNatsMod(nil),
		nest.NewMod(access),
		ops.NewOpsMod(),
		redis.NewRedisMod(),
		remoteentity.NewRemoteEntityMod(0, remoteentity.WithMongoStorage(access)),
		syncbus.NewSyncBusMod(0),
		saga.NewMod(),
		statslog.NewStatsLogMod(),
		// 承载玩家的 game 进程的合服启动自检与它依赖的 account 客户端（DataEngine 把自检列为可选依赖）。
		account.NewClientMod(),
		account.NewServingGuardMod(),
	}
	names := map[app.ModName]bool{}
	for _, mod := range all {
		names[mod.Name()] = true
	}
	for _, mod := range all {
		if provider, ok := mod.(app.ModDependencyProvider); ok {
			for _, dep := range provider.DependsOn() {
				if !names[dep] {
					t.Errorf("%s depends on %q, which is not the name of any kit Mod", mod.Name(), dep)
				}
			}
		}
		if provider, ok := mod.(app.ModOptionalDependencyProvider); ok {
			for _, dep := range provider.OptionalDependsOn() {
				if !names[dep] {
					t.Errorf("%s optionally depends on %q, which is not the name of any kit Mod", mod.Name(), dep)
				}
			}
		}
	}
}
