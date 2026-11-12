package roost

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// portal 模块在全新的 game 工程上：安装器先补 web Mod（连同 web.* 配置段，缺省只绑回环地址），依赖的 login
// 跟着装并带来运维命令 cmd/accountctl；服务级启动入口进适配层；安装后提示列在输出里。之后再装 enter，
// 整条链路的联动场景随它写进工程。重复安装没有变化。
func TestModulePortalInstallsTheWebModTheLoginCommandAndItsHints(t *testing.T) {
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
	for _, command := range []string{"roost add transport tcp", "roost add mod web -service game"} {
		if !contains(plan.Prerequisites, command) {
			t.Fatalf("plan prerequisites %v do not list %q", plan.Prerequisites, command)
		}
	}
	portal := plan.Modules[1]
	if portal.Runtime == nil || portal.Runtime.Package != "game/portal" || portal.Runtime.Start != "Start" {
		t.Fatalf("portal runtime = %+v", portal.Runtime)
	}
	if len(portal.Next) == 0 || !strings.Contains(strings.Join(portal.Next, "\n"), "configs/service/config.game.yaml") ||
		strings.Contains(strings.Join(portal.Next, "\n"), "{{") {
		t.Fatalf("portal's hints are missing or carry a placeholder: %q", portal.Next)
	}
	if !contains(plan.Modules[0].Files, "cmd/accountctl/main.go") {
		t.Fatalf("login does not ship its operator command: %v", plan.Modules[0].Files)
	}

	out := roost("module", "add", "portal")
	for _, want := range []string{
		"prerequisite (did run): roost add mod web -service game",
		"module login: installed", "module portal: installed",
		"  file:      cmd/accountctl/main.go\n",
		"  todo:      打开玩家 TCP 监听：roost config enable player-tcp",
		"go run ./cmd/accountctl upsert-server -sid <game 进程的 sid>\n",
		"  todo:      登录入口随 game 进程监听 web.addr",
	} {
		if !strings.Contains(out, want) {
			t.Fatalf("roost module add portal did not print %q:\n%s", want, out)
		}
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(manifest.Services["game"].Mods, "web") {
		t.Fatalf("the game service has no web mod after the install: %v", manifest.Services["game"].Mods)
	}
	if record := manifest.Modules["portal"]; record.Runtime == nil || strings.Join(record.Requires, ",") != "login" || record.Service != "game" {
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
	// web.* 配置段由 roost add mod 写下：开发配置绑回环地址。
	if config := read("configs/service/config.game.yaml"); !strings.Contains(config, "\nweb:\n") || !strings.Contains(config, "addr: 127.0.0.1:8080") {
		t.Fatalf("config.game.yaml has no web section bound to loopback:\n%s", config)
	}
	if adapter := read("internal/service/game/modules_gen.go"); !strings.Contains(adapter, `portalModule "example.com/studio/frontdoor/game/portal"`) ||
		!strings.Contains(adapter, "portalModule.Start(registry)") {
		t.Fatalf("the service adapter does not start the portal:\n%s", adapter)
	}
	if bootstrap := read("internal/bootstrap/generated.go"); !strings.Contains(bootstrap, "kitweb.NewMod()") {
		t.Fatal("the bootstrap does not assemble the web mod")
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
	if counts["login"] != 22 || counts["portal"] != 13 || counts["portal+enter"] != 2 || counts["login+enter"] != 2 || counts["enter"] != 14 {
		t.Fatalf("scenario counts = %v", counts)
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "enter", "portal", "login")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("installing the modules again changed the project")
	}
	// 已经有 web Mod 的工程：计划里不再有那条前置命令（这里用已装好的工程再问一次别的模块来确认）。
	var again ModulePlan
	if err := json.Unmarshal([]byte(roost("module", "plan", "portal", "--json")), &again); err != nil {
		t.Fatal(err)
	}
	if len(again.Prerequisites) != 0 || again.Modules[1].Status != moduleStatusInstalled || len(again.Modules[1].Next) != 0 {
		t.Fatalf("a second plan = prerequisites %v, portal %s with hints %q", again.Prerequisites, again.Modules[1].Status, again.Modules[1].Next)
	}
}
