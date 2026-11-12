package roost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// portal 模块在全新的 game 工程上：它的入口在托管的 account 服务上启动（清单 runtime.framework），所以安装器
// 给 account 补 web Mod（连同 web.* 配置段，开发端口按端口块是 8081，只绑回环地址），bootstrap 在 account 的
// Mod 列表末尾加上启动它的 Mod；业务服务 game 不因此多出 web Mod、适配层或 NewWithModules。依赖的 login 跟着装
// 并带来运维命令 cmd/accountctl；安装后提示列在输出里。之后再装 enter，整条链路的联动场景随它写进工程。
// 重复安装没有变化，project sync 之后没有漂移。
func TestModulePortalInstallsOnTheHostedAccountService(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, _ := moduleProject(t, "frontdoor")
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "portal", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if len(plan.Modules) != 2 || plan.Modules[0].Name != "login" || plan.Modules[1].Name != "portal" {
		t.Fatalf("plan installs %+v, want login then portal", plan.Modules)
	}
	for _, command := range []string{"roost add transport tcp", "roost add mod web -service account"} {
		if !contains(plan.Prerequisites, command) {
			t.Fatalf("plan prerequisites %v do not list %q", plan.Prerequisites, command)
		}
	}
	if contains(plan.Prerequisites, "roost add mod web -service game") {
		t.Fatalf("the plan adds the web mod to the business service: %v", plan.Prerequisites)
	}
	portal := plan.Modules[1]
	if portal.Runtime == nil || portal.Runtime.Package != "game/portal" || portal.Runtime.Start != "Start" || portal.Runtime.Framework != "account" {
		t.Fatalf("portal runtime = %+v, want game/portal.Start on the hosted account service", portal.Runtime)
	}
	if len(portal.Next) == 0 || !strings.Contains(strings.Join(portal.Next, "\n"), "configs/service/config.account.yaml") ||
		strings.Contains(strings.Join(portal.Next, "\n"), "{{") {
		t.Fatalf("portal's hints are missing or carry a placeholder: %q", portal.Next)
	}
	if !contains(plan.Modules[0].Files, "cmd/accountctl/main.go") {
		t.Fatalf("login does not ship its operator command: %v", plan.Modules[0].Files)
	}

	out := roost("module", "add", "portal")
	for _, want := range []string{
		"prerequisite (did run): roost add mod web -service account",
		"module login: installed", "module portal: installed",
		"  runtime:   game/portal.Start in the process of the hosted account service",
		"  file:      cmd/accountctl/main.go\n",
		"  todo:      打开玩家 TCP 监听：roost config enable player-tcp",
		"go run ./cmd/accountctl upsert-server -sid <game 进程的 sid>",
		"  todo:      登录入口随托管 account 的进程监听 web.addr",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("roost module add portal did not print %q:\n%s", want, out)
		}
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(manifest.Services["account"].Mods, "web") || contains(manifest.Services["game"].Mods, "web") {
		t.Fatalf("web mod: account %v, game %v; want it on account only", manifest.Services["account"].Mods, manifest.Services["game"].Mods)
	}
	if record := manifest.Modules["portal"]; record.Runtime == nil || record.Runtime.Framework != "account" || strings.Join(record.Requires, ",") != "login" || record.Service != "game" {
		t.Fatalf("portal's record = %+v", record)
	}
	read := func(rel string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	// web.* 配置段由 roost add mod 写进 account 的配置：开发配置绑回环地址，端口是 8080 + account 的端口块号。
	if config := read("configs/service/config.account.yaml"); !strings.Contains(config, "\nweb:\n") || !strings.Contains(config, "addr: 127.0.0.1:8081") {
		t.Fatalf("config.account.yaml has no web section on 127.0.0.1:8081:\n%s", config)
	}
	if config := read("configs/service/config.game.yaml"); strings.Contains(config, "\nweb:\n") {
		t.Fatal("config.game.yaml gained a web section; the login entry does not listen on the game process")
	}
	bootstrap := read("internal/bootstrap/generated.go")
	const hosted = `kitmodulehost.NewMod("account", domainaccount.CapabilityName, kitmodulehost.Module{Name: "portal", Start: portalModule.Start})`
	if !strings.Contains(bootstrap, hosted) || !strings.Contains(bootstrap, `portalModule "example.com/studio/frontdoor/game/portal"`) ||
		strings.Contains(bootstrap, "NewWithModules") {
		t.Fatalf("the bootstrap does not start the portal on the account service (or wraps the business service):\n%s", bootstrap)
	}
	if _, err := os.Stat(filepath.Join(root, "internal/service/game/modules_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("the business service got a module adapter: %v", err)
	}
	// 运维命令是渲染过的普通 Go 文件：工程名进了缺省的键前缀与区服名。
	if command := read("cmd/accountctl/main.go"); !strings.Contains(command, `"roost:frontdoor:account"`) || strings.Contains(command, "{{") {
		t.Fatalf("cmd/accountctl/main.go was not rendered for this project:\n%s", command)
	}

	// 再装 enter：portal 与 login 各自和它的联动在这一步写进工程。
	out = roost("module", "add", "enter")
	for _, want := range []string{"glue portal with enter: written", "glue login with enter: written"} {
		if !strings.Contains(out, want) {
			t.Fatalf("installing enter did not write %q:\n%s", want, out)
		}
	}
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("verify: %v\n%+v", err, report)
	}
	counts := map[string]int{}
	for _, module := range report.Modules {
		if !module.OK || module.Skipped != 0 {
			t.Fatalf("module %+v", module)
		}
		counts[module.Name] = module.Passed
	}
	if counts["login"] != 28 || counts["portal"] != 12 || counts["portal+enter"] != 2 || counts["login+enter"] != 2 || counts["enter"] != 14 {
		t.Fatalf("scenario counts = %v", counts)
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "enter", "portal", "login")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("installing the modules again changed the project")
	}
	roost("project", "sync")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("project sync after the install changed the project (the hosted module mod or the shutdown budget drifted)")
	}
	// 已经装好的工程再问一次：没有前置命令，也不再提示。
	var again ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "portal", "--json")), &again); err != nil {
		t.Fatal(err)
	}
	if len(again.Prerequisites) != 0 || again.Modules[1].Status != moduleStatusInstalled || len(again.Modules[1].Next) != 0 {
		t.Fatalf("a second plan = prerequisites %v, portal %s with hints %q", again.Prerequisites, again.Modules[1].Status, again.Modules[1].Next)
	}
}
