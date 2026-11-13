package roost

import (
	"bytes"
	"strings"
	"testing"
)

// 模块的服务级入口可以落在托管的框架服务上（清单 runtime.framework）。下面几条不生成工程，只看生成器的判断
// 与渲染；真实工程上的安装与编译见 TestModulePortalInstallsOnTheHostedAccountService。

// hostedFixture 是 game 模板的清单，装着两个模块：login（没有入口）与 portal（入口在托管的 account 上）。
func hostedFixture(t *testing.T) Manifest {
	t.Helper()
	m := gameTemplateManifest(t)
	m.Modules = map[string]InstalledModule{
		"login":  {Service: "game"},
		"portal": {Service: "game", Requires: []string{"login"}, Runtime: &ModuleRuntime{Package: "services/account/portal", Start: "Start", Framework: "account"}},
	}
	return m
}

// runtime.framework 只能是一个框架服务的种类；工程里必须恰好有一个服务托管它。
func TestModuleRuntimeFrameworkNamesAHostedService(t *testing.T) {
	if err := validateModuleRuntime(&ModuleRuntime{Package: "services/account/portal", Start: "Start", Framework: "account"}); err != nil {
		t.Fatalf("a runtime on the hosted account service: %v", err)
	}
	// v1.47.0：托管服务的手写代码在 services/<服务>/。入口包在那里时必须写 runtime.framework，且服务对得上；
	// 没写 framework 的入口仍只能在 game/ 下（业务服务进程里启动）。
	if err := validateModuleRuntime(&ModuleRuntime{Package: "services/account/portal", Start: "Start"}); err == nil || !strings.Contains(err.Error(), "runtime.framework") {
		t.Fatalf("a services/ runtime without runtime.framework: %v", err)
	}
	if err := validateModuleRuntime(&ModuleRuntime{Package: "services/chat/portal", Start: "Start", Framework: "account"}); err == nil || !strings.Contains(err.Error(), "services/account/") {
		t.Fatalf("a services/ runtime under another framework service: %v", err)
	}
	if err := validateModuleRuntime(&ModuleRuntime{Package: "services/account/portal", Start: "Start", Framework: "game"}); err == nil || !strings.Contains(err.Error(), `runtime.framework "game" is not a framework service`) {
		t.Fatalf("a runtime on something that is not a framework service: %v", err)
	}
	m := hostedFixture(t)
	if host, err := frameworkHostService(m, "account"); err != nil || host != "account" {
		t.Fatalf("host of the account framework service = %q, %v", host, err)
	}
	// 托管它的服务可以叫别的名字：按 framework 的种类找，不按服务名。
	renamed := m
	renamed.Services = map[string]ServiceSpec{}
	for name, spec := range m.Services {
		if name == "account" {
			name = "identity"
		}
		renamed.Services[name] = spec
	}
	if host, err := frameworkHostService(renamed, "account"); err != nil || host != "identity" {
		t.Fatalf("host after renaming the service = %q, %v", host, err)
	}
	if got := hostedModuleRuntimes(renamed, "identity"); strings.Join(got, ",") != "portal" {
		t.Fatalf("modules started on the renamed host = %v", got)
	}
	delete(renamed.Services, "identity")
	if _, err := frameworkHostService(renamed, "account"); err == nil || !strings.Contains(err.Error(), "no service hosts the account framework service") {
		t.Fatalf("no host: %v", err)
	}
	twice := m
	twice.Services = map[string]ServiceSpec{}
	for name, spec := range m.Services {
		twice.Services[name] = spec
	}
	twice.Services["account2"] = ServiceSpec{Framework: "account"}
	if _, err := frameworkHostService(twice, "account"); err == nil || !strings.Contains(err.Error(), "account, account2 all host") {
		t.Fatalf("two hosts: %v", err)
	}
}

