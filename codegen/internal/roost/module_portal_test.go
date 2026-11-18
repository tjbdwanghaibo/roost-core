package roost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// portal 模块在全新的 game 工程上：它的入口在托管的 account 服务上启动（清单 runtime.framework），所以安装器
// 给 account 补 web Mod（连同 web.* 配置段，开发端口按端口块是 8081，只绑回环地址），bootstrap 在 account 的
// Mod 列表末尾加上启动它的 Mod；业务服务 game 不因此多出 web Mod、适配层或 NewWithModules。依赖的 account_auth
// 跟着装（account 的协作者）；portal 不再依赖 login，所以不带玩家 TCP 接入、不带 cmd/accountctl。安装后提示列在输出里。
// 之后先装 enter：portal 与 enter 的整条链路场景还要 login（with.enter.requires），这时不写；再装 login，三组联动
// （portal+login、portal+enter、login+enter）在这一步写进工程。重复安装没有变化，project sync 之后没有漂移。
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
	if len(plan.Modules) != 2 || plan.Modules[0].Name != "account_auth" || plan.Modules[1].Name != "portal" {
		t.Fatalf("plan installs %+v, want account_auth then portal", plan.Modules)
	}
	if plan.Service != "game" || contains(plan.Prerequisites, "roost add transport tcp") {
		t.Fatalf("plan = service %q, prerequisites %v; want the game service and no player TCP (portal no longer brings login)", plan.Service, plan.Prerequisites)
	}
	// -template game 不带 account：account_auth（needs.uses）与 portal（runtime.framework）用到它，安装器在模块事务里声明，
	// portal 要的 web 直接写进新声明的 account（前置命令在事务之前执行，那时 account 还不存在）。
	if strings.Join(plan.FrameworkServices, ",") != "account" {
		t.Fatalf("plan framework services = %v, want account", plan.FrameworkServices)
	}
	if contains(plan.Prerequisites, "roost add mod web -service game") || contains(plan.Prerequisites, "roost add mod web -service account") {
		t.Fatalf("the plan adds the web mod with a prerequisite command: %v", plan.Prerequisites)
	}
	portal := plan.Modules[1]
	if portal.Runtime == nil || portal.Runtime.Package != "services/account/portal" || portal.Runtime.Start != "Start" || portal.Runtime.Framework != "account" {
		t.Fatalf("portal runtime = %+v, want services/account/portal.Start on the hosted account service", portal.Runtime)
	}
	if len(portal.Next) == 0 || !strings.Contains(strings.Join(portal.Next, "\n"), "configs/service/config.account.yaml") ||
		strings.Contains(strings.Join(portal.Next, "\n"), "{{") {
		t.Fatalf("portal's hints are missing or carry a placeholder: %q", portal.Next)
	}
	if got := strings.Join(plan.Modules[0].Replaces, ","); got != "services/account/collaborators.go" || contains(plan.Modules[0].Files, "cmd/accountctl/main.go") {
		t.Fatalf("account_auth replaces %q and ships %v; want the account collaborators stub and no operator command", got, plan.Modules[0].Files)
	}

	out := roost("module", "add", "portal")
	for _, want := range []string{
		"framework service account: declared (services.account.framework: account, added to services.game.uses",
		"module account_auth: installed", "module portal: installed",
		"  runtime:   services/account/portal.Start in the process of the hosted account service",
		"  file:      services/account/collaborators.go (replaces the untouched generated stub)\n",
		"  todo:      开发登录：在 configs/service/config.account.yaml 末尾加 login.dev_token",
		"  todo:      登录入口随托管 account 的进程监听 web.addr",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("roost module add portal did not print %q:\n%s", want, out)
		}
	}
	if strings.Contains(out, "module login") || strings.Contains(out, "cmd/accountctl") {
		t.Fatalf("roost module add portal installed login or its operator command:\n%s", out)
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(manifest.Services["account"].Mods, "web") || contains(manifest.Services["game"].Mods, "web") {
		t.Fatalf("web mod: account %v, game %v; want it on account only", manifest.Services["account"].Mods, manifest.Services["game"].Mods)
	}
	if record := manifest.Modules["portal"]; record.Runtime == nil || record.Runtime.Framework != "account" || strings.Join(record.Requires, ",") != "account_auth" || record.Service != "game" {
		t.Fatalf("portal's record = %+v", record)
	}
	if _, has := manifest.Access["player"]; has {
		t.Fatal("installing portal declared player access; that belongs to login")
	}
	read := func(rel string) string {
		t.Helper()
		body, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(body)
	}
	// account 是带着 web 声明的，它的配置生成时就有 web.* 段：开发配置绑回环地址，端口是 8080 + account 的端口块号（1）。
	if config := read("configs/service/config.account.yaml"); !strings.Contains(config, "\nweb:\n") || !strings.Contains(config, "addr: 127.0.0.1:8081") {
		t.Fatalf("config.account.yaml has no web section on 127.0.0.1:8081:\n%s", config)
	}
	if config := read("configs/service/config.game.yaml"); strings.Contains(config, "\nweb:\n") {
		t.Fatal("config.game.yaml gained a web section; the login entry does not listen on the game process")
	}
	bootstrap := read("internal/bootstrap/generated.go")
	const hosted = `kitmodulehost.NewMod("account", domainaccount.CapabilityName, kitmodulehost.Module{Name: "portal", Start: portalModule.Start, Config: kitmodulehost.ConfigOf[portalModule.Config](nil)})`
	if !strings.Contains(bootstrap, hosted) || !strings.Contains(bootstrap, `portalModule "example.com/studio/frontdoor/services/account/portal"`) ||
		strings.Contains(bootstrap, "NewWithModules") {
		t.Fatalf("the bootstrap does not start the portal on the account service (or wraps the business service):\n%s", bootstrap)
	}
	if _, err := os.Stat(filepath.Join(root, "internal/service/game/modules_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("the business service got a module adapter: %v", err)
	}
	// 入口不 import 任何 game 包：不带 game 的工程也能装（TestModulePortalInstallsOnALobbyProjectWithoutGame）。
	if source := read("services/account/portal/portal.go"); strings.Contains(source, "/game/") {
		t.Fatalf("services/account/portal/portal.go imports a game package:\n%s", source)
	}

	// 先装 enter：portal 与 enter 的链路场景还要 login 的认证器，login 不在时不写。
	out = roost("module", "add", "enter")
	if strings.Contains(out, "glue portal with enter") {
		t.Fatalf("installing enter without login wrote the portal+enter glue that needs login:\n%s", out)
	}
	// 再装 login：它与 portal、enter 的联动，以及 portal+enter（三个都在了）在这一步写进工程。
	out = roost("module", "add", "login")
	for _, want := range []string{"glue portal with login: written", "glue portal with enter: written", "glue login with enter: written", "  file:      cmd/accountctl/main.go\n"} {
		if !strings.Contains(out, want) {
			t.Fatalf("installing login did not print %q:\n%s", want, out)
		}
	}
	// 运维命令是渲染过的普通 Go 文件：工程名进了缺省的键前缀与区服名。只认模板占位（{{MODULE}} 一类大写名字）：
	// Go 的复合字面量 {{Name: …}} 不是占位。
	if command := read("cmd/accountctl/main.go"); !strings.Contains(command, `"roost:frontdoor:account"`) || regexp.MustCompile(`\{\{[A-Z_]+\}\}`).MatchString(command) {
		t.Fatalf("cmd/accountctl/main.go was not rendered for this project:\n%s", command)
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
	if counts["account_auth"] != 7 || counts["login"] != 34 || counts["portal"] != 15 || counts["portal+login"] != 1 ||
		counts["portal+enter"] != 2 || counts["login+enter"] != 6 || counts["enter"] != 29 {
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

// 不带 game 的开房间工程（-template lobby：只托管 account、lobby、relay，没有运行 Nest 的业务服务）：
// roost module add portal 装上 account_auth 与 portal，记录的 service 为空；web Mod 由前置命令加在托管 account 的服务上，
// 入口经 modulehost 在 account 进程里启动。装完 doctor 没有 FAIL（collaborators:account 通过），模块场景全过，
// build / vet / id check / generate --check 通过。要业务服务的模块（login）在这种工程里装不上，并说清楚原因。
func TestModulePortalInstallsOnALobbyProjectWithoutGame(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a project and runs go build / vet / test in it")
	}
	t.Parallel()
	root, roost, try := moduleProjectFrom(t, NewOptions{Name: "party", Template: lobbyTemplateName})
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range checkFrameworkCollaborators(root, manifest) {
		if item.Name == "collaborators:account" && (item.Status != StatusFail || !strings.Contains(item.Detail, "roost module add account_auth")) {
			t.Fatalf("a fresh lobby project's account collaborators: %+v; want the stub reported with the module that implements it", item)
		}
	}
	var plan ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "portal", "--json")), &plan); err != nil {
		t.Fatal(err)
	}
	if plan.Service != "" || len(plan.Modules) != 2 || plan.Modules[0].Name != "account_auth" || plan.Modules[1].Name != "portal" ||
		len(plan.FrameworkServices) != 0 || strings.Join(plan.Prerequisites, ",") != "roost add mod web -service account" {
		t.Fatalf("plan = %+v; want no business service, account_auth then portal, web added to account", plan)
	}
	out := roost("module", "add", "portal")
	if !strings.Contains(out, "service: none (this project hosts only framework services") || !strings.Contains(out, "module portal: installed") {
		t.Fatalf("roost module add portal on a lobby project:\n%s", out)
	}
	if manifest, err = LoadManifest(root); err != nil {
		t.Fatal(err)
	}
	if auth, portal := manifest.Modules["account_auth"], manifest.Modules["portal"]; auth.Service != "" || portal.Service != "" ||
		strings.Join(portal.Requires, ",") != "account_auth" || portal.Runtime == nil || portal.Runtime.Framework != "account" {
		t.Fatalf("records: account_auth %+v, portal %+v", auth, portal)
	}
	if !contains(manifest.Services["account"].Mods, "web") || len(manifest.Services) != 3 {
		t.Fatalf("services after the install = %+v; want web on account and still only account, lobby, relay", manifest.Services)
	}
	for _, item := range checkFrameworkCollaborators(root, manifest) {
		if item.Status != StatusOK {
			t.Fatalf("doctor after installing portal: %+v", item)
		}
	}
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("verify: %v\n%+v", err, report)
	}
	counts := map[string]int{}
	for _, module := range report.Modules {
		counts[module.Name] = module.Passed
	}
	if counts["account_auth"] != 7 || counts["portal"] != 15 || len(counts) != 2 {
		t.Fatalf("scenario counts = %v", counts)
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "portal")
	roost("project", "sync")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("installing portal again or project sync changed the lobby project")
	}
	if out, err := try("module", "add", "login"); err == nil || !strings.Contains(err.Error(), "module login needs a business service with the nest mod and this project has none") {
		t.Fatalf("installing login on a project without a business service: err = %v\n%s", err, out)
	}
	if moduleProjectDigest(t, root) != before {
		t.Fatal("a refused login install changed the lobby project")
	}
}
