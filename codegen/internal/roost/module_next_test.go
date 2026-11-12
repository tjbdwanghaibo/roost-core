package roost

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// 清单的 next（安装后提示）：通过校验的提示原样进计划，只替换工程名字占位；plan 与 add 的文本输出在这次要装的
// 模块下逐条列出，JSON 里是 modules[].next；已经装好的模块不再提示。安装器不执行它们。
func TestModuleNextHintsAreRenderedIntoThePlanAndPrinted(t *testing.T) {
	catalog, err := loadModuleCatalogFS(moduleFixtureFS(moduleFixtureYAML+
		"next:\n  - \"enable the listener: roost config enable player-tcp\"\n  - \"  edit configs/service/config.{{GAME_SERVICE}}.yaml of {{PROJECT}} ({{MODULE}}, package {{GAME_SERVICE_PKG}})  \"\n",
		map[string]string{"game/x.go": "package x\n// {{ID:component:Demo}} {{ID:errcode:demo_bad}}\n"}))
	if err != nil {
		t.Fatal(err)
	}
	spec := catalog["demo"]
	if len(spec.Next) != 2 {
		t.Fatalf("the manifest's next was not read: %q", spec.Next)
	}
	manifest := gameTemplateManifest(t)
	hints := moduleNextHints(manifest, "arena", spec)
	want := []string{
		"enable the listener: roost config enable player-tcp",
		"edit configs/service/config.arena.yaml of planet (example.com/planet, package arena)",
	}
	if strings.Join(hints, "\n") != strings.Join(want, "\n") {
		t.Fatalf("rendered hints = %q, want %q", hints, want)
	}
	if got := moduleNextHints(manifest, "arena", ModuleSpec{Name: "quiet"}); got != nil {
		t.Fatalf("a module without next produced hints: %q", got)
	}

	plan := ModulePlan{
		Service: "arena", Prerequisites: []string{}, Glue: []ModuleGlueItem{},
		Modules: []ModulePlanItem{
			{Name: "demo", Status: moduleStatusInstall, Files: []string{"game/x.go"}, Next: hints},
			// 已装的模块即使带着提示也不打印：提示只跟着“这一次安装”走。
			{Name: "old", Status: moduleStatusInstalled, Next: []string{"must not be printed"}},
		},
	}
	for _, applied := range []bool{false, true} {
		plan.Applied = applied
		var out bytes.Buffer
		printModulePlan(&out, plan)
		text := out.String()
		for _, hint := range want {
			if !strings.Contains(text, "  todo:      "+hint+"\n") {
				t.Fatalf("applied=%v: the output does not list the hint %q:\n%s", applied, hint, text)
			}
		}
		if strings.Contains(text, "must not be printed") {
			t.Fatalf("applied=%v: an installed module's hints were printed:\n%s", applied, text)
		}
		if strings.Index(text, "  file:      game/x.go") > strings.Index(text, "  todo:      ") {
			t.Fatalf("applied=%v: the hints must come after the module's files:\n%s", applied, text)
		}
	}
	encoded, err := json.Marshal(plan)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct {
		Modules []struct {
			Name string   `json:"name"`
			Next []string `json:"next"`
		} `json:"modules"`
	}
	if err := json.Unmarshal(encoded, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Modules) != 2 || strings.Join(decoded.Modules[0].Next, "\n") != strings.Join(want, "\n") {
		t.Fatalf("the JSON plan does not carry the hints under modules[].next: %s", encoded)
	}
}

// needs.mods 里的 web 由安装器代办（与 heartbeat、directory 一样）：服务没有它时不算“装不了”，而是列一条
// 前置命令；已经有了就什么都不列。别的 Mod 仍然只报缺项、给出命令，不代办。
func TestModuleNeedsWebIsAPrerequisiteTheInstallerRuns(t *testing.T) {
	manifest := gameTemplateManifest(t)
	root := t.TempDir()
	spec := ModuleSpec{Name: "frontdoor", Needs: ModuleNeeds{Mods: []string{"web", "nest"}}}
	if contains(manifest.Services["game"].Mods, "web") {
		t.Fatal("fixture: the game template already has the web mod")
	}
	if err := checkModuleNeeds(manifest, "game", spec); err != nil {
		t.Fatalf("a module that needs the web mod is refused instead of having it added: %v", err)
	}
	if got := moduleMissingMods(manifest, "game", spec); strings.Join(got, ",") != "web" {
		t.Fatalf("mods the installer will add = %v, want web alone (nest is already there)", got)
	}
	const command = "roost add mod web -service game"
	if got := modulePrerequisites(root, manifest, "game", spec, ModuleOptions{}, false); !contains(got, command) {
		t.Fatalf("prerequisites = %v, want %q", got, command)
	}

	withWeb := manifest
	service := withWeb.Services["game"]
	service.Mods = append(append([]string(nil), service.Mods...), "web")
	withWeb.Services = map[string]ServiceSpec{}
	for name, value := range manifest.Services {
		withWeb.Services[name] = value
	}
	withWeb.Services["game"] = service
	if got := moduleMissingMods(withWeb, "game", spec); len(got) != 0 {
		t.Fatalf("a service that has the web mod still lists %v", got)
	}
	if got := modulePrerequisites(root, withWeb, "game", spec, ModuleOptions{}, false); contains(got, command) {
		t.Fatalf("prerequisites on a service with the web mod = %v", got)
	}

	// 不在代办名单里的 Mod：拒绝并给出命令，不出现在前置命令里。
	other := ModuleSpec{Name: "sagas", Needs: ModuleNeeds{Mods: []string{"saga"}}}
	if err := checkModuleNeeds(manifest, "game", other); err == nil || !strings.Contains(err.Error(), "roost add mod saga -service game") {
		t.Fatalf("a module that needs the saga mod: %v", err)
	}
	if got := moduleMissingMods(manifest, "game", other); len(got) != 0 {
		t.Fatalf("the installer would add %v on its own", got)
	}
	for _, name := range []string{"heartbeat", "directory", "web"} {
		if !moduleCanAddMod(name) {
			t.Fatalf("the installer no longer adds the %s mod", name)
		}
	}
	for _, name := range []string{"saga", "nats", "mongo", "redis", ""} {
		if moduleCanAddMod(name) {
			t.Fatalf("the installer adds the %q mod on its own", name)
		}
	}
}