// 入口在托管服务上的模块不算业务服务的 runtime（不生成 modules_gen.go、bootstrap 不用 NewWithModules），
// 而是进托管服务的启动顺序；依赖只决定同一个进程里的先后，但仍必须装在同一个业务服务上。
func TestHostedModuleRuntimesBelongToTheHostNotTheBusinessService(t *testing.T) {
	m := hostedFixture(t)
	if hasModuleRuntime(m, "game") || hasModuleRuntime(m, "account") {
		t.Fatal("a module hosted on account counts as a business-service runtime")
	}
	if got := hostedModuleRuntimes(m, "account"); strings.Join(got, ",") != "portal" {
		t.Fatalf("modules started on account = %v", got)
	}
	if got := hostedModuleRuntimes(m, "game"); got != nil {
		t.Fatalf("a business service has hosted modules: %v", got)
	}
	if got := hostedModuleRuntimes(m, "mail"); got != nil {
		t.Fatalf("another hosted service has modules: %v", got)
	}
	// 两个模块的入口都在 account 上：依赖在前，其余按名字。
	m.Modules["beacon"] = InstalledModule{Service: "game", Runtime: &ModuleRuntime{Package: "game/beacon", Start: "Start", Framework: "account"}}
	m.Modules["audit"] = InstalledModule{Service: "game", Requires: []string{"portal"}, Runtime: &ModuleRuntime{Package: "game/audit", Start: "Start", Framework: "account"}}
	// 一个入口在业务服务上的模块不受影响，也不混进托管服务的顺序。
	m.Modules["skillbook"] = InstalledModule{Service: "game", Runtime: &ModuleRuntime{Package: "game/skillbook", Start: "Start"}}
	if got := strings.Join(hostedModuleRuntimes(m, "account"), ","); got != "portal,audit,beacon" {
		t.Fatalf("start order on account = %s, want portal,audit,beacon", got)
	}
	if order, err := moduleRuntimeOrder(m, "game"); err != nil || strings.Join(order, ",") != "skillbook" {
		t.Fatalf("start order on game = %v, %v", order, err)
	}
	if !hasModuleRuntime(m, "game") {
		t.Fatal("skillbook's runtime on the business service was lost")
	}
	// 业务服务自己的入口走 modules_gen.go，不能又被当成“托管在它上面”再启动一遍。
	if got := hostedModuleRuntimes(m, "game"); got != nil {
		t.Fatalf("the business service's own runtime is also listed as hosted on it: %v", got)
	}
	// 依赖装在别的业务服务上：拒绝。
	m.Modules["login"] = InstalledModule{Service: "other"}
	if _, err := moduleRuntimeOrder(m, "account"); err == nil || !strings.Contains(err.Error(), "module login is not installed for service game") {
		t.Fatalf("a dependency recorded for another business service: %v", err)
	}
}

