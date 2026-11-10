package roost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/tjbdwanghaibo/roost-core/modules"
)

// modules/embed.go 用通配嵌入每个模块的 module.yaml 与 files/。磁盘上的模板必须全部在嵌入集合里，
// 且每个模块的清单都通过校验（清单与模板里的 ID 占位一一对应）。
func TestModuleCatalogEmbedsEveryTemplateAndValidates(t *testing.T) {
	catalog, err := loadModuleCatalog()
	if err != nil {
		t.Fatal(err)
	}
	if len(catalog) == 0 {
		t.Fatal("no module is embedded; the embed pattern or the layout is wrong")
	}
	var embedded []string
	for _, name := range sortedModuleNames(catalog) {
		for _, rel := range catalog[name].Files {
			embedded = append(embedded, name+"/files/"+rel+".tmpl")
		}
	}
	root := filepath.Join("..", "..", "..", "modules")
	var onDisk []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil || info.IsDir() {
			return walkErr
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		rel = filepath.ToSlash(rel)
		if strings.Contains(rel, "/files/") {
			onDisk = append(onDisk, rel)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(embedded)
	sort.Strings(onDisk)
	if strings.Join(embedded, "\n") != strings.Join(onDisk, "\n") {
		t.Fatalf("modules/embed.go does not embed every template.\nembedded:\n%s\non disk:\n%s", strings.Join(embedded, "\n"), strings.Join(onDisk, "\n"))
	}
	// 模板不带具体工程的痕迹：module 路径一律是占位符。
	for _, rel := range embedded {
		body, err := fs.ReadFile(modules.Files, rel)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(body, []byte("example.com/")) {
			t.Errorf("%s names a concrete module path; use {{MODULE}}", rel)
		}
	}
}

func moduleFixtureFS(yaml string, files map[string]string) fstest.MapFS {
	out := fstest.MapFS{"demo/module.yaml": {Data: []byte(yaml)}}
	for rel, body := range files {
		out["demo/files/"+rel+".tmpl"] = &fstest.MapFile{Data: []byte(body)}
	}
	return out
}

const moduleFixtureYAML = `name: demo
summary: fixture
needs: {mods: [nest]}
entity: Player
state:
  - {field: DemoState, type: DemoState, store: demo, dao: "persist,sync"}
component: Demo
errcodes: [demo_bad]
verify: {packages: [./game/handler], run: ^TestDemo, pass: 1}
`

// 清单与模板必须对得上：模板用到的号要声明，声明的号要用到；状态字段名与存储键带模块名。
func TestModuleSpecValidationNamesWhatIsWrong(t *testing.T) {
	good := map[string]string{
		"game/x.go": "package x\n// {{ID:component:Demo}} {{ID:errcode:demo_bad}}\n",
	}
	if _, err := loadModuleCatalogFS(moduleFixtureFS(moduleFixtureYAML, good)); err != nil {
		t.Fatalf("a consistent module: %v", err)
	}
	for name, test := range map[string]struct {
		yaml  string
		files map[string]string
		want  string
	}{
		"undeclared id": {moduleFixtureYAML, map[string]string{"game/x.go": "{{ID:component:Demo}} {{ID:errcode:demo_bad}} {{ID:protocol:DemoPing}}"}, "uses {{ID:protocol:DemoPing}} but module.yaml does not declare it"},
		"unused id":     {moduleFixtureYAML, map[string]string{"game/x.go": "{{ID:component:Demo}}"}, `declares errcode "demo_bad" but no template uses`},
		"field prefix":  {strings.Replace(moduleFixtureYAML, "field: DemoState", "field: State", 1), good, `state field "State" must be PascalCase and start with Demo`},
		"store prefix":  {strings.Replace(moduleFixtureYAML, "store: demo", "store: state", 1), good, `store key "state" must start with demo`},
		"no intent":     {strings.Replace(moduleFixtureYAML, `dao: "persist,sync"`, `dao: "map=fast"`, 1), good, "must state persist / nopersist"},
		"unknown key":   {moduleFixtureYAML + "surprise: 1\n", good, "field surprise not found"},
		"name mismatch": {strings.Replace(moduleFixtureYAML, "name: demo", "name: other", 1), good, "the directory is the name"},
		"no files":      {moduleFixtureYAML, nil, "files/ is empty"},
	} {
		_, err := loadModuleCatalogFS(moduleFixtureFS(test.yaml, test.files))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, test.want)
		}
	}
}

