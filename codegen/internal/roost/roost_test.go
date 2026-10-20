package roost

import (
	"bytes"
	"context"
	"errors"
	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
)

// networkTestsEnv opts into the tests that resolve the published framework
// from the module proxy: transactional generate runs go mod tidy, and doctor
// compiles the generated project against what it resolved (GOWORK=off). The
// default go test stays offline; the codegen-network job in
// .github/workflows/framework-compat.yml sets it (维护者决定 C9). It replaced
// the constant publishedDataEngineGeneratorDependencies, whose condition — the
// minimum published core carrying the Data Engine packages — had long held.
const networkTestsEnv = "ROOST_NETWORK_TESTS"

func networkTestsEnabled() bool { return os.Getenv(networkTestsEnv) == "1" }

func TestResolveModsAddsRequiredDependencies(t *testing.T) {
	got, err := resolveMods([]string{"remote_entity", "syncbus"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"redis", "nats", "remote_entity", "syncbus"} {
		if !contains(got, want) {
			t.Fatalf("resolved mods %v do not contain %s", got, want)
		}
	}
}

func TestResolveModsDefaultsNestAndSagaToDataEngine(t *testing.T) {
	got, err := resolveMods([]string{"nest", "saga"})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"mongo", "nats", "dataengine", "nest", "saga"} {
		if !contains(got, want) {
			t.Fatalf("resolved mods %v do not contain %s", got, want)
		}
	}
	if contains(got, "checkpoint") || contains(got, "nestwal") {
		t.Fatalf("new Nest/Saga project unexpectedly selected legacy persistence: %v", got)
	}
}

func TestResolveModsRejectsRemovedPersistenceMods(t *testing.T) {
	for _, legacy := range []string{"checkpoint", "nestwal"} {
		if _, err := resolveMods([]string{legacy}); err == nil {
			t.Fatalf("removed persistence mod %q was accepted", legacy)
		}
	}
}

func TestDefaultManifestUsesOnlyDataEnginePersistence(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", []string{"game"}, nil, nil)
	for name, service := range manifest.Services {
		if !contains(service.Mods, "dataengine") || contains(service.Mods, "checkpoint") || contains(service.Mods, "nestwal") {
			t.Fatalf("service %s mods=%v", name, service.Mods)
		}
	}
	plan, err := renderProject(manifest)
	if err != nil {
		t.Fatal(err)
	}
	manifestRaw, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	manifestFile := string(manifestRaw)
	if !strings.Contains(manifestFile, "- dataengine") {
		t.Fatalf("default roost.yaml does not enable dataengine:\n%s", manifestFile)
	}
	for path, file := range plan {
		generated := string(file.Body)
		for _, forbidden := range []string{"kitcheckpoint", "kitnestwal", "roost-kit/checkpoint", "checkpoint.NewMod", "\n          - checkpoint", "\n          - nestwal"} {
			if strings.Contains(generated, forbidden) {
				t.Fatalf("%s retained legacy persistence output %q", path, forbidden)
			}
		}
	}
}

func TestNewProjectSyncPreservesBusinessFiles(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	result, root, err := NewProject(NewOptions{
		Name:     "planet",
		Module:   "example.com/planet",
		Out:      target,
		Services: []string{"game", "gate"},
		Mods:     []string{"configdata", "syncbus", "remote_entity"},
		Features: []string{"config", "nest"},
	})
	if err != nil {
		t.Fatalf("new project: %v", err)
	}
	if len(result.Created) == 0 || root != target {
		t.Fatalf("unexpected result: %#v root=%s", result, root)
	}
	for _, rel := range []string{
		"roost.yaml",
		"Makefile",
		"docs/QUICKSTART.zh-CN.md",
		"docs/ROOST_YAML.zh-CN.md",
		"docs/ENTITY_COMPONENT.zh-CN.md",
		"internal/bootstrap/generated.go",
		"internal/service/game/service.go",
		"configs/generated/gen_table_config.go",
		"internal/registry/nest_gen.go",
		"deploy/dev/run.sh",
	} {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("%s missing: %v", rel, err)
		}
	}

	servicePath := filepath.Join(root, "internal", "service", "game", "service.go")
	raw, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	raw = append(raw, []byte("\n// business-owned\n")...)
	if err := os.WriteFile(servicePath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncProject(root); err != nil {
		t.Fatalf("sync project: %v", err)
	}
	after, _ := os.ReadFile(servicePath)
	if !bytes.Contains(after, []byte("business-owned")) {
		t.Fatal("sync overwrote the business service file")
	}
}

func TestAddProtocolAllocatesIDAndCheckDetectsDuplicate(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"protocol"}})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := Add(root, AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "game"})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 {
		t.Fatalf("paths = %v", paths)
	}
	raw, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(paths[0])))
	if !strings.Contains(string(raw), "id=10000") {
		t.Fatalf("protocol did not receive first ID:\n%s", raw)
	}
	m, _ := LoadManifest(root)
	if err := CheckIDs(root, m); err != nil {
		t.Fatalf("id check: %v", err)
	}
	duplicate := filepath.Join(root, "protocol", "def", "duplicate.go")
	if err := os.WriteFile(duplicate, []byte("package protocoldef\n//roost:msg id=10000\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckIDs(root, m); err == nil {
		t.Fatal("expected duplicate ID failure")
	}
}

func TestLoadManifestRejectsUnknownFields(t *testing.T) {
	root := t.TempDir()
	raw := []byte("schema: 1\nproject:\n  name: planet\n  module: example.com/planet\nversions: {}\nservices:\n  game: {}\nunknown: true\n")
	if err := os.WriteFile(filepath.Join(root, ManifestName), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(root); err == nil {
		t.Fatal("expected strict YAML decode error")
	}
}

func TestUpgradeCanReadVersionsBelowCurrentFloor(t *testing.T) {
	root := copyOfNewProject(t, "bare")
	manifestPath := filepath.Join(root, ManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	for _, module := range []string{"core", "kit"} {
		raw = bytes.Replace(raw, []byte("  "+module+": latest"), []byte("  "+module+": v1.0.0"), 1)
	}
	if err := os.WriteFile(manifestPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadManifest(root); err == nil {
		t.Fatal("normal manifest load accepted unsupported legacy versions")
	}
	makefilePath := filepath.Join(root, "Makefile")
	legacyMakefile := []byte("# Code generated by roost-codegen. DO NOT EDIT.\n\n.PHONY: sync\nsync:\n\t$(ROOST) project sync\n")
	if err := os.WriteFile(makefilePath, legacyMakefile, 0o644); err != nil {
		t.Fatal(err)
	}
	var preview bytes.Buffer
	if err := Run([]string{"project", "upgrade", "--root", root, "--dry-run", "-core", "latest"}, &preview, io.Discard); err != nil {
		t.Fatalf("preview legacy upgrade: %v", err)
	}
	if !strings.Contains(preview.String(), "roost.yaml") || !strings.Contains(preview.String(), "Makefile") {
		t.Fatalf("upgrade preview did not report manifest and Makefile:\n%s", preview.String())
	}
	afterPreview, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(afterPreview, legacyMakefile) {
		t.Fatal("upgrade preview modified Makefile")
	}
	manifestAfterPreview, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(manifestAfterPreview, []byte("core: v1.0.0")) {
		t.Fatal("upgrade preview modified roost.yaml")
	}
	manifest, err := loadManifestForUpgrade(root)
	if err != nil {
		t.Fatalf("decode legacy manifest for upgrade: %v", err)
	}
	if manifest.CICD.Provider != "github" || !contains(manifest.CICD.Deploy, "shell") || !contains(manifest.CICD.Deploy, "docker") || !contains(manifest.CICD.Deploy, "k8s") {
		t.Fatalf("legacy manifest did not receive production CI/CD defaults: %+v", manifest.CICD)
	}
	mergeVersions(&manifest.Versions, VersionSpec{Core: "latest"})
	if err := saveManifest(root, manifest); err != nil {
		t.Fatalf("save upgraded manifest: %v", err)
	}
	if _, err := LoadManifest(root); err != nil {
		t.Fatalf("load upgraded manifest: %v", err)
	}
}

func TestProductionConfigRejectsDevelopmentValues(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte("sid: 1\nredis:\n  addr: 127.0.0.1:6379\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := CheckConfig(path, true); err == nil {
		t.Fatal("expected production config rejection")
	}
}

func TestConfigCheckAllValidatesEveryDeclaredService(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Services: []string{"game", "gate"}, Mods: []string{"configdata"}})
	if err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	// 夹具没有解析依赖、编译不过：键都通过，但跨 Mod 规则没查，退出码 5（检查没有跑全），不是 0。
	if err := Run([]string{"config", "check", "--root", root, "--all"}, &output, io.Discard); configCheckExitCode(err) != configExitNotFullyChecked {
		t.Fatalf("uncompilable project: config check --all = %v; want exit %d", err, configExitNotFullyChecked)
	}
	for _, service := range []string{"game", "gate"} {
		if !strings.Contains(output.String(), "config."+service+".yaml") {
			t.Errorf("all-service config output missing %s:\n%s", service, output.String())
		}
	}
	// 夹具没有解析依赖、编译不过：跨 Mod 规则没查时要明说，不能报 "config ok"。
	if !strings.Contains(output.String(), "cross-mod rules NOT checked") || strings.Contains(output.String(), "config ok:") {
		t.Errorf("uncompilable project: output must say the cross-mod rules were not checked:\n%s", output.String())
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "service", "config.gate.yaml"), []byte("invalid: ["), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"config", "check", "--root", root, "--all"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "service gate") || configCheckExitCode(err) != configExitKeyErrors {
		t.Fatalf("invalid secondary service config was not reported as a key error (exit %d): %v", configExitKeyErrors, err)
	}
}

// configCheckExitCode 是 roost config check 返回的退出码（nil 为 0；没有退出码的错误是 1，与 main 相同）。
func configCheckExitCode(err error) int {
	if err == nil {
		return 0
	}
	var coded *ExitError
	if errors.As(err, &coded) {
		return coded.Code
	}
	return 1
}

// config check 与 doctor 用同一份框架声明查配置键：框架段里没人声明的键（多半拼错）让检查失败，
// --service、--file（按文件名认出服务）与 --all 三种入口一致；修前 config check 只看 sid 与生产禁用值，
// 拼错的 mongo.urii 报 "config is valid"，进程启动时静默退回默认地址。
func TestConfigCheckRejectsMisspelledFrameworkKeys(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata", "nest"}})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "configs", "service", "config.game.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"config", "check", "--root", root, "--service", "game", "--keys-only"}, io.Discard, io.Discard); err != nil {
		t.Fatalf("generated config rejected: %v", err)
	}
	misspelled := strings.Replace(string(raw), "\nmongo:\n", "\nmongo:\n  urii: mongodb://127.0.0.1:27017\n", 1)
	if misspelled == string(raw) {
		t.Fatal("generated config has no mongo section")
	}
	if err := os.WriteFile(path, []byte(misspelled), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "check", "--root", root, "--service", "game"},
		{"config", "check", "--root", root, "--file", "configs/service/config.game.yaml"},
		{"config", "check", "--root", root, "--all"},
		{"config", "check", "--root", root, "--all", "--keys-only"},
	} {
		err := Run(args, io.Discard, io.Discard)
		if err == nil || !strings.Contains(err.Error(), "mongo.urii") || configCheckExitCode(err) != configExitKeyErrors {
			t.Errorf("%v: err = %v, want the misspelled mongo.urii reported", args[len(args)-2:], err)
		}
	}
}

