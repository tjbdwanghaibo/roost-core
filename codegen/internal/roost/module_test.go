package roost

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"go/format"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
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
		// next（安装后提示）：每条是非空的一行、不带 {{ID:...}}，条数与长度有上限；写错键名照样按未知键拒绝。
		"empty hint":     {moduleFixtureYAML + "next: [\"roost config enable player-tcp\", \"  \"]\n", good, "next[1] is empty"},
		"multiline hint": {moduleFixtureYAML + "next: [\"first line\\nsecond line\"]\n", good, "next[0] spans several lines"},
		"hint with id":   {moduleFixtureYAML + "next: [\"code {{ID:errcode:demo_bad}}\"]\n", good, "next[0] uses an {{ID:...}} placeholder"},
		"long hint":      {moduleFixtureYAML + "next: [\"" + strings.Repeat("长", 241) + "\"]\n", good, "next[0] is 241 characters; at most 240"},
		"too many hints": {moduleFixtureYAML + "next: [a, b, c, d, e, f, g, h, i]\n", good, "next lists 9 hints; at most 8"},
		"misspelt next":  {moduleFixtureYAML + "nexts: [a]\n", good, "field nexts not found"},
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
	if passed["wallet"] != 14 || passed["checkin"] != 8 {
		t.Fatalf("passed scenarios = %v, want wallet 14 and checkin 8", passed)
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

// tenModuleStructsTest 写进生成工程的 db 包：主 DAO 下挂十个模块形状的嵌套结构体时，
// 改其中一个的子字段，同步补丁与持久补丁都只带这一处。
const tenModuleStructsTest = `package db

import (
	"bytes"
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type tenCommitter struct{ records []nest.CommitRecord }

func (c *tenCommitter) Commit(_ context.Context, record nest.CommitRecord) error {
	c.records = append(c.records, record)
	return nil
}

func tenTx(t *testing.T, committer *tenCommitter, fn func() error) error {
	t.Helper()
	_, err := nest.RunIsolatedTransaction(context.Background(), committer, "ten", func() (any, error) { return nil, fn() })
	return err
}

// seededPlayer 每个结构体都有内容：一个标量、一百个 map 元素、十个嵌套对象。
func seededPlayer(t *testing.T) *PlayerDao {
	t.Helper()
	source := NewPlayerDao()
	source.SetId(1001)
	if err := tenTx(t, &tenCommitter{}, func() error {
		fill := func(setLevel func(int32), setItem func(int32, int64), setSlot func(int32, *ModSlot)) {
			setLevel(1)
			for key := int32(0); key < 100; key++ {
				setItem(key, int64(key))
			}
			for key := int32(0); key < 10; key++ {
				slot := &ModSlot{}
				slot.SetPower(int64(key))
				setSlot(key, slot)
			}
		}
		fill(source.GetMod0().SetLevel, source.GetMod0().SetItemsKey, source.GetMod0().SetSlotsKey)
		fill(source.GetMod1().SetLevel, source.GetMod1().SetItemsKey, source.GetMod1().SetSlotsKey)
		fill(source.GetMod2().SetLevel, source.GetMod2().SetItemsKey, source.GetMod2().SetSlotsKey)
		fill(source.GetMod3().SetLevel, source.GetMod3().SetItemsKey, source.GetMod3().SetSlotsKey)
		fill(source.GetMod4().SetLevel, source.GetMod4().SetItemsKey, source.GetMod4().SetSlotsKey)
		fill(source.GetMod5().SetLevel, source.GetMod5().SetItemsKey, source.GetMod5().SetSlotsKey)
		fill(source.GetMod6().SetLevel, source.GetMod6().SetItemsKey, source.GetMod6().SetSlotsKey)
		fill(source.GetMod7().SetLevel, source.GetMod7().SetItemsKey, source.GetMod7().SetSlotsKey)
		fill(source.GetMod8().SetLevel, source.GetMod8().SetItemsKey, source.GetMod8().SetSlotsKey)
		fill(source.GetMod9().SetLevel, source.GetMod9().SetItemsKey, source.GetMod9().SetSlotsKey)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// 从持久字节重建一份：变化记录是干净的，之后的补丁只反映用例里的改动。
	player := NewPlayerDao()
	player.SetId(1001)
	if err := player.Unmarshal(source.Marshal()); err != nil {
		t.Fatal(err)
	}
	player.DirtyTracker().SetVersion(1)
	return player
}

func TestTenModuleStructsSyncAndPersistOnlyWhatChanged(t *testing.T) {
	player := seededPlayer(t)
	full := player.MarshalSync(^uint64(0))
	committer := &tenCommitter{}
	if err := tenTx(t, committer, func() error {
		player.GetMod2().SetLevel(9)       // 同步的结构体：标量
		player.GetMod2().SetItemsKey(7, 70) // 同步的结构体：一个 map 元素
		slot, _ := player.GetMod4().GetSlots(3)
		slot.SetPower(99)                // 同步的结构体：嵌套对象的子字段
		player.GetMod5().SetLevel(9)     // 只落库的结构体
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	// 同步补丁：只有 Mod2 与 Mod4，且比全量小一个数量级以上。
	delta := player.MarshalSyncChanges(player.DirtyTracker().SyncDirtyMask())
	patch, err := UnmarshalSyncPlayerDao(delta)
	if err != nil {
		t.Fatal(err)
	}
	if patch.Mod2 == nil || patch.Mod4 == nil {
		t.Fatalf("the changed structs are missing from the sync patch: %+v", patch)
	}
	if patch.Mod0 != nil || patch.Mod6 != nil || patch.Mod8 != nil {
		t.Fatalf("an untouched struct is in the sync patch: %+v", patch)
	}
	if patch.Mod2.Patch == nil || patch.Mod4.Patch == nil {
		t.Fatal("a sub-field change was sent as a whole-struct replacement")
	}
	if len(delta)*10 > len(full) {
		t.Fatalf("delta is %d bytes, full is %d: the delta is not incremental", len(delta), len(full))
	}
	mirror := NewPlayerDao()
	if err := mirror.ApplySync(full); err != nil {
		t.Fatal(err)
	}
	if err := mirror.ApplySync(delta); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(mirror.MarshalSync(^uint64(0)), player.MarshalSync(^uint64(0))) {
		t.Fatal("full + delta does not rebuild the current state")
	}

	// 持久补丁：按路径，只有改过的四处；只落库的 Mod5 在这里、不在同步补丁里。
	if len(committer.records) != 1 || len(committer.records[0].Mutations) != 1 {
		t.Fatalf("records: %+v", committer.records)
	}
	mutation := committer.records[0].Mutations[0]
	if mutation.Kind != dataengine.MutationPatch {
		t.Fatalf("mutation kind = %v, want a patch", mutation.Kind)
	}
	elements, err := bson.Raw(mutation.Patch.SetBSON).Elements()
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, element := range elements {
		keys = append(keys, element.Key())
	}
	slices.Sort(keys)
	if want := []string{"mod2.1", "mod2.2.7", "mod4.3.3.1", "mod5.1"}; !slices.Equal(keys, want) {
		t.Fatalf("persisted paths = %v, want %v", keys, want)
	}
	t.Logf("sync full %d bytes, delta %d bytes; persist patch %d bytes for %d paths", len(full), len(delta), len(mutation.Patch.SetBSON), len(keys))

	// 失败的事务：值回到原样，提交器没有收到记录。
	refused := errors.New("refused")
	if err := tenTx(t, committer, func() error { player.GetMod6().SetLevel(5); player.GetMod7().SetItemsKey(1, 555); return refused }); !errors.Is(err, refused) {
		t.Fatalf("err = %v", err)
	}
	item, _ := player.GetMod7().GetItems(1)
	if player.GetMod6().GetLevel() != 1 || item != 1 || len(committer.records) != 1 {
		t.Fatalf("a refused transaction left level=%d item=%d records=%d", player.GetMod6().GetLevel(), item, len(committer.records))
	}
}
`

// 主 DAO 下挂十个模块形状的嵌套结构体（五个同步、五个只落库，各含标量、map、嵌套对象）时，
// 增量同步与按路径落库照常工作：改一个子字段，两种补丁都只带这一处。这是“模块状态用嵌套结构体”
// 这条决定在规模上的验证（方案第 11.10 节）。
func TestTenModuleStructsUnderOneDAO(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go test in it")
	}
	t.Parallel()
	root, roost, _ := moduleProject(t, "tenmods")
	roost("add", "dao", "Player", "--entity", "Player")
	var types strings.Builder
	types.WriteString("package dbdef\n\n// ModSlot 是各结构体共用的嵌套对象。\ntype ModSlot struct {\n\tPower int64 `store:\"1\"`\n}\n")
	var fields []daoFieldSpec
	for i := 0; i < 10; i++ {
		name := "Mod" + strconv.Itoa(i)
		types.WriteString("\ntype " + name + "State struct {\n\tLevel int32 `store:\"1\"`\n\tItems map[int32]int64 `store:\"2\"`\n\tSlots map[int32]*ModSlot `store:\"3\"`\n}\n")
		intent := "persist,sync"
		if i%2 == 1 {
			intent = "persist"
		}
		fields = append(fields, daoFieldSpec{Name: name, Type: name + "State", Store: strings.ToLower(name), Dao: intent})
	}
	if err := os.WriteFile(filepath.Join(root, "db", "def", "tenmods.go"), []byte(types.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	source, err := locateDaoStruct(root, "PlayerDao")
	if err != nil || source == nil {
		t.Fatalf("locate PlayerDao: %v %v", source, err)
	}
	updated, err := source.appendFields(root, fields)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source.path, updated, 0o644); err != nil {
		t.Fatal(err)
	}
	roost("generate")
	if err := os.WriteFile(filepath.Join(root, "db", "tenmods_test.go"), []byte(tenModuleStructsTest), 0o644); err != nil {
		t.Fatal(err)
	}
	output, err := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-race", "-count=1", "-v", "-run", "^TestTenModuleStructs", "./db")
	if err != nil || !bytes.Contains(output, []byte("--- PASS: TestTenModuleStructsSyncAndPersistOnlyWhatChanged")) {
		t.Fatalf("ten module structs: %v\n%s", err, output)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, "sync full") {
			t.Log(strings.TrimSpace(line))
		}
	}
}