// bootstrap：托管服务的装配到 owner Mod 为止不变，模块的 Mod 加在它后面；业务服务仍用 New()。
// 停机预算把多出来的这个 Mod 算进去。
func TestBootstrapStartsHostedModulesAfterTheOwnerMod(t *testing.T) {
	plain := gameTemplateManifest(t)
	m := hostedFixture(t)
	account := m.Services["account"]
	account.Mods = []string{"web"}
	m.Services["account"] = account
	body := renderBootstrap(m)
	const owner = "svcaccount.NewMod(serviceAccount.Verifier(), serviceAccount.Allocator(), serviceAccount.NameRules(), serviceAccount.Metrics())"
	const hosted = `kitmodulehost.NewMod("account", domainaccount.CapabilityName, kitmodulehost.Module{Name: "portal", Start: portalModule.Start})`
	for _, want := range []string{
		`kitmodulehost "github.com/tjbdwanghaibo/roost-core/wiring/modulehost"`,
		`portalModule "example.com/planet/services/account/portal"`,
		owner + ",\n\t\t" + hosted + ")\n",
		`a.RegisterServer(app.ServiceName("game"), serviceGame.New()`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("the bootstrap does not contain %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "NewWithModules") {
		t.Fatal("a module hosted on account made the business service use NewWithModules")
	}
	if web, mod := strings.Index(body, "kitweb.NewMod()"), strings.Index(body, hosted); web < 0 || web > mod {
		t.Fatalf("the web mod must be registered before the module mod (web at %d, modules at %d)", web, mod)
	}
	// 没有托管模块的工程，bootstrap 里没有这个 Mod，也不 import 它。
	if strings.Contains(renderBootstrap(plain), "modulehost") {
		t.Fatal("a project without hosted modules imports wiring/modulehost")
	}
	// 别的托管服务不受影响。
	if strings.Count(body, "kitmodulehost.NewMod(") != 1 {
		t.Fatalf("modulehost mods in the bootstrap: %d, want one", strings.Count(body, "kitmodulehost.NewMod("))
	}
	withWeb := plain
	withWeb.Services = map[string]ServiceSpec{}
	for name, spec := range plain.Services {
		withWeb.Services[name] = spec
	}
	withWeb.Services["account"] = account
	if before, after := serviceShutdownPlan(withWeb, "account").mods, serviceShutdownPlan(m, "account").mods; after != before+1 {
		t.Fatalf("the account service's shutdown plan counts %d mods with the module mod and %d without; want one more", after, before)
	}
}

// needs.mods 对的是入口所在的服务：portal 要 web，缺的是 account 上的 web，前置命令也是给 account 加。
// 工程没有托管 account 时装不了，错误说出原因。
func TestHostedModuleNeedsItsModsOnTheHost(t *testing.T) {
	manifest := gameTemplateManifest(t)
	root := t.TempDir()
	spec := ModuleSpec{Name: "portal", Needs: ModuleNeeds{Mods: []string{"web"}}, Runtime: &ModuleRuntime{Package: "services/account/portal", Start: "Start", Framework: "account"}}
	if service, err := moduleModsService(manifest, "game", spec); err != nil || service != "account" {
		t.Fatalf("the service a hosted module needs its mods on = %q, %v", service, err)
	}
	if service, err := moduleModsService(manifest, "game", ModuleSpec{Name: "skillbook", Runtime: &ModuleRuntime{Package: "game/skillbook", Start: "Start"}}); err != nil || service != "game" {
		t.Fatalf("a module without runtime.framework needs its mods on %q, %v", service, err)
	}
	if err := checkModuleNeeds(manifest, "game", spec); err != nil {
		t.Fatal(err)
	}
	if got := modulePrerequisites(root, manifest, "game", spec, ModuleOptions{}, false); !contains(got, "roost add mod web -service account") || contains(got, "roost add mod web -service game") {
		t.Fatalf("prerequisites = %v, want the web mod added to account, not to game", got)
	}
	// game 已经有 web 也不算：要的是 account 上的。
	game := manifest.Services["game"]
	game.Mods = append(append([]string(nil), game.Mods...), "web")
	withGameWeb := manifest
	withGameWeb.Services = map[string]ServiceSpec{}
	for name, value := range manifest.Services {
		withGameWeb.Services[name] = value
	}
	withGameWeb.Services["game"] = game
	if got := moduleMissingMods(withGameWeb, "game", spec); strings.Join(got, ",") != "web" {
		t.Fatalf("with web on game only, the installer adds %v to account; want web", got)
	}
	account := manifest.Services["account"]
	account.Mods = []string{"web"}
	withGameWeb.Services["account"] = account
	if got := moduleMissingMods(withGameWeb, "game", spec); len(got) != 0 {
		t.Fatalf("with web on account, the installer still adds %v", got)
	}
	// 不代办的 Mod 缺在托管服务上：点名托管服务。
	saga := spec
	saga.Needs = ModuleNeeds{Mods: []string{"manager"}}
	if err := checkModuleNeeds(manifest, "game", saga); err == nil || !strings.Contains(err.Error(), "service account has no manager mod: roost add mod manager -service account") {
		t.Fatalf("a mod the installer does not add, missing on the host: %v", err)
	}
	delete(withGameWeb.Services, "account")
	if err := checkModuleNeeds(withGameWeb, "game", spec); err == nil || !strings.Contains(err.Error(), "no service hosts the account framework service") {
		t.Fatalf("a project that does not host account: %v", err)
	}
}

// 开发配置里 web.addr 的端口按端口块派生（8080 + 块号）：块号 0 的服务仍是 8080，托管的 account（块号 1）是 8081。
// 别的 addr（ops 9100、玩家 TCP）不受影响；生产示例不动。
func TestDevWebPortFollowsThePortBlock(t *testing.T) {
	m := gameTemplateManifest(t).withPortBlocks()
	section := "web:\n  enabled: true\n  addr: 127.0.0.1:8080\nplayer_access:\n  tcp:\n    addr: 0.0.0.0:7000\n"
	if block := *m.Services["game"].PortBlock; block != 0 {
		t.Fatalf("fixture: game's port block is %d", block)
	}
	if got := localizeDevConfig(m, "game", section); got != section {
		t.Fatalf("the block-0 service's web section changed:\n%s", got)
	}
	account := localizeDevConfig(m, "account", section)
	if !strings.Contains(account, "  addr: 127.0.0.1:8081\n") || !strings.Contains(account, "    addr: 0.0.0.0:7000\n") {
		t.Fatalf("the account service (block %d) web section:\n%s", *m.Services["account"].PortBlock, account)
	}
	if ports := portsOf(m, "account"); ports.Web != 8081 || ports.Ops != 9101 {
		t.Fatalf("ports of account = %+v", ports)
	}
	// The production example keeps web on loopback: a non-loopback host is refused in production
	// unless web.allow_public_addr is set, and the generated example has to pass production validation.
	if got := productionizeConfig(section); !strings.Contains(got, "  addr: 127.0.0.1:8080\n") || strings.Contains(got, "CHANGE_ME:8080") {
		t.Fatalf("the production example's web address:\n%s", got)
	}
	var hint bytes.Buffer
	printWebRouteNext(&hint, "example.com/planet", "account", "ping", portsOf(m, "account").Web)
	if !strings.Contains(hint.String(), "curl http://127.0.0.1:8081/ping") {
		t.Fatalf("the webroute hint names the wrong port:\n%s", hint.String())
	}
}

// plan / add 的输出写明入口在哪个进程里启动。
func TestModulePlanSaysWhereAHostedRuntimeStarts(t *testing.T) {
	plan := ModulePlan{Service: "game", Prerequisites: []string{}, Glue: []ModuleGlueItem{}, Modules: []ModulePlanItem{
		{Name: "portal", Status: moduleStatusInstall, Runtime: &ModuleRuntime{Package: "services/account/portal", Start: "Start", Framework: "account"}},
		{Name: "skillbook", Status: moduleStatusInstall, Runtime: &ModuleRuntime{Package: "game/skillbook", Start: "Start"}},
	}}
	var out bytes.Buffer
	printModulePlan(&out, plan)
	for _, want := range []string{
		"  runtime:   services/account/portal.Start in the process of the hosted account service (after its Mods start, before service Init; stopped in reverse)\n",
		"  runtime:   game/skillbook.Start (before service Init; stop after service Shutdown)\n",
	} {
		if !strings.Contains(out.String(), want) {
			t.Fatalf("the plan does not print %q:\n%s", want, out.String())
		}
	}
	if !modulePlanHasHostedRuntime(plan) {
		t.Fatal("a plan that installs a hosted runtime does not ask for a project sync")
	}
	plan.Modules[0].Status = moduleStatusInstalled
	if modulePlanHasHostedRuntime(plan) {
		t.Fatal("an already installed hosted runtime asks for a project sync")
	}
}