// roost.yaml 读不出来时 config check 不能把文件报成通过：声明检查需要它，静默跳过就是放过拼错的键。
// 修前 --service / --file 打印一行 "keys not checked" 后报 "config is valid"，还提示“到有 roost.yaml 的工程里跑”。
func TestConfigCheckFailsWhenManifestDoesNotLoad(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata", "nest"}})
	if err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, ManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(manifestPath, append(raw, []byte("\nno_such_manifest_field: true\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][]string{
		{"config", "check", "--root", root, "--service", "game"},
		{"config", "check", "--root", root, "--file", "configs/service/config.game.yaml"},
	} {
		var stdout bytes.Buffer
		err := Run(args, &stdout, io.Discard)
		if err == nil || !strings.Contains(err.Error(), ManifestName) {
			t.Errorf("%v: err = %v (stdout %q), want the roost.yaml load error", args[len(args)-2:], err, stdout.String())
		}
	}
}

// 框架段里由业务服务声明的键（例如认证器读的 player_access.tcp.dev_auth）不算拼错：静态检查发现可疑键后
// 按进程的业务声明复核。
func TestConfigFileDeclarationsAcceptBusinessKeysInFrameworkSections(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata", "nest"}})
	if err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "configs", "service", "config.game.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	business := strings.Replace(string(raw), "\nmongo:\n", "\nmongo:\n  shop_region: cn\n", 1)
	if business == string(raw) {
		t.Fatal("generated config has no mongo section")
	}
	if err := os.WriteFile(path, []byte(business), 0o644); err != nil {
		t.Fatal(err)
	}
	asked := 0
	process := func(service string) (configschema.Schema, error) {
		asked++
		return configschema.Schema{Keys: []configschema.Key{{Name: "mongo.shop_region", Kind: configschema.KindString}}}, nil
	}
	problems, err := checkConfigFileDeclarations(path, false, manifest, "game", process)
	if err != nil || len(problems) != 0 || asked != 1 {
		t.Fatalf("business key: problems=%v err=%v process asked %d times, want none / nil / 1", problems, err, asked)
	}
}

func TestAddCamelCaseProtocolUsesSnakeFileAndPascalTypes(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"protocol"}})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := Add(root, AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "game"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths[0], "protocol/def/player_login.go"; got != want {
		t.Fatalf("path = %s, want %s", got, want)
	}
	raw, _ := os.ReadFile(filepath.Join(root, filepath.FromSlash(paths[0])))
	if !strings.Contains(string(raw), "type PlayerLoginRequest struct") {
		t.Fatalf("unexpected protocol:\n%s", raw)
	}
}

func TestBeginnerEntityComponentAndDAOFlowWiresAggregate(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"}, Features: []string{"entity", "dao", "nest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "entity", Name: "Player"}); err != nil {
		t.Fatal(err)
	}
	componentPaths, err := Add(root, AddOptions{Kind: "component", Name: "Inventory", Entity: "Player"})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := strings.Join(componentPaths, ","), "game/entities/player/inventory_component.go,game/entities/player/entity.go"; got != want {
		t.Fatalf("component paths = %s, want %s", got, want)
	}
	if _, err := Add(root, AddOptions{Kind: "dao", Name: "Player", Entity: "Player"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "handler", Name: "RenamePlayer", Entity: "Player", Component: "Inventory"}); err == nil || !strings.Contains(err.Error(), "roost add mod nest -service game") {
		t.Fatalf("handler without the nest runtime mod error = %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "nest", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	entitySource, err := os.ReadFile(filepath.Join(root, "game", "entities", "player", "entity.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"type IPlayerEntity interface", "entity.IThreadSafeEntity", "entity.ComponentManager", "entity.DaoManager",
		"inventory *InventoryComponent `comp:\"CompTypeInventory\"`",
		"*db.PlayerDao", "`dao:\"player\"`", `db "example.com/planet/db"`,
	} {
		if !strings.Contains(string(entitySource), want) {
			t.Errorf("Entity scaffold missing %q:\n%s", want, entitySource)
		}
	}
	componentSource, err := os.ReadFile(filepath.Join(root, "game", "entities", "player", "inventory_component.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"entity.RegisterComponentFactory", "owner.(*Player)", "func (component *InventoryComponent) Owner() *Player", "type IInventoryEntity interface", "InventoryComp() *InventoryComponent",
		"Do not add mutexes here", "CompTypeInventory entity.ComponentType = 2000",
	} {
		if !strings.Contains(string(componentSource), want) {
			t.Errorf("Component scaffold missing %q:\n%s", want, componentSource)
		}
	}
	if _, err := Add(root, AddOptions{Kind: "handler", Name: "RenamePlayer", Entity: "Player", Component: "Inventory"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("generate beginner aggregate: %v", err)
	}
	wire, err := os.ReadFile(filepath.Join(root, "game", "entities", "player", "player_gen_wire.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"CreateComponent(CompTypeInventory", "func (e *Player) InventoryComp() *InventoryComponent", "func (e *Player) Dao() *db.PlayerDao"} {
		if !strings.Contains(string(wire), want) {
			t.Errorf("generated Entity wire missing %q:\n%s", want, wire)
		}
	}
	nestFiles, err := filepath.Glob(filepath.Join(root, "game", "handler", "*_nest_gen.go"))
	if err != nil || len(nestFiles) == 0 {
		t.Fatalf("Nest output missing: files=%v err=%v", nestFiles, err)
	}
	nestWire, err := os.ReadFile(nestFiles[0])
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"handlerRenamePlayer", "player.IInventoryEntity", "nest.DurabilityStrict"} {
		if !strings.Contains(string(nestWire), want) {
			t.Errorf("generated Nest wire missing %q:\n%s", want, nestWire)
		}
	}
}

func TestAddComponentInfersOnlyEntityAndRejectsAmbiguity(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"entity"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "entity", Name: "Player"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "component", Name: "Profile"}); err != nil {
		t.Fatalf("infer the only Entity: %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "entity", Name: "Guild"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "component", Name: "Inventory"}); err == nil || !strings.Contains(err.Error(), "--entity") {
		t.Fatalf("ambiguous component target error = %v", err)
	}
}

func TestExplicitFirstBusinessWorkflowGeneratesAccessLifecycleAndEndpoint(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"},
	})
	if err != nil {
		t.Fatal(err)
	}
	steps := []AddOptions{
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "entity", Name: "Player"},
		{Kind: "component", Name: "Profile", Entity: "Player"},
		{Kind: "dao", Name: "Player", Entity: "Player"},
		{Kind: "handler", Name: "RenamePlayer", Entity: "Player", Component: "Profile"},
		{Kind: "protocol", Name: "RenamePlayer", Group: "game", Handler: "player"},
		{Kind: "lifecycle", Name: "Player", Entity: "Player", Service: "game"},
		{Kind: "endpoint", Name: "RenamePlayer", Handler: "player", Protocol: "RenamePlayer", NestHandler: "RenamePlayer"},
	}
	for _, step := range steps {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
		switch step.Kind {
		case "component":
			path := filepath.Join(root, "game", "entities", "player", "profile_component.go")
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			raw = append(raw, []byte("\nfunc (component *ProfileComponent) Rename(name string) error { return nil }\n")...)
			if writeErr := os.WriteFile(path, raw, 0o644); writeErr != nil {
				t.Fatal(writeErr)
			}
		case "dao":
			path := filepath.Join(root, "db", "def", "player.go")
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			raw = bytes.Replace(raw, []byte("\t// Add domain fields here. Example:\n"), []byte("\tName string `bson:\"name\" dao:\"persist,sync\"`\n\t// Add domain fields here. Example:\n"), 1)
			if writeErr := os.WriteFile(path, raw, 0o644); writeErr != nil {
				t.Fatal(writeErr)
			}
		case "handler":
			path := filepath.Join(root, "game", "handler", "rename_player.go")
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			raw = bytes.Replace(raw,
				[]byte("func handlerRenamePlayer(target player.IProfileEntity) error {\n\t// TODO: validate input and call target.ProfileComp().<BusinessMethod>().\n\treturn nil\n}"),
				[]byte("func handlerRenamePlayer(target player.IProfileEntity, name string) error {\n\treturn target.ProfileComp().Rename(name)\n}"), 1)
			if writeErr := os.WriteFile(path, raw, 0o644); writeErr != nil {
				t.Fatal(writeErr)
			}
		case "protocol":
			path := filepath.Join(root, "protocol", "def", "rename_player.go")
			raw, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			raw = bytes.Replace(raw, []byte("type RenamePlayerRequest struct{}"), []byte("type RenamePlayerRequest struct { Name string `pb:\"1\"` }"), 1)
			if writeErr := os.WriteFile(path, raw, 0o644); writeErr != nil {
				t.Fatal(writeErr)
			}
		}
	}
	// 首次真实请求前玩家必须先有 Entity：没有“登录时 lifecycle.GetOrCreate”这一步，Nest 冷加载会回
	// entity_not_found。first-business 必须把这一步列为未完成，并给出可执行的命令（DESIGN-TODO D7）。
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	loginItem := func() (CheckItem, bool) {
		t.Helper()
		items, err := checkFirstBusinessWorkflow(root, manifest)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.Name == "workflow:login" {
				return item, true
			}
		}
		return CheckItem{}, false
	}
	if item, found := loginItem(); !found || item.Status != StatusFail || !strings.Contains(item.Detail, "roost add endpoint EnterGame --handler player --lifecycle Player") {
		t.Fatalf("first-business without a login endpoint: workflow:login = %+v (found=%v), want FAIL with the add command", item, found)
	}
	for _, step := range []AddOptions{
		{Kind: "protocol", Name: "EnterGame", Group: "game", Handler: "player"},
		{Kind: "endpoint", Name: "EnterGame", Handler: "player", Lifecycle: "Player"},
	} {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
	}
	if item, found := loginItem(); !found || item.Status != StatusOK {
		t.Fatalf("first-business with the login endpoint: workflow:login = %+v (found=%v), want OK", item, found)
	}
	if networkTestsEnabled() {
		if err := GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard}, io.Discard); err != nil {
			t.Fatalf("generate first business workflow: %v", err)
		}
		var doctorOutput bytes.Buffer
		if err := DoctorWithOptions(context.Background(), root, DoctorOptions{Strict: true, Workflow: "first-business"}, &doctorOutput); err != nil {
			t.Fatalf("doctor first business workflow: %v\n%s", err, doctorOutput.String())
		}
	} else if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatalf("generate source-head first business workflow: %v", err)
	}
	checks := map[string][]string{
		"roost.yaml":                               {"access:", "player:", "service: game", "- nest"},
		"game/playeraccess/runtime_gen.go":         {"type ProtocolRegistry struct", "func (registry *ProtocolRegistry) Dispatch", "gateway.ErrUnauthenticated"},
		"internal/access/player/mod_gen.go":        {"protocolbootstrap.RegisterPlayerProtocols", "registry.Register(Name, mod.runtime)"},
		"game/protocol_bootstrap/protocol_gen.go":  {"RegisterPlayerPlayerProtocols"},
		"game/controllers/player/controller.go":    {"app.Lookup[corenest.Client]", "NestClient() corenest.Client", "Registry() *app.Registry"},
		"game/controllers/player/enter_game.go":    {"lifecycle.PlayerFromRegistry(controller.Registry())", ".GetOrCreate(context.Context(), context.PlayerID)", "errcode.ClientError(err)", "&pb.EnterGameResponse{Code: code, Reason: reason}, nil"},
		"game/controllers/player/rename_player.go": {"Sync_RenamePlayer", "NewRenamePlayerSender", "entity.BuildEntityID(context.PlayerID, player.EntityKindPlayer)", "Sync_RenamePlayer(context.Context(), entityID, request.Name)"},
		"game/lifecycle/player.go": {"GetOrCreate", "FromRegistry", "mods.ModEntityRuntime", "engine.ErrEntityAggregateNotFound", "entity.BuildEntityID", "EntityKindPlayer", "errors.Is(err, entity.ErrEntityRemoved) && attempt < 3",
			// D10 方案 B（只守 GetOrCreate）：删过档的 ID 不再创建，Get 给稳定码 entity_deleted。
			"func deletedPlayerError(err error) error", "errcode.Wrap(errcode.ErrEntityDeleted, err)", "return nil, deletedPlayerError(err)"},
		"protocol/player_bind/bind_gen.go": {"RegisterRenamePlayer"},
	}
	for path, fragments := range checks {
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if err != nil {
			t.Errorf("read %s: %v", path, err)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(raw), fragment) {
				t.Errorf("%s missing %q:\n%s", path, fragment, raw)
			}
		}
	}
	// A duplicate endpoint must be rejected before any companion controller is
	// created. This keeps a failed add operation free of partial artifacts.
	controllerPath := filepath.Join(root, "game", "controllers", "player", "controller.go")
	if err := os.Remove(controllerPath); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "endpoint", Name: "RenamePlayer", Handler: "player"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate endpoint error = %v", err)
	}
	if _, err := os.Stat(controllerPath); !os.IsNotExist(err) {
		t.Fatalf("duplicate endpoint recreated controller: %v", err)
	}
}