// 模块可以接管一个生成桩（托管服务默认拒绝一切的 collaborators.go），但只在它没被改过的时候：
// 没改过就替换，业务改过一个字符就拒绝安装、工程不变。
func TestModuleReplacesOnlyAnUntouchedGeneratedStub(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, try := moduleProject(t, "parlor")
	stub := filepath.Join(root, "internal", "service", "chat", "collaborators.go")
	original, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(original, []byte("is not configured")) {
		t.Fatalf("the generated chat collaborators are not the refusing stub:\n%s", original)
	}

	// 业务改过：拒绝，工程不变。
	if err := os.WriteFile(stub, append(append([]byte(nil), original...), []byte("\n// 业务加的一行。\n")...), 0o644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if out, err := try("module", "add", "chat"); err == nil || !strings.Contains(err.Error(), "internal/service/chat/collaborators.go has been edited since it was generated") {
		t.Fatalf("an edited stub: err = %v\n%s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("a refused install changed the project")
	}

	// 没改过：plan 写明会替换，add 之后是模块的实现，doctor 不再把它当拒绝桩。
	if err := os.WriteFile(stub, original, 0o644); err != nil {
		t.Fatal(err)
	}
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "chat", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Modules) != 1 || strings.Join(plan.Modules[0].Replaces, ",") != "internal/service/chat/collaborators.go" {
		t.Fatalf("plan does not say which stub it replaces: %+v", plan.Modules)
	}
	roost("module", "add", "chat")
	replaced, err := os.ReadFile(stub)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(replaced, []byte(collaboratorUnconfiguredMarker)) || !bytes.Contains(replaced, []byte("gamechat.Allow")) {
		t.Fatalf("the stub was not replaced by the module's collaborators:\n%s", replaced)
	}
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil || len(report.Modules) != 1 || report.Modules[0].Passed != 10 {
		t.Fatalf("verify: %v\n%+v", err, report)
	}
	// 再装一次没有变化：现在这个文件是模块自己的。
	installed := moduleProjectDigest(t, root)
	roost("module", "add", "chat")
	if moduleProjectDigest(t, root) != installed {
		t.Fatal("installing chat again changed the project")
	}
}

// login 模块接管两种生成桩：玩家 TCP 的鉴权桩与 account 的 collaborators。两个都只在没改过时替换，
// 任何一个被业务改过就拒绝安装、工程不变；鉴权桩还没生成（工程没有玩家 TCP 接入）时由前置命令先生成再替换，
// plan 照样写明。替换之后 doctor 不再把它们当拒绝桩，config enable player-tcp 放行。
func TestModuleLoginReplacesTheAuthenticatorAndAccountStubs(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, try := moduleProject(t, "gatehouse")
	const authRel, collaboratorsRel = "internal/access/player/tcp/auth.go", "internal/service/account/collaborators.go"
	authPath := filepath.Join(root, filepath.FromSlash(authRel))
	collaboratorsPath := filepath.Join(root, filepath.FromSlash(collaboratorsRel))
	planned := func() ModulePlan {
		t.Helper()
		var plan ModulePlan
		if err := json.Unmarshal([]byte(roost("module", "plan", "login", "--json")), &plan); err != nil {
			t.Fatal(err)
		}
		if len(plan.Modules) != 1 {
			t.Fatalf("plan: %+v", plan.Modules)
		}
		return plan
	}

	// 全新工程还没有玩家 TCP 接入：鉴权桩不存在，前置命令会生成它。plan 列出两个要替换的桩。
	if _, err := os.Stat(authPath); !os.IsNotExist(err) {
		t.Fatalf("a new game project already has %s: %v", authRel, err)
	}
	plan := planned()
	if !contains(plan.Prerequisites, "roost add transport tcp") {
		t.Fatalf("plan does not add the TCP transport first: %v", plan.Prerequisites)
	}
	if got := strings.Join(plan.Modules[0].Replaces, ","); got != authRel+","+collaboratorsRel {
		t.Fatalf("plan replaces %q, want the authenticator stub (to be generated) and account's collaborators", got)
	}

	// 接入已经在、桩已经落地的工程：plan 仍然列出两个。
	roost("add", "access", "player", "--service", "game")
	roost("add", "transport", "tcp")
	authStub, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if playerTCPAuthenticatorImplemented(authStub) {
		t.Fatalf("the generated authenticator is not the refusing stub:\n%s", authStub)
	}
	collaboratorsStub, err := os.ReadFile(collaboratorsPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(collaboratorsStub, []byte(collaboratorUnconfiguredMarker)) {
		t.Fatalf("the generated account collaborators are not the refusing stub:\n%s", collaboratorsStub)
	}
	if got := strings.Join(planned().Modules[0].Replaces, ","); got != authRel+","+collaboratorsRel {
		t.Fatalf("plan replaces %q on a project that has both stubs", got)
	}

	// 接入已经声明、鉴权文件却不在（被删掉）的工程：没有桩可替换，也没有前置命令会再生成它，
	// plan 只把它当普通的新文件，不说“替换”。
	if err := os.Remove(authPath); err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(planned().Modules[0].Replaces, ","); got != collaboratorsRel {
		t.Fatalf("plan replaces %q although %s does not exist and no prerequisite creates it", got, authRel)
	}
	if err := os.WriteFile(authPath, authStub, 0o644); err != nil {
		t.Fatal(err)
	}

	// 业务改过其中任何一个：拒绝，点名是哪个文件，工程不变。
	edits := []struct {
		rel, path string
		original  []byte
	}{
		{authRel, authPath, authStub},
		{collaboratorsRel, collaboratorsPath, collaboratorsStub},
	}
	for _, edit := range edits {
		if err := os.WriteFile(edit.path, append(append([]byte(nil), edit.original...), []byte("\n// 业务加的一行。\n")...), 0o644); err != nil {
			t.Fatal(err)
		}
		before := moduleProjectDigest(t, root)
		if out, err := try("module", "add", "login"); err == nil || !strings.Contains(err.Error(), edit.rel+" has been edited since it was generated") {
			t.Fatalf("an edited %s: err = %v\n%s", edit.rel, err, out)
		}
		if moduleProjectDigest(t, root) != before {
			t.Fatalf("a refused install (edited %s) changed the project", edit.rel)
		}
		if err := os.WriteFile(edit.path, edit.original, 0o644); err != nil {
			t.Fatal(err)
		}
	}

	// 都没改过：替换成模块的实现。
	roost("module", "add", "login")
	auth, err := os.ReadFile(authPath)
	if err != nil {
		t.Fatal(err)
	}
	if !playerTCPAuthenticatorImplemented(auth) || !bytes.Contains(auth, []byte("gamelogin.Authenticator")) {
		t.Fatalf("the authenticator stub was not replaced by the module's authenticator:\n%s", auth)
	}
	collaborators, err := os.ReadFile(collaboratorsPath)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(collaborators, []byte(collaboratorUnconfiguredMarker)) || !bytes.Contains(collaborators, []byte("identityVerifier")) {
		t.Fatalf("account's collaborators stub was not replaced by the module's:\n%s", collaborators)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range checkFrameworkCollaborators(root, manifest) {
		if item.Name == "collaborators:account" && item.Status != StatusOK {
			t.Fatalf("doctor still reports account's collaborators: %+v", item)
		}
	}
	// 鉴权不再是拒绝骨架，所以开监听的命令放行，player-tcp 流程全部 OK。
	roost("config", "enable", "player-tcp")
	workflow, err := checkPlayerTCPWorkflow(root, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range workflow {
		if item.Status != StatusOK {
			t.Fatalf("player-tcp workflow after installing login and enabling the listener: %+v", item)
		}
	}
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil || len(report.Modules) != 1 || report.Modules[0].Passed != 22 {
		t.Fatalf("verify: %v\n%+v", err, report)
	}
	// 再装一次没有变化：这两个文件现在是模块自己的。
	installed := moduleProjectDigest(t, root)
	roost("module", "add", "login")
	if moduleProjectDigest(t, root) != installed {
		t.Fatal("installing login again changed the project")
	}
}

// 安装器认识的生成桩是一张表（generatedStubKinds）：托管服务的 collaborators.go 与玩家 TCP 的鉴权桩。
// 每一种都只在逐字节等于生成器现在写出的内容（格式化前后都算）时才是“没改过的桩”；
// 表里没有的路径不算桩，表里的路径改过一个字符也不算。
func TestModuleStubKindsRecogniseOnlyUntouchedGeneratedStubs(t *testing.T) {
	manifest := gameTemplateManifest(t)
	formatted := func(source string) []byte {
		t.Helper()
		out, err := format.Source([]byte(source))
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	stubs := map[string]string{
		"internal/service/chat/collaborators.go":    renderFrameworkCollaborators(manifest, "chat"),
		"internal/service/account/collaborators.go": renderFrameworkCollaborators(manifest, "account"),
		"internal/access/player/tcp/auth.go":        renderPlayerTCPAuthenticator(),
	}
	for rel, stub := range stubs {
		for name, body := range map[string][]byte{"as rendered": []byte(stub), "as formatted": formatted(stub)} {
			pristine, err := isUntouchedGeneratedStub(manifest, rel, body)
			if err != nil || !pristine {
				t.Errorf("%s %s: pristine = %v, err = %v; want an untouched stub", rel, name, pristine, err)
			}
		}
		edited := append(formatted(stub), []byte("\n// 业务加的一行。\n")...)
		if pristine, err := isUntouchedGeneratedStub(manifest, rel, edited); err != nil || pristine {
			t.Errorf("%s with one added line: pristine = %v, err = %v; want business code", rel, pristine, err)
		}
	}
	// 鉴权桩的内容放在别的路径上不算桩；业务服务（不是托管服务）的 collaborators.go 也不算。
	authStub := formatted(renderPlayerTCPAuthenticator())
	for _, rel := range []string{
		"internal/access/player/ws/auth.go",
		"internal/access/player/tcp/auth_extra.go",
		"internal/service/game/collaborators.go",
		"game/handler/auth.go",
	} {
		if pristine, err := isUntouchedGeneratedStub(manifest, rel, authStub); err != nil || pristine {
			t.Errorf("%s: pristine = %v, err = %v; want not a stub", rel, pristine, err)
		}
	}
	// 一种桩的内容不能冒充另一种。
	if pristine, _ := isUntouchedGeneratedStub(manifest, "internal/service/chat/collaborators.go", authStub); pristine {
		t.Error("the authenticator stub passed as chat's collaborators stub")
	}
	if pristine, _ := isUntouchedGeneratedStub(manifest, "internal/access/player/tcp/auth.go", formatted(stubs["internal/service/chat/collaborators.go"])); pristine {
		t.Error("chat's collaborators stub passed as the authenticator stub")
	}
	// “现在还没有、但待执行的前置命令会生成”的桩只有鉴权桩一种，并且只在那条命令确实待执行时才算。
	pending := map[string]bool{"roost add transport tcp": true}
	if !stubComesFromPrerequisite(manifest, "internal/access/player/tcp/auth.go", pending) {
		t.Error("the authenticator stub is not expected from the pending roost add transport tcp")
	}
	if stubComesFromPrerequisite(manifest, "internal/access/player/tcp/auth.go", map[string]bool{"roost add access player --service game": true}) {
		t.Error("the authenticator stub is expected although roost add transport tcp is not pending")
	}
	if stubComesFromPrerequisite(manifest, "internal/service/chat/collaborators.go", pending) || stubComesFromPrerequisite(manifest, "game/handler/auth.go", pending) {
		t.Error("a file no prerequisite creates is expected from one")
	}
	// 生成的鉴权桩确实是 doctor 说的“还是拒绝骨架”，否则接管它就没有意义。
	if playerTCPAuthenticatorImplemented(authStub) {
		t.Error("doctor takes the generated authenticator stub for an implemented authenticator")
	}
}

// 联动文件（with/<另一个模块>/）的清单规则：目录与 with 段一一对应；场景名与模块自己的分开；
// 只能用模块已声明的号；整个目录里没有两处写同一个落点。
func TestModuleWithValidationNamesWhatIsWrong(t *testing.T) {
	files := map[string]string{"game/x.go": "package x\n// {{ID:component:Demo}} {{ID:errcode:demo_bad}}\n"}
	const otherYAML = "name: other\nsummary: fixture\nverify: {packages: [./game/other], run: ^TestOther, pass: 1}\n"
	const withYAML = "with:\n  other:\n    summary: glue\n    verify: {packages: [./game/handler], run: ^TestWithDemoOther, pass: 1}\n"
	build := func(yaml string, glue map[string]string, otherFiles map[string]string) fstest.MapFS {
		out := moduleFixtureFS(yaml, files)
		out["other/module.yaml"] = &fstest.MapFile{Data: []byte(otherYAML)}
		for rel, body := range otherFiles {
			out["other/files/"+rel+".tmpl"] = &fstest.MapFile{Data: []byte(body)}
		}
		for rel, body := range glue {
			out["demo/with/other/"+rel+".tmpl"] = &fstest.MapFile{Data: []byte(body)}
		}
		return out
	}
	otherFiles := map[string]string{"game/other/other.go": "package other\n"}
	glue := map[string]string{"game/x_other.go": "package x\n// {{ID:errcode:demo_bad}}\n"}
	catalog, err := loadModuleCatalogFS(build(moduleFixtureYAML+withYAML, glue, otherFiles))
	if err != nil {
		t.Fatalf("a consistent module with glue: %v", err)
	}
	if got := catalog["demo"].With["other"].Files; len(got) != 1 || got[0] != "game/x_other.go" {
		t.Fatalf("glue files = %v", got)
	}
	// 联动文件的内容算进摘要：它变了，已装的模块显示为另一版。
	changed, err := loadModuleCatalogFS(build(moduleFixtureYAML+withYAML, map[string]string{"game/x_other.go": "package x\n// changed {{ID:errcode:demo_bad}}\n"}, otherFiles))
	if err != nil || changed["demo"].Digest == catalog["demo"].Digest {
		t.Fatalf("a changed glue file left the digest alone (err=%v)", err)
	}
	for name, test := range map[string]struct {
		yaml       string
		glue       map[string]string
		otherFiles map[string]string
		want       string
	}{
		"directory without entry": {moduleFixtureYAML, glue, otherFiles, "with/other exists but module.yaml has no with.other entry"},
		"entry without files":     {moduleFixtureYAML + withYAML, nil, otherFiles, "with.other: with/other/ has no .tmpl file"},
		"unknown module":          {moduleFixtureYAML + strings.Replace(withYAML, "  other:", "  ghost:", 1), nil, otherFiles, "with.ghost"},
		"scenario name overlaps":  {moduleFixtureYAML + strings.Replace(withYAML, "^TestWithDemoOther", "^TestDemoOther", 1), glue, otherFiles, `must start with ^TestWithDemo`},
		"no summary":              {moduleFixtureYAML + strings.Replace(withYAML, "summary: glue", "summary: ''", 1), glue, otherFiles, "with.other: summary is required"},
		"undeclared id":           {moduleFixtureYAML + withYAML, map[string]string{"game/x_other.go": "{{ID:protocol:DemoPing}}"}, otherFiles, "with/other/game/x_other.go uses {{ID:protocol:DemoPing}} but module.yaml does not declare it"},
		"same path as own file":   {moduleFixtureYAML + withYAML, map[string]string{"game/x.go": "package x\n"}, otherFiles, "both ship game/x.go"},
		"same path as other's":    {moduleFixtureYAML + withYAML, map[string]string{"game/other/other.go": "package other\n"}, otherFiles, "both ship game/other/other.go"},
	} {
		_, err := loadModuleCatalogFS(build(test.yaml, test.glue, test.otherFiles))
		if err == nil || !strings.Contains(err.Error(), test.want) {
			t.Errorf("%s: err = %v, want it to contain %q", name, err, test.want)
		}
	}
}

// 一对模块的联动文件在第二个装上的那一次写进工程，不论先装哪个；只装一个时没有；
// 落点被业务文件占着就拒绝、工程不变；写过之后重复安装没有变化；联动自带的场景单独计数。
func TestModuleGlueIsWrittenWhenTheSecondModuleArrives(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, try := moduleProject(t, "lobby")
	glue := filepath.Join(root, "game", "chat", "chat_enter.go")

	// 只装 chat：没有联动文件，计划里也没有。
	out := roost("module", "add", "chat")
	if strings.Contains(out, "glue ") {
		t.Fatalf("chat alone wrote glue:\n%s", out)
	}
	if _, err := os.Stat(glue); !os.IsNotExist(err) {
		t.Fatalf("chat alone has the enter glue (err=%v)", err)
	}

	// 落点被业务文件占着：拒绝安装 enter，工程不变。
	if err := os.WriteFile(glue, []byte("package chat\n\n// 业务自己的文件。\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	before := moduleProjectDigest(t, root)
	if out, err := try("module", "add", "enter"); err == nil || !strings.Contains(err.Error(), "module chat with enter: game/chat/chat_enter.go already exists and is not this glue file") {
		t.Fatalf("an occupied glue path: err = %v\n%s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("a refused install changed the project")
	}
	if err := os.Remove(glue); err != nil {
		t.Fatal(err)
	}

	// 装上 enter：plan 写明会写哪一组联动，add 之后文件在、归属记在 chat 名下。
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "enter", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Glue) != 1 || plan.Glue[0].Module != "chat" || plan.Glue[0].With != "enter" || !contains(plan.Glue[0].Files, "game/chat/chat_enter.go") {
		t.Fatalf("plan does not list the chat+enter glue: %+v", plan.Glue)
	}
	if _, err := os.Stat(glue); !os.IsNotExist(err) {
		t.Fatalf("roost module plan wrote the glue (err=%v)", err)
	}
	out = roost("module", "add", "enter")
	if !strings.Contains(out, "glue chat with enter: written") {
		t.Fatalf("add did not report the glue:\n%s", out)
	}
	body, err := os.ReadFile(glue)
	if err != nil || !bytes.Contains(body, []byte(`"example.com/studio/lobby/game/enter"`)) || bytes.Contains(body, []byte("{{")) {
		t.Fatalf("the glue file is missing or not rendered (err=%v):\n%s", err, body)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if got := manifest.Modules["chat"].With["enter"]; !contains(got, "game/chat/chat_enter.go") || len(manifest.Modules["enter"].With) != 0 {
		t.Fatalf("ownership: chat.with = %v, enter.with = %v", manifest.Modules["chat"].With, manifest.Modules["enter"].With)
	}

	// 重复安装没有变化。
	installed := moduleProjectDigest(t, root)
	out = roost("module", "add", "chat", "enter")
	if strings.Contains(out, "glue ") || moduleProjectDigest(t, root) != installed {
		t.Fatalf("installing again changed the project or wrote glue again:\n%s", out)
	}

	// 验收：两个模块各自的场景数不变，联动的场景单独一项。
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("verify: %v\n%+v", err, report)
	}
	got := map[string]int{}
	for _, result := range report.Modules {
		got[result.Name] = result.Passed
	}
	if want := map[string]int{"chat": 10, "chat+enter": 2, "enter": 14}; !reflect.DeepEqual(got, want) {
		t.Fatalf("verify results = %v, want %v", got, want)
	}
}