func TestModuleInstallOrderPutsDependenciesFirst(t *testing.T) {
	catalog := map[string]ModuleSpec{
		"a": {Name: "a", Requires: []string{"b", "c"}},
		"b": {Name: "b", Requires: []string{"c"}},
		"c": {Name: "c"},
		"d": {Name: "d", Conflicts: []string{"c"}},
		"x": {Name: "x", Requires: []string{"y"}},
		"y": {Name: "y", Requires: []string{"x"}},
	}
	order, err := moduleInstallOrder(catalog, []string{"a"})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, spec := range order {
		names = append(names, spec.Name)
	}
	if strings.Join(names, ",") != "c,b,a" {
		t.Fatalf("order = %v, want c,b,a", names)
	}
	for request, want := range map[string]string{
		"x":    "dependency cycle",
		"nope": `unknown module "nope"`,
		"a,d":  "cannot be installed together",
	} {
		if _, err := moduleInstallOrder(catalog, strings.Split(request, ",")); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("request %s: err = %v, want %q", request, err, want)
		}
	}
}

func writeDaoDefinition(t *testing.T, body string) string {
	t.Helper()
	root := t.TempDir()
	path := filepath.Join(root, "db", "def", "player.go")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

// 追加总在结构体末尾（同步的 PB 字段号按定义顺序排），保留业务注释；再追加一次没有变化；
// 同名不同型、存储键被占用都拒绝并点名。
func TestAppendDaoFieldsAppendsAtTheEndAndIsIdempotent(t *testing.T) {
	const definition = "package dbdef\n\n//roost:dao coll=player db=main\ntype PlayerDao struct {\n\t// Name 是玩家名。\n\tName string `store:\"name\" dao:\"persist,sync\"`\n\t// 业务留在末尾的说明。\n}\n"
	root := writeDaoDefinition(t, definition)
	fields := []daoFieldSpec{
		{Name: "Wallet", Type: "WalletState", Store: "wallet", Dao: "persist,sync", Comment: "Wallet 是余额。"},
		{Name: "WalletLedger", Type: "WalletLedger", Store: "wallet_ledger", Dao: "persist"},
	}
	source, err := locateDaoStruct(root, "PlayerDao")
	if err != nil || source == nil {
		t.Fatalf("locate: %v %v", source, err)
	}
	updated, err := source.appendFields(root, fields)
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	name, wallet, ledger := strings.Index(text, "Name string"), strings.Index(text, "Wallet WalletState `store:\"wallet\" dao:\"persist,sync\"`"), strings.Index(text, "WalletLedger WalletLedger `store:\"wallet_ledger\" dao:\"persist\"`")
	if name < 0 || wallet < name || ledger < wallet {
		t.Fatalf("fields are not appended in order after the existing ones:\n%s", text)
	}
	for _, kept := range []string{"// Name 是玩家名。", "// 业务留在末尾的说明。", "// Wallet 是余额。"} {
		if !strings.Contains(text, kept) {
			t.Errorf("lost %q:\n%s", kept, text)
		}
	}
	if err := os.WriteFile(source.path, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	again, err := locateDaoStruct(root, "PlayerDao")
	if err != nil {
		t.Fatal(err)
	}
	second, err := again.appendFields(root, fields)
	if err != nil || !bytes.Equal(second, updated) {
		t.Fatalf("a second append changed the file (err %v):\n%s", err, second)
	}
	if _, err := again.appendFields(root, []daoFieldSpec{{Name: "Wallet", Type: "int64", Store: "wallet", Dao: "persist,sync"}}); err == nil || !strings.Contains(err.Error(), "PlayerDao.Wallet already exists as WalletState") {
		t.Fatalf("same name, another type: %v", err)
	}
	if _, err := again.appendFields(root, []daoFieldSpec{{Name: "Purse", Type: "PurseState", Store: "wallet", Dao: "persist"}}); err == nil || !strings.Contains(err.Error(), `store key "wallet" is already used by PlayerDao.Wallet`) {
		t.Fatalf("store key taken: %v", err)
	}
	if missing, err := locateDaoStruct(root, "GuildDao"); err != nil || missing != nil {
		t.Fatalf("a DAO that is not defined: %v %v", missing, err)
	}
}

// 一次计划里的两个模块不会拿到同一个号；工程里已用的号、已装模块记录里的号都跳过。
func TestModuleIDAllocatorNeverHandsOutAUsedID(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "protocol", "def"), 0o755); err != nil {
		t.Fatal(err)
	}
	used := "//go:build protocoldef\n\npackage protocoldef\n\n//roost:protocol group=game handler=x\ntype XProtocol interface {\n\t//roost:msg id=10000\n\tX(XRequest) XResponse\n}\n"
	if err := os.WriteFile(filepath.Join(root, "protocol", "def", "x.go"), []byte(used), 0o644); err != nil {
		t.Fatal(err)
	}
	manifest := DefaultManifest("demo", "example.com/demo", nil, nil, nil)
	manifest.Modules = map[string]InstalledModule{"old": {IDs: map[string]map[string]int64{"protocol": {"OldPing": 10001}, "errcode": {"old_bad": 100000}}}}
	allocator, err := newModuleIDAllocator(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	first, err := allocator.allocate(ModuleSpec{Name: "a", Component: "A", Protocols: []string{"APing", "APong"}, Errcodes: []string{"a_bad"}})
	if err != nil {
		t.Fatal(err)
	}
	second, err := allocator.allocate(ModuleSpec{Name: "b", Component: "B", Protocols: []string{"BPing"}, Errcodes: []string{"b_bad"}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]int64{
		"a.APing": first["protocol"]["APing"], "a.APong": first["protocol"]["APong"], "b.BPing": second["protocol"]["BPing"],
		"a.a_bad": first["errcode"]["a_bad"], "b.b_bad": second["errcode"]["b_bad"],
		"a.A": first["component"]["A"], "b.B": second["component"]["B"],
	}
	want := map[string]int64{"a.APing": 10002, "a.APong": 10003, "b.BPing": 10004, "a.a_bad": 100001, "b.b_bad": 100002, "a.A": 2000, "b.B": 2001}
	for key, id := range want {
		if got[key] != id {
			t.Errorf("%s = %d, want %d (all: %v)", key, got[key], id, got)
		}
	}
}

// moduleProject 建一个 -template game 的工程，依赖指向本仓库工作树。
func moduleProject(t *testing.T, name string) (root string, roost func(args ...string) string, try func(args ...string) (string, error)) {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root = filepath.Join(t.TempDir(), name)
	if _, _, err := NewProject(NewOptions{Name: name, Module: "example.com/studio/" + name, Out: root, Template: "game"}); err != nil {
		t.Fatal(err)
	}
	modPath := filepath.Join(root, "go.mod")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	mod = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(mod, nil)
	mod = append(mod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(modPath, mod, 0o644); err != nil {
		t.Fatal(err)
	}
	try = func(args ...string) (string, error) {
		var out bytes.Buffer
		err := Run(append(args, "--root", root), &out, &out)
		return out.String(), err
	}
	roost = func(args ...string) string {
		t.Helper()
		out, err := try(args...)
		if err != nil {
			t.Fatalf("roost %s: %v\n%s", strings.Join(args, " "), err, out)
		}
		return out
	}
	return root, roost, try
}

// moduleProjectDigest 是工程里全部文件（不含 .git）的内容摘要：用来断言一条命令没有改动任何东西。
func moduleProjectDigest(t *testing.T, root string) string {
	t.Helper()
	digest := sha256.New()
	err := filepath.Walk(root, func(path string, info os.FileInfo, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if info.IsDir() {
			if info.Name() == ".git" {
				return filepath.SkipDir
			}
			return nil
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		digest.Write([]byte(filepath.ToSlash(rel)))
		digest.Write(body)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(digest.Sum(nil))
}

// 在生成工程上真实安装两个模块：依赖先装、号不相撞、重复安装无变化、模块自带场景与工程级检查全部通过。
// 这是第 2 期的验收（docs/maintenance/FEATURE-2026-11-10-business-modules.md 第 11.12 节）。
func TestModulesInstallOntoAGeneratedProjectAndVerify(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, try := moduleProject(t, "quest")

	// plan 不改动工程，且给出的号就是 add 之后工程里的号。
	before := moduleProjectDigest(t, root)
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "checkin", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("roost module plan changed the project")
	}
	if plan.Applied || len(plan.Modules) != 2 || plan.Modules[0].Name != "wallet" || plan.Modules[1].Name != "checkin" {
		t.Fatalf("plan = %+v, want wallet (the dependency) then checkin, not applied", plan)
	}
	if strings.Join(plan.Prerequisites, "; ") != "roost add dao Player --entity Player; roost add access player --service game" {
		t.Fatalf("prerequisites = %v", plan.Prerequisites)
	}
	seen := map[string]map[int64]string{}
	for _, item := range plan.Modules {
		if item.Status != moduleStatusInstall {
			t.Fatalf("module %s status = %s", item.Name, item.Status)
		}
		for kind, names := range item.IDs {
			for name, id := range names {
				if other, taken := seen[kind][id]; taken {
					t.Fatalf("%s id %d was planned for both %s and %s", kind, id, other, name)
				}
				if seen[kind] == nil {
					seen[kind] = map[int64]string{}
				}
				seen[kind][id] = name
			}
		}
	}

	roost("module", "add", "checkin")
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range plan.Modules {
		installed, ok := manifest.Modules[item.Name]
		if !ok {
			t.Fatalf("roost.yaml has no record of %s: %+v", item.Name, manifest.Modules)
		}
		planned, _ := json.Marshal(item.IDs)
		recorded, _ := json.Marshal(installed.IDs)
		if string(planned) != string(recorded) {
			t.Fatalf("%s: planned ids %s, recorded ids %s", item.Name, planned, recorded)
		}
	}
	definition, err := os.ReadFile(filepath.Join(root, "db", "def", "player.go"))
	if err != nil {
		t.Fatal(err)
	}
	wallet, ledger, checkin := bytes.Index(definition, []byte("Wallet WalletState")), bytes.Index(definition, []byte("WalletLedger WalletLedger")), bytes.Index(definition, []byte("Checkin CheckinState"))
	if wallet < 0 || ledger < wallet || checkin < ledger {
		t.Fatalf("state fields are not appended in install order:\n%s", definition)
	}

	// 重复安装：没有任何文件变化。
	installedDigest := moduleProjectDigest(t, root)
	if out := roost("module", "add", "checkin", "wallet"); !strings.Contains(out, "already installed") {
		t.Fatalf("second add: %s", out)
	}
	if moduleProjectDigest(t, root) != installedDigest {
		t.Fatal("installing installed modules again changed the project")
	}

	// 模块自带场景恰好是清单里的数，工程级检查通过。
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("verify: %v\n%+v", err, report)
	}
	passed := map[string]int{}
	for _, result := range report.Modules {
		passed[result.Name] = result.Passed
	}
	if passed["wallet"] != 7 || passed["checkin"] != 5 {
		t.Fatalf("passed scenarios = %v, want wallet 7 and checkin 5", passed)
	}
	if moduleProjectDigest(t, root) != installedDigest {
		t.Fatal("roost module verify changed the project")
	}

	// 业务改过模块的文件之后，安装器仍然不动它。
	edited := filepath.Join(root, "game", "handler", "wallet_grant.go")
	body, err := os.ReadFile(edited)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(edited, append(body, []byte("\n// 业务加的一行。\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	editedDigest := moduleProjectDigest(t, root)
	roost("module", "add", "wallet")
	if moduleProjectDigest(t, root) != editedDigest {
		t.Fatal("the installer touched a file the project had edited")
	}
	_ = try
}

// 不是全新工程也能装：已有业务字段和组件的工程上，模块的字段追加在业务字段之后；
// 模板的落点被业务文件占着时拒绝，工程不变；生成失败时模块写过的东西全部恢复。
func TestModulesInstallOntoAProjectWithBusinessCodeAndRollBackOnFailure(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, try := moduleProject(t, "saga")
	// 前置骨架先由业务自己建好：下面的失败用例要失败在模块写入之后，而不是前置命令里。
	roost("add", "dao", "Player", "--entity", "Player")
	roost("add", "access", "player", "--service", "game")
	roost("add", "component", "Profile", "--entity", "Player")
	definition := filepath.Join(root, "db", "def", "player.go")
	body, err := os.ReadFile(definition)
	if err != nil {
		t.Fatal(err)
	}
	closing := bytes.LastIndexByte(body, '}')
	body = append(append(append([]byte(nil), body[:closing]...), []byte("\tName string `store:\"name\" dao:\"persist,sync\"`\n")...), body[closing:]...)
	if err := os.WriteFile(definition, body, 0o644); err != nil {
		t.Fatal(err)
	}
	roost("generate")

	// 落点被业务文件占着：拒绝，工程不变。
	occupied := filepath.Join(root, "db", "def", "wallet.go")
	if err := os.WriteFile(occupied, []byte("package dbdef\n\n// 业务自己的货币定义。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if out, err := try("module", "add", "wallet"); err == nil || !strings.Contains(err.Error(), "db/def/wallet.go already exists and is not this module's file") {
		t.Fatalf("occupied target: err = %v\n%s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("a refused install changed the project")
	}
	if err := os.Remove(occupied); err != nil {
		t.Fatal(err)
	}

	// 生成失败（工程里有一个写坏的 Nest handler）：模块写过的业务文件与 roost.yaml 全部恢复。
	broken := filepath.Join(root, "game", "handler", "zz_broken.go")
	if err := os.WriteFile(broken, []byte("package handler\n\n//roost:nest rollback=undo durability=strict\nfunc handlerBroken() error { return nil }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before = moduleProjectDigest(t, root)
	if out, err := try("module", "add", "wallet"); err == nil || !strings.Contains(err.Error(), "generate after installing modules") {
		t.Fatalf("install with a broken handler in the project: err = %v\n%s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("a failed install left changes behind")
	}
	if err := os.Remove(broken); err != nil {
		t.Fatal(err)
	}

	// 去掉障碍后安装成功，模块字段在业务字段之后，业务的组件还在。
	roost("module", "add", "wallet")
	body, err = os.ReadFile(definition)
	if err != nil {
		t.Fatal(err)
	}
	if name, wallet := bytes.Index(body, []byte("Name string")), bytes.Index(body, []byte("Wallet WalletState")); name < 0 || wallet < name {
		t.Fatalf("the module's fields are not after the project's own:\n%s", body)
	}
	entityFile, err := os.ReadFile(filepath.Join(root, "game", "entities", "player", "entity.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(entityFile, []byte("ProfileComponent")) || !bytes.Contains(entityFile, []byte("WalletComponent")) {
		t.Fatalf("entity lost a component:\n%s", entityFile)
	}
	if report, err := VerifyModules(context.Background(), root, []string{"wallet"}, nil); err != nil {
		t.Fatalf("verify: %v\n%+v", err, report)
	}
}