// add protocol 之后跑过 roost generate，协议生成器为 handler 写了“未实现”占位（HandleXxx 返回 not implemented）。
// add endpoint 接管仍未改动的占位（新用户第七轮：修前报 `game/controllers/player/rename_player.go already exists`，
// 不说怎么办）；占位被业务改过就不覆盖，报错写明是协议占位、怎么处置，文件不变。
func TestAddEndpointTakesOverTheUntouchedProtocolPlaceholder(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range []AddOptions{
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "entity", Name: "Player"},
		{Kind: "component", Name: "Profile", Entity: "Player"},
		{Kind: "handler", Name: "RenamePlayer", Entity: "Player", Component: "Profile"},
		{Kind: "protocol", Name: "RenamePlayer", Group: "game", Handler: "player"},
		{Kind: "protocol", Name: "Ping", Group: "game", Handler: "player"},
	} {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
	}
	if err := Generate(root, GenerateOptions{}); err != nil {
		t.Fatalf("generate: %v", err)
	}
	placeholder := filepath.Join(root, "game", "controllers", "player", "rename_player.go")
	raw, err := os.ReadFile(placeholder)
	if err != nil || !strings.Contains(string(raw), "not implemented") {
		t.Fatalf("generate did not leave the protocol placeholder (err %v):\n%s", err, raw)
	}
	if _, err := Add(root, AddOptions{Kind: "endpoint", Name: "RenamePlayer", Handler: "player"}); err != nil {
		t.Fatalf("add endpoint over the untouched protocol placeholder: %v", err)
	}
	raw, err = os.ReadFile(placeholder)
	if err != nil || !strings.Contains(string(raw), "Sync_RenamePlayer") || strings.Contains(string(raw), "not implemented") {
		t.Fatalf("add endpoint did not replace the placeholder (err %v):\n%s", err, raw)
	}

	// 业务改过的占位：拒绝、给处置、不改文件。
	ping := filepath.Join(root, "game", "controllers", "player", "ping.go")
	pingRaw, err := os.ReadFile(ping)
	if err != nil {
		t.Fatal(err)
	}
	edited := bytes.Replace(pingRaw, []byte("return nil, fmt.Errorf("), []byte("_ = req\n\treturn nil, fmt.Errorf("), 1)
	if bytes.Equal(edited, pingRaw) {
		t.Fatalf("ping placeholder changed shape:\n%s", pingRaw)
	}
	if err := os.WriteFile(ping, edited, 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = Add(root, AddOptions{Kind: "endpoint", Name: "Ping", Handler: "player", NestHandler: "RenamePlayer"})
	if err == nil || !strings.Contains(err.Error(), "game/controllers/player/ping.go already exists") || !strings.Contains(err.Error(), "delete") {
		t.Fatalf("add endpoint over an edited file = %v; want already exists with what to do", err)
	}
	if after, _ := os.ReadFile(ping); !bytes.Equal(after, edited) {
		t.Error("a refused add endpoint changed the edited file")
	}
}

// `roost add endpoint` 写的是协议边界：handler 返回值时 Sync_ 返回 (result, err)，骨架要接住全部返回值才能编译；
// 接入层把 Dispatch 返回的 error 当坏帧断开连接（infra/network/gateway/tcp.go dispatchRequest），所以业务失败必须
// 经 errcode.ClientError 变成 (Code, Reason) 响应、只给 CodeInternal 记日志，不能 return nil, err。
// 响应没有 Code / Reason 字段时边界无处表达失败，生成前就拒绝。
func TestEndpointScaffoldTakesTheHandlerResultAndAnswersBusinessErrors(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}})
	if err != nil {
		t.Fatal(err)
	}
	edit := func(rel, old, replacement string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte(old)) {
			t.Fatalf("fixture drift: %s no longer contains %q", rel, old)
		}
		if err := os.WriteFile(path, bytes.Replace(raw, []byte(old), []byte(replacement), 1), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []AddOptions{
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "entity", Name: "Player"},
		{Kind: "component", Name: "Profile", Entity: "Player"},
		{Kind: "handler", Name: "RenamePlayer", Entity: "Player", Component: "Profile"},
		{Kind: "protocol", Name: "RenamePlayer", Group: "game", Handler: "player"},
		{Kind: "protocol", Name: "Ping", Group: "game", Handler: "player"},
	} {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
	}
	edit("game/handler/rename_player.go",
		"func handlerRenamePlayer(target player.IProfileEntity) error {\n\t// TODO: validate input and call target.ProfileComp().<BusinessMethod>().\n\treturn nil\n}",
		"func handlerRenamePlayer(target player.IProfileEntity, name string) (string, error) {\n\treturn name, nil\n}")
	edit("protocol/def/rename_player.go", "type RenamePlayerRequest struct{}", "type RenamePlayerRequest struct { Name string `pb:\"1\"` }")

	if _, err := Add(root, AddOptions{Kind: "endpoint", Name: "RenamePlayer", Handler: "player"}); err != nil {
		t.Fatalf("add endpoint: %v", err)
	}
	path := filepath.Join(root, "game", "controllers", "player", "rename_player.go")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	file, err := parser.ParseFile(token.NewFileSet(), path, raw, 0)
	if err != nil {
		t.Fatal(err)
	}
	received := -1
	ast.Inspect(file, func(node ast.Node) bool {
		assign, ok := node.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		if call, ok := assign.Rhs[0].(*ast.CallExpr); ok {
			if selector, ok := call.Fun.(*ast.SelectorExpr); ok && selector.Sel.Name == "Sync_RenamePlayer" {
				received = len(assign.Lhs)
			}
		}
		return true
	})
	if received != 2 {
		t.Errorf("endpoint receives %d values from Sync_RenamePlayer, which returns (string, error):\n%s", received, raw)
	}
	for _, want := range []string{"errcode.ClientError(err)", "code == errcode.CodeInternal", "&pb.RenamePlayerResponse{Code: code, Reason: reason}, nil"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("endpoint does not answer a business error with a coded response (missing %q):\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), "return nil, err\n") {
		t.Errorf("endpoint returns the handler error to the access layer, which closes the connection:\n%s", raw)
	}

	edit("protocol/def/ping.go", "\tReason string `pb:\"2\"`\n", "")
	if _, err := Add(root, AddOptions{Kind: "endpoint", Name: "Ping", Handler: "player", NestHandler: "RenamePlayer"}); err == nil || !strings.Contains(err.Error(), "PingResponse") || !strings.Contains(err.Error(), "Code") {
		t.Fatalf("endpoint for a response without Code / Reason: %v", err)
	}
}

// 业务时间是服务器决定的值，不能来自请求（客户端可以借此延长限时效果、提前领奖励）。handler 参数按约定名
// nowUnix / nowUnixMilli（int64）声明业务时间时，endpoint 骨架从 app 的业务钟（app.BusinessClock，含
// time.logic_offset）取值传给 Sender，不要求、也不读请求里的同名字段；其余请求里没有的参数照旧拒绝，报错不再只说
// “请求缺字段”，而是写明服务器决定的值不放进请求（新用户试用第四轮登记项 2）。
func TestEndpointFillsBusinessTimeFromTheServer(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Mods: []string{"configdata"}})
	if err != nil {
		t.Fatal(err)
	}
	edit := func(rel, old, replacement string) {
		t.Helper()
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(raw, []byte(old)) {
			t.Fatalf("fixture drift: %s no longer contains %q", rel, old)
		}
		if err := os.WriteFile(path, bytes.Replace(raw, []byte(old), []byte(replacement), 1), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	for _, step := range []AddOptions{
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "entity", Name: "Player"},
		{Kind: "component", Name: "Profile", Entity: "Player"},
		{Kind: "handler", Name: "UseDoubleCard", Entity: "Player", Component: "Profile"},
		{Kind: "protocol", Name: "UseDoubleCard", Group: "game", Handler: "player"},
		{Kind: "handler", Name: "Claim", Entity: "Player", Component: "Profile"},
		{Kind: "protocol", Name: "Claim", Group: "game", Handler: "player"},
	} {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
	}
	edit("game/handler/use_double_card.go",
		"func handlerUseDoubleCard(target player.IProfileEntity) error {\n\t// TODO: validate input and call target.ProfileComp().<BusinessMethod>().\n\treturn nil\n}",
		"func handlerUseDoubleCard(target player.IProfileEntity, cardID int64, nowUnixMilli int64) error {\n\t_, _ = cardID, nowUnixMilli\n\treturn nil\n}")
	edit("protocol/def/use_double_card.go", "type UseDoubleCardRequest struct{}", "type UseDoubleCardRequest struct { CardID int64 `pb:\"1\"` }")
	if _, err := Add(root, AddOptions{Kind: "endpoint", Name: "UseDoubleCard", Handler: "player"}); err != nil {
		t.Fatalf("add endpoint with a business-time parameter: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "game", "controllers", "player", "use_double_card.go"))
	if err != nil {
		t.Fatal(err)
	}
	body := string(raw)
	for _, want := range []string{"app.BusinessClock(controller.Registry()).Now()", "request.CardID, businessNow.UnixMilli())"} {
		if !strings.Contains(body, want) {
			t.Errorf("endpoint does not fill the business time from the server clock (missing %q):\n%s", want, body)
		}
	}
	if strings.Contains(body, "request.NowUnixMilli") {
		t.Errorf("endpoint reads the business time from the request:\n%s", body)
	}

	// 请求里没有、也不是业务时间的参数：照旧拒绝，但报错写明服务器决定的值不放进请求、业务时间用约定名。
	edit("game/handler/claim.go",
		"func handlerClaim(target player.IProfileEntity) error {\n\t// TODO: validate input and call target.ProfileComp().<BusinessMethod>().\n\treturn nil\n}",
		"func handlerClaim(target player.IProfileEntity, claimedAt int64) error {\n\t_ = claimedAt\n\treturn nil\n}")
	_, err = Add(root, AddOptions{Kind: "endpoint", Name: "Claim", Handler: "player"})
	if err == nil {
		t.Fatal("endpoint accepted a parameter that is neither a request field nor server-supplied")
	}
	for _, want := range []string{`"claimedAt"`, "nowUnix", "nowUnixMilli", "business clock", "not from the request"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not steer away from client-supplied time (missing %q): %v", want, err)
		}
	}
}

func TestDoctorFirstBusinessReportsActionableMissingSteps(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	var output bytes.Buffer
	err := DoctorWithOptions(context.Background(), root, DoctorOptions{Strict: false, Workflow: "first-business"}, &output)
	if err == nil {
		t.Fatal("incomplete first business workflow unexpectedly passed")
	}
	for _, want := range []string{"workflow:player-access", "roost add access player", "workflow:endpoint", "roost add endpoint"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, output.String())
		}
	}
}

// first-business 的修复命令按工程现状填名字：已有 Nest handler 而缺协议时用那个 handler，已有 controller 域时沿用它；
// 什么都没有时才给示例名并标明是示例。修前固定提示 RenamePlayer 与 --handler player，照抄会生成与业务无关的协议、
// 多出一个 player 域（新用户试用第三轮）。
func TestFirstBusinessFixesNameWhatTheProjectAlreadyHas(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	fixOf := func(name string) string {
		t.Helper()
		manifest, err := LoadManifest(root)
		if err != nil {
			t.Fatal(err)
		}
		items, err := checkFirstBusinessWorkflow(root, manifest)
		if err != nil {
			t.Fatal(err)
		}
		for _, item := range items {
			if item.Name == "workflow:"+name {
				_, fix := splitWorkflowFix(item.Detail)
				return fix
			}
		}
		t.Fatalf("no workflow:%s item", name)
		return ""
	}
	for _, step := range []AddOptions{
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "entity", Name: "Player"},
		{Kind: "component", Name: "Shop", Entity: "Player"},
	} {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
	}
	if fix := fixOf("nest-handler"); fix != "roost add handler RenamePlayer --entity Player --component Shop"+exampleNameNote {
		t.Errorf("nest-handler fix = %q, want the existing component and the handler name marked as an example", fix)
	}
	for _, step := range []AddOptions{
		{Kind: "handler", Name: "BuyItem", Entity: "Player", Component: "Shop"},
		{Kind: "handler", Name: "GrantGold", Entity: "Player", Component: "Shop"},
	} {
		if _, err := Add(root, step); err != nil {
			t.Fatalf("add %s %s: %v", step.Kind, step.Name, err)
		}
	}
	if fix := fixOf("protocol"); fix != "roost add protocol BuyItem --group game --handler player" {
		t.Errorf("protocol fix = %q, want the first handler that has no protocol", fix)
	}
	if _, err := Add(root, AddOptions{Kind: "protocol", Name: "BuyItem", Group: "game", Handler: "shop"}); err != nil {
		t.Fatal(err)
	}
	if fix := fixOf("endpoint"); fix != "roost add endpoint BuyItem --handler shop" {
		t.Errorf("endpoint fix = %q, want the protocol without an endpoint, in its own controller domain", fix)
	}
	if fix := fixOf("login"); !strings.Contains(fix, "roost add protocol EnterGame --group game --handler shop") || !strings.Contains(fix, "roost add endpoint EnterGame --handler shop --lifecycle Player") {
		t.Errorf("login fix = %q, want the existing controller domain shop", fix)
	}
}

func TestDoctorFailsWhenGeneratedProjectDoesNotCompile(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	if err := os.WriteFile(filepath.Join(root, "broken.go"), []byte("package planet\nfunc broken("), 0o644); err != nil {
		t.Fatal(err)
	}

	var output bytes.Buffer
	err := DoctorWithOptions(context.Background(), root, DoctorOptions{Strict: false}, &output)
	if err == nil {
		t.Fatalf("non-compiling project unexpectedly passed doctor:\n%s", output.String())
	}
	if !strings.Contains(output.String(), "compile:go-list") || !strings.Contains(output.String(), "FAIL") {
		t.Fatalf("doctor did not expose compile failure:\n%s", output.String())
	}
}

func TestProjectNextReturnsOnlyTheCurrentSafeAction(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	var output bytes.Buffer
	if err := PrintNextStep(root, "", &output); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"workflow: first-business", "progress: 0/", "next: roost add access player", "guide: docs/BEGINNER_WORKBOOK.zh-CN.md"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("next output missing %q:\n%s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "roost add entity Player") {
		t.Fatalf("next printed later steps instead of one action:\n%s", output.String())
	}
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := PrintNextStep(root, "", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "next: roost add entity Player") {
		t.Fatalf("next did not advance to Entity:\n%s", output.String())
	}
}

func TestProjectNextRejectsEmptyDAOAndComponentSkeleton(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	for _, step := range []AddOptions{
		{Kind: "access", Name: "player", Service: "game"},
		{Kind: "entity", Name: "Player"},
		{Kind: "component", Name: "Profile", Entity: "Player"},
		{Kind: "dao", Name: "Player", Entity: "Player"},
	} {
		if _, err := Add(root, step); err != nil {
			t.Fatal(err)
		}
	}
	var output bytes.Buffer
	if err := PrintNextStep(root, "first-business", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "add a persisted field") {
		t.Fatalf("empty DAO was treated as application state:\n%s", output.String())
	}
	daoPath := filepath.Join(root, "db", "def", "player.go")
	raw, err := os.ReadFile(daoPath)
	if err != nil {
		t.Fatal(err)
	}
	raw = bytes.Replace(raw, []byte("\t// Add domain fields here. Example:\n"), []byte("\tName string `bson:\"name\" dao:\"persist,sync\"`\n\t// Add domain fields here. Example:\n"), 1)
	if err := os.WriteFile(daoPath, raw, 0o644); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := PrintNextStep(root, "first-business", &output); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(output.String(), "implement a method") {
		t.Fatalf("Component Name method was treated as business logic:\n%s", output.String())
	}
}

func TestAddManifestMutationRollsBackWhenSyncPreflightFails(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	manifestPath := filepath.Join(root, ManifestName)
	before, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	makefilePath := filepath.Join(root, "Makefile")
	if err := os.WriteFile(makefilePath, []byte("application owned\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "mod", Name: "nest", Service: "game"}); err == nil {
		t.Fatal("add mod unexpectedly succeeded with an unmanaged Makefile conflict")
	}
	after, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatalf("manifest changed after failed sync:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

func TestAddPlayerTCPTransportIsExplicitAndProductionGuarded(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	if _, err := Add(root, AddOptions{Kind: "transport", Name: "tcp"}); err == nil || !strings.Contains(err.Error(), "roost add access player") {
		t.Fatalf("transport without access error = %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	paths, err := Add(root, AddOptions{Kind: "transport", Name: "tcp", Service: "game"})
	if err != nil {
		t.Fatal(err)
	}
	// 15: the k8s Secret example gets the same player_access block as the
	// production example it embeds (RR-20260928-07).
	if len(paths) != 15 {
		t.Fatalf("transport paths = %v", paths)
	}
	for path, fragments := range map[string][]string{
		"roost.yaml": {"transports:", "- tcp"},
		"internal/access/player/tcp/server_gen.go":      {"max_connections_per_ip", "MaxHandshakes", "MaxHandshakeBytes", "gateway.NewTCPServer", "gateway.NewTCPRuntime", "RegistryBound", "bind authenticator"},
		"internal/access/player/tcp/server_gen_test.go": {"TestDeclaredDefaultsAreDefaultConfig", "TestDispatchBudgetFollowsTheNestRequestTimeout", "TestAModStopRetryKeepsTheServerUntilItDrains"},
		"internal/access/player/tcp/auth.go":            {"gateway.ErrUnauthenticated", "newApplicationAuthenticator"},
		"cmd/playerprobe/main.go":                       {"ROOST_PLAYER_TOKEN", "writeAuth", "readAck"},
		"internal/bootstrap/generated.go":               {"accessplayertcp.NewMod()"},
		"configs/examples/access.player.tcp.yaml":       {"enabled: false", "max_handshake_bytes: 8192", "max_payload_bytes: 1048576"},
		"configs/service/config.game.yaml":              {"player_access:", "enabled: false", "max_payload_bytes: 1048576"},
		"configs/service/config.game.prod.example.yaml": {"player_access:", "enabled: false", "max_payload_bytes: 1048576"},
		"deploy/k8s/base/secret.game.example.yaml":      {"    player_access:", "        enabled: false", "        max_payload_bytes: 1048576"},
	} {
		raw, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil {
			t.Errorf("read %s: %v", path, readErr)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(raw), fragment) {
				t.Errorf("%s missing %q:\n%s", path, fragment, raw)
			}
		}
	}
	for path, fragments := range map[string][]string{
		"Dockerfile":                          {"EXPOSE 9100 7000"},
		"deploy/k8s/base/game.yaml":           {"name: player-tcp", "containerPort: 7000", "port: 7000"},
		"deploy/k8s/base/network-policy.yaml": {"roost.tjbdwanghaibo.io/player-access", "port: 7000"},
	} {
		raw, readErr := os.ReadFile(filepath.Join(root, filepath.FromSlash(path)))
		if readErr != nil {
			t.Errorf("read %s: %v", path, readErr)
			continue
		}
		for _, fragment := range fragments {
			if !strings.Contains(string(raw), fragment) {
				t.Errorf("%s missing %q:\n%s", path, fragment, raw)
			}
		}
	}
	var templateDiff bytes.Buffer
	if err := DiffProject(root, &templateDiff); err != nil {
		t.Fatalf("official player TCP workflow must remain codegen-owned: %v", err)
	}
	if !strings.Contains(templateDiff.String(), "summary: 0 file(s) would change") {
		t.Fatalf("official player TCP workflow left stale templates:\n%s", templateDiff.String())
	}
	if _, err := Add(root, AddOptions{Kind: "transport", Name: "tcp"}); err == nil || !strings.Contains(err.Error(), "already exists") {
		t.Fatalf("duplicate transport error = %v", err)
	}
	var output bytes.Buffer
	if err := DoctorWithOptions(context.Background(), root, DoctorOptions{Strict: false, Workflow: "player-tcp"}, &output); err == nil {
		t.Fatal("default reject auth and disabled listener unexpectedly passed player-tcp doctor")
	}
	for _, want := range []string{"workflow:tcp-auth", "workflow:tcp-config"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("doctor output missing %q:\n%s", want, output.String())
		}
	}
}

func TestDockerReadmePublishesPlayerPortForOwningService(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", []string{"alpha", "world"}, []string{"configdata"}, nil)
	manifest.Access = map[string]AccessSpec{
		"player": {Service: "world", Transports: []string{"tcp"}},
	}
	readme := renderDockerReadme(manifest)
	for _, want := range []string{"planet-world-1000", "-p 7000:7000", "planet:v1.0.0 world"} {
		if !strings.Contains(readme, want) {
			t.Errorf("Docker README missing %q:\n%s", want, readme)
		}
	}
}

func TestDoctorPlayerTCPPassesAfterAuthAndConfig(t *testing.T) {
	if !networkTestsEnabled() {
		t.Skip("resolves the published framework from the module proxy; set " + networkTestsEnv + "=1 to run it")
	}
	root := copyOfNewProject(t, "configdata")
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "transport", Name: "tcp"}); err != nil {
		t.Fatal(err)
	}
	authPath := filepath.Join(root, "internal", "access", "player", "tcp", "auth.go")
	auth, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	auth = bytes.Replace(auth, []byte("func (applicationAuthenticator) Authenticate(context.Context, string, net.Addr)"), []byte("func (applicationAuthenticator) Authenticate(_ context.Context, token string, _ net.Addr)"), 1)
	auth = bytes.Replace(auth, []byte("return gateway.Principal{}, gateway.ErrUnauthenticated"), []byte(`return gateway.Principal{PlayerID: int64(len(token)), SessionID: token}, nil`), 1)
	if err := os.WriteFile(authPath, auth, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePlayerTCPConfig(root, "game", "", true); err != nil {
		t.Fatal(err)
	}
	// Doctor is intentionally non-mutating and requires committed dependency
	// checksums. Transactional generation performs the explicit dependency
	// sync step without exposing a partially updated project.
	if err := GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard}, io.Discard); err != nil {
		t.Fatalf("generate before doctor: %v", err)
	}
	var output bytes.Buffer
	if err := DoctorWithOptions(context.Background(), root, DoctorOptions{Strict: false, Workflow: "player-tcp"}, &output); err != nil {
		t.Fatalf("doctor player-tcp: %v\n%s", err, output.String())
	}
}

func TestConfigEnablePlayerTCPRefusesSkeletonThenPreservesConfig(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	if _, err := Add(root, AddOptions{Kind: "access", Name: "player", Service: "game"}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "transport", Name: "tcp"}); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	err := Run([]string{"config", "enable", "player-tcp", "--root", root}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "auth.go is still") {
		t.Fatalf("enable with skeleton auth error = %v", err)
	}
	authPath := filepath.Join(root, "internal", "access", "player", "tcp", "auth.go")
	auth, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	auth = bytes.Replace(auth, []byte("func (applicationAuthenticator) Authenticate(context.Context, string, net.Addr)"), []byte("func (applicationAuthenticator) Authenticate(_ context.Context, token string, _ net.Addr)"), 1)
	auth = bytes.Replace(auth, []byte("return gateway.Principal{}, gateway.ErrUnauthenticated"), []byte(`return gateway.Principal{PlayerID: int64(len(token)), SessionID: token}, nil`), 1)
	if err := os.WriteFile(authPath, auth, 0o644); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(root, "configs", "service", "config.game.yaml")
	config, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config = bytes.Replace(config, []byte("sid: 1000"), []byte("# keep-this-comment\nsid: 1000"), 1)
	if err := os.WriteFile(configPath, config, 0o644); err != nil {
		t.Fatal(err)
	}
	output.Reset()
	if err := Run([]string{"config", "enable", "player-tcp", "--root", root}, &output, &output); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(after, []byte("# keep-this-comment")) || !bytes.Contains(after, []byte("enabled: true")) || bytes.Count(after, []byte("player_access:")) != 1 {
		t.Fatalf("config merge did not preserve content or enable once:\n%s", after)
	}
	if !strings.Contains(output.String(), "project doctor --workflow player-tcp") {
		t.Fatalf("enable output has no next step:\n%s", output.String())
	}
	if err := Run([]string{"config", "disable", "player-tcp", "--root", root}, &output, &output); err != nil {
		t.Fatal(err)
	}
	after, _ = os.ReadFile(configPath)
	if !bytes.Contains(after, []byte("enabled: false")) {
		t.Fatalf("disable did not update config:\n%s", after)
	}
}

func TestPlayerTCPConfigPreservesCRLFAndExistingParentMap(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "config.yaml")
	original := "sid: 1\r\n# application comment\r\nplayer_access:\r\n  gateway_name: edge\r\n"
	if err := os.WriteFile(path, []byte(original), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ensurePlayerTCPConfig(root, "game", path, false); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(bytes.ReplaceAll(raw, []byte("\r\n"), nil), []byte("\n")) {
		t.Fatalf("config line endings were mixed:\n%q", raw)
	}
	for _, want := range []string{"# application comment", "gateway_name: edge", "tcp:", "max_handshake_bytes: 8192"} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("config missing %q:\n%s", want, raw)
		}
	}
}

// player_access.tcp.outbound.resident_bytes 是段里的键：新块写成 outbound: 段，已有 tcp: 没有段时连段名补上，
// 已有段缺这个键时补进段里（沿用段里已有键的缩进），已写的值不动；合并结果按声明读得出来。
func TestPlayerTCPConfigFillsTheOutboundSection(t *testing.T) {
	for name, tc := range map[string]struct{ original, want string }{
		"new block":          {"sid: 1\n", "    outbound:\n      resident_bytes: 268435456\n"},
		"tcp without it":     {"player_access:\n  tcp:\n    enabled: false\n", "    outbound:\n      resident_bytes: 268435456\n"},
		"section without it": {"player_access:\n  tcp:\n    outbound:\n        future_key: 1\n", "    outbound:\n        resident_bytes: 268435456\n        future_key: 1\n"},
		"value kept":         {"player_access:\n  tcp:\n    outbound:\n      resident_bytes: 1048576\n", "    outbound:\n      resident_bytes: 1048576\n"},
	} {
		t.Run(name, func(t *testing.T) {
			merged, err := mergePlayerTCPConfig(tc.original, "config.yaml", false)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(merged, tc.want) || strings.Count(merged, "resident_bytes:") != 1 || strings.Count(merged, "outbound:") != 1 {
				t.Fatalf("merged config lacks %q once:\n%s", tc.want, merged)
			}
			again, err := mergePlayerTCPConfig(merged, "config.yaml", false)
			if err != nil || again != merged {
				t.Fatalf("a second merge changed the config (err %v):\n%s", err, again)
			}
		})
	}
}

func TestCLIRejectsDangerousDefaultsAndUnexpectedArguments(t *testing.T) {
	var output bytes.Buffer
	err := Run([]string{"project", "new", "planet", "-out", filepath.Join(t.TempDir(), "planet")}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "-module is required") {
		t.Fatalf("missing module error = %v", err)
	}
	output.Reset()
	err = Run([]string{"generate", "typo"}, &output, &output)
	if err == nil || !strings.Contains(err.Error(), "unexpected arguments") {
		t.Fatalf("unexpected generate argument error = %v", err)
	}
	output.Reset()
	if err := Run([]string{"version"}, &output, &output); err != nil || !strings.Contains(output.String(), "roost-codegen") {
		t.Fatalf("version output=%q error=%v", output.String(), err)
	}
}

func TestProjectInputSnapshotRejectsConcurrentBusinessEdit(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "bare")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	inputPath := filepath.Join(root, "game", "business.go")
	if err := os.WriteFile(inputPath, []byte("package game\n\nconst Version = 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stage := t.TempDir()
	if err := copyProject(context.Background(), root, stage); err != nil {
		t.Fatal(err)
	}
	inputs, err := snapshotProjectInputs(stage, manifest)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(inputPath, []byte("package game\n\nconst Version = 2\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = verifyProjectInputs(root, manifest, inputs)
	if err == nil || !strings.Contains(err.Error(), "game/business.go") || !strings.Contains(err.Error(), "rerun") {
		t.Fatalf("concurrent input error = %v", err)
	}
}

func TestMergeSyncChangesDeduplicatesDependencyMetadata(t *testing.T) {
	first := syncChange{rel: "go.mod", path: filepath.Join(t.TempDir(), "go.mod"), before: []byte("old"), body: []byte("new"), existed: true}
	merged, err := mergeSyncChanges([]syncChange{first}, []syncChange{first})
	if err != nil {
		t.Fatal(err)
	}
	if len(merged) != 1 || merged[0].rel != "go.mod" {
		t.Fatalf("merged changes = %+v", merged)
	}
	conflict := first
	conflict.body = []byte("different")
	if _, err := mergeSyncChanges([]syncChange{first}, []syncChange{conflict}); err == nil {
		t.Fatal("conflicting duplicate change unexpectedly accepted")
	}
}

func TestRelatedBusinessFilesPreserveConcurrentEntityEdit(t *testing.T) {
	root := t.TempDir()
	entityPath := filepath.Join(root, "game", "entities", "player", "entity.go")
	newPath := filepath.Join(root, "game", "entities", "player", "profile_component.go")
	if err := os.MkdirAll(filepath.Dir(entityPath), 0o755); err != nil {
		t.Fatal(err)
	}
	before := []byte("package player\n\ntype Player struct{}\n")
	concurrent := []byte("package player\n\ntype Player struct{ Revision int }\n")
	if err := os.WriteFile(entityPath, concurrent, 0o644); err != nil {
		t.Fatal(err)
	}
	err := writeRelatedBusinessFiles(entityPath, before, []byte("package player\n\ntype Player struct{ Profile int }\n"), newPath, []byte("package player\n"))
	if err == nil || !strings.Contains(err.Error(), "concurrently changed") {
		t.Fatalf("concurrent Entity error = %v", err)
	}
	if _, err := os.Stat(newPath); !os.IsNotExist(err) {
		t.Fatalf("new companion file was not rolled back: %v", err)
	}
	current, err := os.ReadFile(entityPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(current, concurrent) {
		t.Fatalf("concurrent Entity edit was overwritten:\n%s", current)
	}
}

func TestAddRejectsTraversalInRelatedNames(t *testing.T) {
	root := copyOfNewProject(t, "bare")
	if _, err := Add(root, AddOptions{Kind: "component", Name: "Profile", Entity: `..\..\outside`}); err == nil || !strings.Contains(err.Error(), "invalid Entity name") {
		t.Fatalf("traversal Entity error = %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "protocol", Name: "Login", Group: "game\n//injected", ID: 10001}); err == nil || !strings.Contains(err.Error(), "invalid protocol group") {
		t.Fatalf("invalid protocol group error = %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "protocol", Name: "Login", Group: "game", ID: 999}); err == nil || !strings.Contains(err.Error(), "outside 10000-19999") {
		t.Fatalf("out-of-range protocol id error = %v", err)
	}
	if _, err := Add(root, AddOptions{Kind: "protocol", Name: "Login", Group: "game", ID: 10001}); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "protocol", Name: "LoginAgain", Group: "game", ID: 10001}); err == nil || !strings.Contains(err.Error(), "already used") {
		t.Fatalf("duplicate explicit protocol id error = %v", err)
	}
}

func TestManifestRejectsModulePathInjection(t *testing.T) {
	for _, module := range []string{"example.com/planet\nreplace victim => ../local", `example.com\planet`, "example.com/../planet", `example.com/planet\"`} {
		manifest := DefaultManifest("planet", module, nil, nil, nil)
		if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "invalid module") {
			t.Errorf("module %q error = %v", module, err)
		}
	}
}

func TestGuardedRollbackDoesNotOverwriteConcurrentEdit(t *testing.T) {
	root := t.TempDir()
	rel := "config.yaml"
	path := filepath.Join(root, rel)
	if err := os.WriteFile(path, []byte("before\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before, err := captureFiles(root, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("generated\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	expected, err := captureFiles(root, []string{rel})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("developer edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	err = restoreFilesIfCurrent(root, before, expected, errors.New("later step failed"))
	if err == nil || !strings.Contains(err.Error(), "rollback skipped concurrently changed") {
		t.Fatalf("guarded rollback error = %v", err)
	}
	current, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(current) != "developer edit\n" {
		t.Fatalf("concurrent edit was overwritten: %q", current)
	}
}

func TestAddSkillUsesStablePackageAndNeutralDefinition(t *testing.T) {
	root := copyOfNewProject(t, "bare")
	paths, err := Add(root, AddOptions{Kind: "skill", Name: "Fireball"})
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 3 {
		t.Fatalf("created paths = %v", paths)
	}
	definition, err := os.ReadFile(filepath.Join(root, "game", "skills", "fireball.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"schema": "roost.skill/v2"`, `"id": "skill.planet.fireball"`, `"flow": "finish"`} {
		if !bytes.Contains(definition, []byte(want)) {
			t.Errorf("Skill definition missing %q:\n%s", want, definition)
		}
	}
	catalog, err := os.ReadFile(filepath.Join(root, "game", "skills", "catalog.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(catalog, []byte("github.com/tjbdwanghaibo/roost-core/gameplay/skill")) {
		t.Fatalf("Skill catalog must use the stable package:\n%s", catalog)
	}
}

// 技能目录在生成工程里必须有地方编译：add skill 首次生成 catalog.go 时一并生成 catalog_test.go，用框架的正式
// 加载路径（CompileAll → skill.Parse / skill.Compile）编译全部 JSON，写坏的定义在 go test 就失败，而不是等到业务
// 自己接上编译或上线后第一次施放。已有 catalog.go、还没有测试的工程，下一次 add skill 补上测试。
func TestAddSkillGeneratesACatalogTest(t *testing.T) {
	root := copyOfNewProject(t, "bare")
	paths, err := Add(root, AddOptions{Kind: "skill", Name: "Fireball"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, "game/skills/catalog_test.go") {
		t.Fatalf("created paths = %v, want game/skills/catalog_test.go", paths)
	}
	test, err := os.ReadFile(filepath.Join(root, "game", "skills", "catalog_test.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"func TestSkillCatalogCompiles(t *testing.T)", "CompileAll(skill.DefaultCompileEnvironment())"} {
		if !bytes.Contains(test, []byte(want)) {
			t.Errorf("catalog_test.go missing %q:\n%s", want, test)
		}
	}
	// 已有目录、没有测试：下一次 add skill 补上；已有测试（业务文件）不改写。
	if err := os.Remove(filepath.Join(root, "game", "skills", "catalog_test.go")); err != nil {
		t.Fatal(err)
	}
	paths, err = Add(root, AddOptions{Kind: "skill", Name: "IceBolt"})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(paths, "game/skills/catalog_test.go") || slices.Contains(paths, "game/skills/catalog.go") {
		t.Fatalf("second add paths = %v, want the missing test added and catalog.go left alone", paths)
	}
	custom := []byte("package skills\n\n// business-owned\n")
	if err := os.WriteFile(filepath.Join(root, "game", "skills", "catalog_test.go"), custom, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "skill", Name: "Heal"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "game", "skills", "catalog_test.go")); !bytes.Equal(got, custom) {
		t.Fatalf("add skill rewrote the business-owned catalog_test.go:\n%s", got)
	}
}

// 端到端：生成的目录测试真的编译 JSON。把火球的 flow 改成不存在的流程后，go test ./game/skills/ 与 doctor 的
// skills:catalog 都失败并带框架编译器的原话（修前 build / test / doctor 全部通过，只有手动调用 CompileAll 才报错）。
func TestSkillCatalogTestRejectsABrokenDefinition(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles a generated project")
	}
	root := copyOfNewProject(t, "bare")
	if _, err := Add(root, AddOptions{Kind: "skill", Name: "Fireball"}); err != nil {
		t.Fatal(err)
	}
	useLocalRoostCore(t, root)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if item, ok := checkSkillCatalog(ctx, root); !ok || item.Status != StatusOK {
		t.Fatalf("fresh Skill catalog: %+v (reported %v)", item, ok)
	}
	path := filepath.Join(root, "game", "skills", "fireball.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	broken := bytes.Replace(raw, []byte(`"flow": "finish"`), []byte(`"flow": "explode_everything"`), 1)
	if bytes.Equal(broken, raw) {
		t.Fatal("fireball.json changed shape; update this test")
	}
	if err := os.WriteFile(path, broken, 0o644); err != nil {
		t.Fatal(err)
	}
	item, ok := checkSkillCatalog(ctx, root)
	if !ok || item.Status != StatusFail || !strings.Contains(item.Detail, `unsupported flow "explode_everything"`) {
		t.Fatalf("broken Skill definition: skills:catalog = %+v (reported %v); want a FAIL quoting the compiler", item, ok)
	}
}

func TestManifestRejectsIDsOutsideFrameworkEncoding(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	space := manifest.IDs["entity"]
	space.Max = 256
	manifest.IDs["entity"] = space
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "encoding limit 255") {
		t.Fatalf("invalid EntityKind range error = %v", err)
	}
	space = manifest.IDs["entity"]
	space.Max = 255
	manifest.IDs["entity"] = space
	protocol := manifest.IDs["protocol"]
	protocol.Groups["game"] = IDRange{Min: 1, Max: 4294967296}
	manifest.IDs["protocol"] = protocol
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "encoding limit 4294967295") {
		t.Fatalf("invalid protocol range error = %v", err)
	}
	protocol.Groups["game"] = IDRange{Min: 10000, Max: 19999}
	manifest.IDs["protocol"] = protocol
	// 1-999 是框架段（errcode.CodeInternal、errcode.CodeEntityNotFound …）：业务码落进去会与框架码撞号。
	manifest.IDs["errcode"] = IDSpace{Min: 1, Max: 99999}
	if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), "framework segment 1-999") {
		t.Fatalf("errcode range overlapping the framework segment: error = %v", err)
	}
}

func TestEndpointArgumentsMapNamedRequestFields(t *testing.T) {
	directory := t.TempDir()
	protocolPath := filepath.Join(directory, "protocol.go")
	handlerPath := filepath.Join(directory, "handler.go")
	if err := os.WriteFile(protocolPath, []byte("package def\ntype RenamePlayerRequest struct { Name string; Locale string }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(handlerPath, []byte("package handler\n\nimport player \"example.com/planet/game/entities/player\"\n\nfunc handlerRenamePlayer(target player.IPlayerEntity, name, locale string) error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	signature, err := endpointArguments(protocolPath, "RenamePlayer", handlerPath, "RenamePlayer")
	if err != nil {
		t.Fatal(err)
	}
	arguments, target := signature.arguments, signature.entity
	if got, want := strings.Join(arguments, ","), "request.Name,request.Locale"; got != want {
		t.Fatalf("endpoint arguments = %q, want %q", got, want)
	}
	// The Entity the handler locks is what the endpoint builds the full id
	// from; the bare context.PlayerID is not an entity id (see endpointEntity).
	want := endpointEntity{alias: "player", importPath: "example.com/planet/game/entities/player", kind: "EntityKindPlayer"}
	if target != want {
		t.Fatalf("endpoint entity = %+v, want %+v", target, want)
	}
	// A target the scaffold cannot resolve to a package is refused up front,
	// not turned into an endpoint that fails on its first request.
	if err := os.WriteFile(handlerPath, []byte("package handler\nfunc handlerRenamePlayer(target IPlayerEntity, name, locale string) error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := endpointArguments(protocolPath, "RenamePlayer", handlerPath, "RenamePlayer"); err == nil || !strings.Contains(err.Error(), "<package>.I<Component>Entity") {
		t.Fatalf("unqualified Entity target accepted: %v", err)
	}
}

func TestAddSagaGeneratesValidatedDefinition(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"saga"}})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := Add(root, AddOptions{Kind: "saga", Name: "AllianceRally", Steps: []string{"ReserveTroops", "CreateMarch"}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := paths[0], "saga/alliance_rally/definition.go"; got != want {
		t.Fatalf("path = %s, want %s", got, want)
	}
	raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(paths[0])))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"= \"alliance_rally\"", "Version uint32 = 1", "DefinitionVersion: Version", "func Definitions() []saga.Definition", "reserve_troops.compensate", "func Register(engine *saga.Engine) error", "func EmitStart(", "saga.EmitStart", "engine.StartSaga", "TopicReserveTroopsCompensation = \"alliance_rally.reserve_troops.compensate\""} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("generated Saga missing %q:\n%s", want, raw)
		}
	}
	// 步骤消费者只有一种写法：steps.go 经 wiring/saga Steps 登记（D12 方向 B 去掉了 Subscribe* 辅助）。
	if strings.Contains(string(raw), "func Subscribe") {
		t.Fatalf("definition.go still generates Subscribe* helpers next to steps.go:\n%s", raw)
	}
	if got, want := paths[1], "saga/alliance_rally/steps.go"; got != want {
		t.Fatalf("second path = %s, want %s", got, want)
	}
	bootstrap, err := os.ReadFile(filepath.Join(root, "internal", "bootstrap", "generated.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"var EntityAccess = entity.NewManagerAccess(EntityManager)", "kitsaga.NewMod(kitsaga.CombineDefinitions(sagaAllianceRally.Definitions())...)", "example.com/planet/saga/alliance_rally"} {
		if !strings.Contains(string(bootstrap), want) {
			t.Fatalf("bootstrap missing %q:\n%s", want, bootstrap)
		}
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(manifest.Services["game"].Mods, "saga") || !contains(manifest.Sagas, "alliance_rally") {
		t.Fatalf("Saga was not wired into manifest: %+v", manifest)
	}
}

func TestAddSagaRequiresSteps(t *testing.T) {
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"saga"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "AllianceRally"}); err == nil {
		t.Fatal("expected missing steps to fail")
	}
}

// Saga 按 Definition.Steps 的下标推进、按下标倒序补偿，所以 -steps 的书写顺序
// 就是业务顺序：生成物必须原样保留，重复的步骤名报错而不是被静默合并。
func TestAddSagaKeepsDeclaredStepOrder(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"saga"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name  string
		steps string
		want  []string
	}{
		{name: "gift_gold", steps: "debit,credit", want: []string{"debit", "credit"}},
		// roost help saga 里的示例顺序。
		{name: "cross_trade", steps: "reserve, deduct ,deliver", want: []string{"reserve", "deduct", "deliver"}},
	} {
		var out bytes.Buffer
		if err := Run([]string{"add", "saga", tc.name, "-steps", tc.steps, "--root", root}, &out, &out); err != nil {
			t.Fatalf("add saga %s: %v\n%s", tc.name, err, out.String())
		}
		raw, err := os.ReadFile(filepath.Join(root, "saga", tc.name, "definition.go"))
		if err != nil {
			t.Fatal(err)
		}
		var got []string
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if strings.HasPrefix(line, "{Name: \"") {
				got = append(got, strings.SplitN(strings.TrimPrefix(line, "{Name: \""), "\"", 2)[0])
			}
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("saga %s Definition step order = %v, want %v (as given by -steps %q)", tc.name, got, tc.want, tc.steps)
		}
	}
}

func TestAddSagaRejectsDuplicateSteps(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"saga"}})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	err = Run([]string{"add", "saga", "rally", "-steps", "reserve,deduct,reserve", "--root", root}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), `duplicate saga step "reserve"`) {
		t.Fatalf("duplicate -steps accepted or unclear error: %v", err)
	}
	// 只差大小写的两个名字生成同一个蛇形步骤名与同一组 topic，同样是重复。
	if _, err := Add(root, AddOptions{Kind: "saga", Name: "Rally", Steps: []string{"ReserveTroops", "reserve_troops"}}); err == nil || !strings.Contains(err.Error(), `duplicate saga step "reserve_troops"`) {
		t.Fatalf("steps that collide after snake_case accepted or unclear error: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "saga", "rally")); !os.IsNotExist(err) {
		t.Fatalf("rejected saga left files behind: %v", err)
	}
}

// -template 把“第一个声明的业务服务”接到九个托管框架服务上；-services 的书写
// 顺序因此有含义，不能先排序再取第一个。
func TestProjectNewTemplateWiresFirstDeclaredService(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	var out bytes.Buffer
	if err := Run([]string{"project", "new", "planet", "-module", "example.com/planet", "-out", target, "-services", "world,game", "-template", "game", "--skip-deps"}, &out, &out); err != nil {
		t.Fatalf("project new: %v\n%s", err, out.String())
	}
	manifest, err := LoadManifest(target)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Services["world"].Uses) == 0 || len(manifest.Services["game"].Uses) != 0 {
		t.Fatalf("template wired uses world=%v game=%v, want the first declared service (world)", manifest.Services["world"].Uses, manifest.Services["game"].Uses)
	}
}

func TestFrameworkMinimumVersionGuard(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"saga"}, []string{"saga"})
	m.Versions.Core = "v1.7.9"
	if err := m.Validate(); err == nil || !strings.Contains(err.Error(), minimumVersions.Core) {
		t.Fatalf("expected framework minimum version guard, got %v", err)
	}
	m.Versions.Core = minimumVersions.Core
	if err := m.Validate(); err != nil {
		t.Fatalf("minimum supported versions rejected: %v", err)
	}
}

func TestReplicationPresetCompilesAsGoSource(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"etcd"}, []string{"nettransport-quic", "nettransport-kcp", "nettransport-udp"})
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	file, ok := plan["internal/transport/generated.go"]
	if !ok || !bytes.Contains(file.Body, []byte("func NewQUIC")) || !bytes.Contains(file.Body, []byte("func NewKCP")) || !bytes.Contains(file.Body, []byte("func NewUDP")) || !bytes.Contains(file.Body, []byte("func NewSyncTransport")) {
		t.Fatalf("replication preset missing:\n%s", file.Body)
	}
}

func TestProductionRenderingIncludesDurabilityAndTopologyGuards(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"mongo", "redis", "nats", "dataengine", "nest"}, []string{"config", "nest"})
	production := renderServiceConfig(m, "game", true)
	for _, want := range []string{
		"require_replica_set: true",
		"transaction_timeout: 30s",
		"transaction_receipt_ttl: 720h",
		"dir: /var/lib/roost/wal",
		"max_disk_bytes: 8589934592",
		"duplicate_window: 10m",
		"replicas: 3",
		"addr: 0.0.0.0:9100",
	} {
		if !strings.Contains(production, want) {
			t.Errorf("production config missing %q:\n%s", want, production)
		}
	}
	if strings.Contains(production, "127.0.0.1") || strings.Contains(production, "file: true") {
		t.Fatalf("development topology leaked into production config:\n%s", production)
	}
	// 开发配置不写 dataengine.wal.dir（运行期按 sid 分目录），生产示例由 productionizeConfig 补上持久卷；
	// 再改写一次（add mod 追加段等）不能写出第二个 dir。
	dev := renderServiceConfig(m, "game", false)
	for _, line := range strings.Split(dev, "\n") {
		if value, ok := strings.CutPrefix(strings.TrimSpace(line), "dir: "); ok && strings.Contains(value, "wal") {
			t.Errorf("development config pins the WAL directory %q instead of the per-sid default:\n%s", value, dev)
		}
	}
	if again := productionizeConfig(production); again != production || strings.Count(production, "dir: /var/lib/roost/wal") != 1 {
		t.Errorf("productionizeConfig is not idempotent on the WAL directory:\n%s", again)
	}
	dockerfile := renderDockerfile(m)
	if !strings.Contains(dockerfile, `VOLUME ["/var/lib/roost/wal"]`) || !strings.Contains(dockerfile, "USER nonroot:nonroot") || !strings.Contains(dockerfile, "go mod download && go mod verify") {
		t.Fatalf("durable non-root Dockerfile missing:\n%s", dockerfile)
	}
	if strings.Contains(dockerfile, "COPY configs") {
		t.Fatalf("production image must not bake configuration or secrets:\n%s", dockerfile)
	}
	compose := renderCompose(m)
	if !strings.Contains(compose, `"--appendfsync", "everysec"`) {
		t.Fatalf("Redis AOF configuration missing from compose:\n%s", compose)
	}
	if !strings.Contains(compose, `"-sd", "/data"`) || !strings.Contains(compose, "mongo-data:/data/db") {
		t.Fatalf("development dependencies are not durable:\n%s", compose)
	}
	if m.Versions.Core != "latest" {
		t.Fatalf("latest version policy defaults = %+v", m.Versions)
	}
}

func TestProductionDeploymentRendering(t *testing.T) {
	m := DefaultManifest(
		"planet",
		"example.com/planet",
		[]string{"game", "gate"},
		[]string{"mongo", "redis", "nats", "dataengine", "nest"},
		[]string{"config", "nest"},
	)
	// Exercise both workload classes: the gate is intentionally stateless.
	m.Services["gate"] = ServiceSpec{Mods: []string{"etcd", "ops"}}

	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		"deploy/shell/build.sh",
		"deploy/shell/install.sh",
		"deploy/shell/healthcheck.sh",
		"deploy/docker/README.md",
		"deploy/k8s/base/kustomization.yaml",
		"deploy/k8s/base/network-policy.yaml",
		"deploy/k8s/base/game.yaml",
		"deploy/k8s/base/gate.yaml",
		"deploy/k8s/base/secret.game.example.yaml",
		"docs/IMPLEMENTATION.zh-CN.md",
		"docs/DEPLOYMENT.zh-CN.md",
	} {
		if _, ok := plan[path]; !ok {
			t.Errorf("deployment plan missing %s", path)
		}
	}

	game := string(plan["deploy/k8s/base/game.yaml"].Body)
	for _, want := range []string{"kind: StatefulSet", "volumeClaimTemplates:", "readOnlyRootFilesystem: true", "startupProbe:", "readinessProbe:", "livenessProbe:"} {
		if !strings.Contains(game, want) {
			t.Errorf("stateful workload missing %q:\n%s", want, game)
		}
	}
	gate := string(plan["deploy/k8s/base/gate.yaml"].Body)
	if !strings.Contains(gate, "kind: Deployment") || strings.Contains(gate, "volumeClaimTemplates:") {
		t.Fatalf("stateless workload rendered incorrectly:\n%s", gate)
	}
	secret := string(plan["deploy/k8s/base/secret.game.example.yaml"].Body)
	if !strings.Contains(secret, "CHANGE_ME") || !strings.Contains(secret, "addr: 0.0.0.0:9100") {
		t.Fatalf("secret example is not fail-closed or probe-ready:\n%s", secret)
	}
	kustomization := string(plan["deploy/k8s/base/kustomization.yaml"].Body)
	if strings.Contains(kustomization, "secret.") {
		t.Fatalf("secret examples must not be applied by kustomize:\n%s", kustomization)
	}

	for path, file := range plan {
		if strings.HasPrefix(path, "deploy/k8s/") && strings.HasSuffix(path, ".yaml") {
			assertYAMLDocuments(t, path, file.Body)
		}
	}
	installer := string(plan["deploy/shell/install.sh"].Body)
	for _, want := range []string{"releases/$VERSION", "switch_release", "wait_ready", "rolling back", "ProtectKernelTunables=true"} {
		if !strings.Contains(installer, want) {
			t.Errorf("systemd deployer missing %q:\n%s", want, installer)
		}
	}
}

func assertYAMLDocuments(t *testing.T, path string, body []byte) {
	t.Helper()
	decoder := yaml.NewDecoder(bytes.NewReader(body))
	for document := 1; ; document++ {
		var value any
		err := decoder.Decode(&value)
		if err == io.EOF {
			return
		}
		if err != nil {
			t.Fatalf("decode %s document %d: %v\n%s", path, document, err, body)
		}
	}
}

func TestBootstrapImportsTransitiveDataEngineDependencies(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"nest"}, []string{"nest"})
	bootstrap := renderBootstrap(m)
	for _, want := range []string{
		`kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"`,
		`kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"`,
		`kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"`,
		"kitdataengine.NewMod(kitdataengine.WithEntityAccess(EntityAccess))",
	} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap missing transitive dependency %q:\n%s", want, bootstrap)
		}
	}
	for _, forbidden := range []string{"kitcheckpoint", "kitnestwal", "roost-kit/checkpoint", "checkpoint.NewMod"} {
		if strings.Contains(bootstrap, forbidden) {
			t.Fatalf("bootstrap retained legacy persistence output %q:\n%s", forbidden, bootstrap)
		}
	}
}

func TestBootstrapWiresDataEngineWithEntityAccess(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"dataengine", "nest"}, []string{"entity", "nest", "dao"})
	bootstrap := renderBootstrap(m)
	for _, want := range []string{
		`kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"`,
		`kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"`,
		`kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"`,
		"kitdataengine.NewMod(kitdataengine.WithEntityAccess(EntityAccess))",
		"kitnest.NewMod(EntityAccess)",
	} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap missing Data Engine wiring %q:\n%s", want, bootstrap)
		}
	}
	if strings.Contains(bootstrap, "kitcheckpoint") || strings.Contains(bootstrap, "kitnestwal") {
		t.Fatalf("new Data Engine bootstrap retained legacy writers:\n%s", bootstrap)
	}
}

func TestRenderGoModUsesPublishedModulesWithoutReplace(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	goMod := renderGoMod(m)
	if want := "github.com/tjbdwanghaibo/roost-core " + minimumVersions.Core; !strings.Contains(goMod, want) {
		t.Errorf("go.mod missing %q:\n%s", want, goMod)
	}
	// And exactly one framework module: kit is a package path inside
	// roost-core since the consolidation, so a require for it would name a
	// module that does not exist (三仓合一仓 P3).
	if strings.Contains(goMod, "roost-core/kit v") || strings.Contains(goMod, "roost-kit ") {
		t.Errorf("go.mod requires kit as a module of its own:\n%s", goMod)
	}
	if strings.Contains(goMod, "replace ") {
		t.Fatalf("generated release go.mod contains replace directive:\n%s", goMod)
	}
	if strings.Contains(goMod, "roost-core latest") || strings.Contains(goMod, "roost-kit latest") {
		t.Fatalf("go.mod must contain resolved semantic versions, not module queries:\n%s", goMod)
	}
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	deps, ok := plan["internal/frameworkdeps/generated.go"]
	if !ok || !strings.Contains(string(deps.Body), "github.com/tjbdwanghaibo/roost-core/gameplay/skill") || strings.Contains(string(deps.Body), "roost-skill") {
		t.Fatalf("generated project does not pin the skill module: %q", deps.Body)
	}
	makefile := string(plan["Makefile"].Body)
	if !strings.Contains(makefile, "deps-update:") || !strings.Contains(makefile, "$(ROOST) project deps") {
		t.Fatalf("generated Makefile does not expose latest dependency resolution:\n%s", makefile)
	}
	for _, want := range []string{
		"project-upgrade:",
		"go run $(CODEGEN_MODULE)/cmd/roost@latest project upgrade --root . -core latest",
		"roost-up:",
		"GOWORK=off go get -u ./...",
		"GOWORK=off go mod tidy",
		"codegen-up:",
		"go install $(CODEGEN_MODULE)/cmd/roost@latest",
		"COMPONENT ?=",
		"HANDLER ?=",
		"new-handler:",
		"new-access:",
		"new-lifecycle:",
		"new-endpoint:",
		"new-skill:",
		"glsvet:",
		"go run github.com/tjbdwanghaibo/roost-core/cmd/glsvet ./...",
	} {
		if !strings.Contains(makefile, want) {
			t.Errorf("generated Makefile missing %q:\n%s", want, makefile)
		}
	}
	ci := string(plan[".github/workflows/ci.yml"].Body)
	if strings.Contains(ci, "make deps-update") || strings.Contains(ci, "go get -u") {
		t.Fatalf("ordinary generated CI must use the committed dependency graph:\n%s", ci)
	}
	for _, want := range []string{"go mod verify", "go test -race ./...", "roost-core/cmd/glsvet", "docker compose", "kubectl kustomize"} {
		if !strings.Contains(ci, want) {
			t.Errorf("generated CI missing %q:\n%s", want, ci)
		}
	}
	for _, path := range []string{
		".github/workflows/dependency-update.yml",
		".github/workflows/release.yml",
		".github/workflows/deploy-shell.yml",
		".github/workflows/deploy-docker.yml",
		".github/workflows/deploy-k8s.yml",
		".github/dependabot.yml",
		".github/actionlint.yaml",
		"deploy/docker/docker-compose.prod.yaml",
		"deploy/docker/deploy.sh",
		"deploy/docker/rollback.sh",
		"deploy/k8s/deploy.sh",
		"deploy/k8s/overlays/staging/kustomization.yaml",
		"deploy/k8s/overlays/production/kustomization.yaml",
		"cmd/healthprobe/main.go",
	} {
		if _, ok := plan[path]; !ok {
			t.Errorf("generated CI/CD plan missing %s", path)
		} else if strings.HasSuffix(path, ".yml") {
			assertYAMLDocuments(t, path, plan[path].Body)
			if strings.HasPrefix(path, ".github/workflows/") {
				assertWorkflowActionsPinned(t, path, plan[path].Body)
			}
		}
	}
	dependencyUpdate := string(plan[".github/workflows/dependency-update.yml"].Body)
	if !strings.Contains(dependencyUpdate, "project upgrade") || !strings.Contains(dependencyUpdate, "gh pr create") {
		t.Fatalf("dependency updates must be isolated in a tested pull request workflow:\n%s", dependencyUpdate)
	}
	release := string(plan[".github/workflows/release.yml"].Body)
	for _, want := range []string{"linux/amd64,linux/arm64", "SHA256SUMS", "attest-build-provenance", "sbom: true", "gh release create"} {
		if !strings.Contains(release, want) {
			t.Errorf("generated release workflow missing %q:\n%s", want, release)
		}
	}
}

func assertWorkflowActionsPinned(t *testing.T, path string, body []byte) {
	t.Helper()
	for lineNumber, line := range strings.Split(string(body), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "- uses: ") && !strings.HasPrefix(line, "uses: ") {
			continue
		}
		parts := strings.SplitN(line, "@", 2)
		if len(parts) != 2 || strings.HasPrefix(strings.TrimSpace(parts[1]), "./") {
			continue
		}
		ref := strings.Fields(parts[1])[0]
		if len(ref) != 40 || strings.Trim(ref, "0123456789abcdef") != "" {
			t.Errorf("%s:%d action is not pinned to a full commit SHA: %s", path, lineNumber+1, line)
		}
	}
}

func TestRepositoryWorkflowsAreValidAndPinned(t *testing.T) {
	paths, err := filepath.Glob(filepath.Join("..", "..", "..", ".github", "workflows", "*.yml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) == 0 {
		t.Fatal("repository has no workflows")
	}
	for _, path := range paths {
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatal(readErr)
		}
		assertYAMLDocuments(t, filepath.ToSlash(path), raw)
		assertWorkflowActionsPinned(t, filepath.ToSlash(path), raw)
	}
}

func FuzzValidModulePath(f *testing.F) {
	for _, seed := range []string{"example.com/planet", "github.com/acme/game-server", "", "../escape", "C:\\repo", "example.com//bad"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, value string) {
		_ = validModulePath(value)
	})
}

func FuzzManifestDecode(f *testing.F) {
	valid, err := DefaultManifest("planet", "example.com/planet", nil, nil, nil).Marshal()
	if err != nil {
		f.Fatal(err)
	}
	f.Add(valid)
	f.Add([]byte("schema: [\x00"))
	f.Fuzz(func(t *testing.T, raw []byte) {
		manifest := Manifest{CICD: defaultCICDSpec()}
		decoder := yaml.NewDecoder(bytes.NewReader(raw))
		decoder.KnownFields(true)
		if decoder.Decode(&manifest) == nil {
			_ = manifest.Validate()
		}
	})
}

func BenchmarkRenderProject(b *testing.B) {
	manifest := DefaultManifest("planet", "example.com/planet", []string{"game", "gate"}, nil, nil)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if _, err := renderProject(manifest); err != nil {
			b.Fatal(err)
		}
	}
}

func TestGeneratedBeginnerAndManifestDocumentationIsComplete(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	manifestGuide := string(plan["docs/ROOST_YAML.zh-CN.md"].Body)
	for _, want := range []string{
		"schema", "project.name", "project.module", "versions.core", "versions.kit / skill / service / codegen 已退役",
		"shared_mods", "services.<name>.mods",
		"cicd.provider", "cicd.registry", "cicd.environments", "cicd.deploy",
		"access.player.service", "features", "sagas", "ids", "groups", "min", "max", "完整示例",
		"Feature 和 Mod 必须分别理解", "roost id next protocol -group game",
		minimumVersions.Core,
	} {
		if !strings.Contains(manifestGuide, want) {
			t.Errorf("manifest guide missing %q:\n%s", want, manifestGuide)
		}
	}
	for mod := range modCatalog {
		if !strings.Contains(manifestGuide, "| "+mod+" |") {
			t.Errorf("manifest guide missing mod %q", mod)
		}
	}
	for feature := range knownFeatures {
		if !strings.Contains(manifestGuide, "| "+feature+" |") {
			t.Errorf("manifest guide missing feature %q", feature)
		}
	}
	quickstart := string(plan["docs/QUICKSTART.zh-CN.md"].Body)
	for _, want := range []string{
		"零基础快速开始", "Go " + generatedGoVersion, "Service", "Mod", "Feature", "Generated file",
		"make deps-update", "make generate", "make doctor", "make run SERVICE=game",
		"roost add access player --service game", "roost add protocol", "roost add endpoint", "常见问题", "ROOST_YAML.zh-CN.md",
	} {
		if !strings.Contains(quickstart, want) {
			t.Errorf("beginner guide missing %q:\n%s", want, quickstart)
		}
	}
	entityGuide := string(plan["docs/ENTITY_COMPONENT.zh-CN.md"].Body)
	for _, want := range []string{
		"roost add component Profile --entity Player", "roost add dao Player --entity Player",
		"roost add mod nest -service game",
		"roost add handler RenamePlayer --entity Player --component Profile",
		`import player "example.com/planet/game/entities/player"`, "SetName(name)",
		"业务层不要再次", "rollback=undo durability=strict", "ID、id、tracker",
	} {
		if !strings.Contains(entityGuide, want) {
			t.Errorf("Entity beginner guide missing %q:\n%s", want, entityGuide)
		}
	}
}

// sync 重写过时的受控生成文件（这里是缺少新目标的生成 Makefile）。
func TestSyncRewritesAnOutdatedGeneratedMakefile(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "bare")
	makefilePath := filepath.Join(root, "Makefile")
	legacy := []byte("# Code generated by roost-codegen. DO NOT EDIT.\n\n.PHONY: sync\nsync:\n\t$(ROOST) project sync\n")
	if err := os.WriteFile(makefilePath, legacy, 0o644); err != nil {
		t.Fatal(err)
	}
	result, err := SyncProject(root)
	if err != nil {
		t.Fatalf("upgrade legacy project: %v", err)
	}
	if !contains(result.Updated, "Makefile") {
		t.Fatalf("updated files = %v, want Makefile", result.Updated)
	}
	upgraded, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"project-upgrade:", "roost-up:", "codegen-up:"} {
		if !bytes.Contains(upgraded, []byte(want)) {
			t.Errorf("upgraded Makefile missing %q:\n%s", want, upgraded)
		}
	}
}

func TestSyncRefusesUnmanagedMakefile(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "bare")
	makefilePath := filepath.Join(root, "Makefile")
	custom := []byte("build:\n\tgo build ./...\n")
	if err := os.WriteFile(makefilePath, custom, 0o644); err != nil {
		t.Fatal(err)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Versions.Core = minimumVersions.Core
	if err := saveManifest(root, manifest); err != nil {
		t.Fatal(err)
	}
	manifestPath := filepath.Join(root, ManifestName)
	manifestBefore, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"project", "upgrade", "--root", root, "-core", "latest"}, io.Discard, io.Discard); err == nil || !strings.Contains(err.Error(), "preflight project upgrade") {
		t.Fatalf("expected upgrade preflight conflict, got %v", err)
	}
	manifestAfter, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(manifestBefore, manifestAfter) {
		t.Fatal("failed upgrade modified roost.yaml before ownership preflight")
	}
	if _, err := SyncProject(root); err == nil || !strings.Contains(err.Error(), "refusing to overwrite non-generated file Makefile") {
		t.Fatalf("expected unmanaged Makefile conflict, got %v", err)
	}
	after, err := os.ReadFile(makefilePath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(after, custom) {
		t.Fatal("unmanaged Makefile was modified")
	}
}

func TestDiffIgnoresResolvedUserOwnedGoMod(t *testing.T) {
	root := copyOfNewProject(t, "bare")
	goModPath := filepath.Join(root, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	goMod = bytes.Replace(goMod, []byte(minimumVersions.Core), []byte("v1.99.0"), 1)
	if err := os.WriteFile(goModPath, goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := DiffProject(root, &output); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(output.String(), "go.mod") {
		t.Fatalf("project diff reported the dependency resolver-owned go.mod:\n%s", output.String())
	}
}

func TestSyncPreflightsConflictsBeforeWriting(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"config"}})
	if err != nil {
		t.Fatal(err)
	}
	makefile := filepath.Join(root, "Makefile")
	before, err := os.ReadFile(makefile)
	if err != nil {
		t.Fatal(err)
	}
	bootstrap := filepath.Join(root, "internal", "bootstrap", "generated.go")
	if err := os.WriteFile(bootstrap, []byte("package bootstrap\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	m.Services["gate"] = ServiceSpec{Mods: []string{"etcd"}}
	if err := saveManifest(root, m); err != nil {
		t.Fatal(err)
	}
	if _, err := SyncProject(root); err == nil {
		t.Fatal("expected ownership conflict")
	}
	after, err := os.ReadFile(makefile)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(before, after) {
		t.Fatal("Makefile changed before a later preflight conflict")
	}
}

func TestSyncConcurrencyGuardRefusesDrift(t *testing.T) {
	path := filepath.Join(t.TempDir(), "generated.go")
	before := []byte("before")
	if err := os.WriteFile(path, before, 0o644); err != nil {
		t.Fatal(err)
	}
	change := syncChange{rel: "generated.go", path: path, before: before, body: []byte("generated"), existed: true}
	if err := os.WriteFile(path, []byte("developer edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := verifySyncChangeCurrent(change); err == nil || !strings.Contains(err.Error(), "concurrently changed") {
		t.Fatalf("expected concurrency conflict, got %v", err)
	}
}

func TestSyncRollbackPreservesConcurrentEdit(t *testing.T) {
	path := filepath.Join(t.TempDir(), "generated.go")
	change := syncChange{rel: "generated.go", path: path, before: []byte("before"), body: []byte("generated"), existed: true}
	if err := os.WriteFile(path, []byte("developer edit"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := rollbackSync([]syncChange{change}); err == nil || !strings.Contains(err.Error(), "rollback skipped") {
		t.Fatalf("expected rollback conflict, got %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != "developer edit" {
		t.Fatalf("rollback overwrote concurrent edit: %q", after)
	}
}

func TestSyncCommitsGeneratedOrphanDeletion(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"}, Features: []string{"dao"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "dao", Name: "player"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(root, "db", "gen_player_dao.go")
	if _, err := os.Stat(generated); err != nil {
		t.Fatalf("generated DAO missing before orphan test: %v", err)
	}
	if err := os.Remove(filepath.Join(root, "db", "def", "player.go")); err != nil {
		t.Fatal(err)
	}
	result, err := SyncProject(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("orphan generated DAO still exists: %v", err)
	}
	if !contains(result.Removed, "db/gen_player_dao.go") {
		t.Fatalf("removed files = %v", result.Removed)
	}
}

func TestTransactionalGenerateDoesNotCommitEarlierGeneratorOnLaterFailure(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata"}, Features: []string{"dao", "nest"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "dao", Name: "player"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	generatedPath := filepath.Join(root, "db", "gen_player_dao.go")
	generatedBefore, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatal(err)
	}
	definitionPath := filepath.Join(root, "db", "def", "player.go")
	definition, err := os.ReadFile(definitionPath)
	if err != nil {
		t.Fatal(err)
	}
	definition = bytes.Replace(definition, []byte("type PlayerDao struct {"), []byte("type PlayerDao struct {\n\tName string `dao:\"persist,sync\"`"), 1)
	if err := os.WriteFile(definitionPath, definition, 0o644); err != nil {
		t.Fatal(err)
	}
	invalidHandler := filepath.Join(root, "game", "handler", "broken.go")
	if err := os.WriteFile(invalidHandler, []byte("package handler\n//roost:nest rollback=undo durability=strict\nfunc handlerBroken("), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := GenerateTransactional(context.Background(), root, GenerateOptions{Stdout: io.Discard}, io.Discard); err == nil {
		t.Fatal("invalid later generator unexpectedly succeeded")
	}
	generatedAfter, err := os.ReadFile(generatedPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(generatedBefore, generatedAfter) {
		t.Fatal("failed transactional generation committed an earlier DAO output")
	}
}
