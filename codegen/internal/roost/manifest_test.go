package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 未知的 mod / feature 名一律拒绝并点名；改名前的旧名（"sync"、"room" mod，"replication-*" feature）同样是未知名，
// 不在内存里改写成新名——项目未上线，roost.yaml 需要手工改成 syncbus / nettransport-*。
func TestValidateRejectsUnknownAndRetiredModAndFeatureNames(t *testing.T) {
	for _, mod := range []string{"not_a_mod", "sync", "room"} {
		manifest := DefaultManifest("planet", "example.com/planet", []string{"game"},
			[]string{mod}, []string{"nest"})
		if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unknown wiring mod %q", mod)) {
			t.Fatalf("mod %q: Validate = %v, want it refused by name", mod, err)
		}
	}
	shared := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"nest"}, []string{"nest"})
	shared.SharedMods = []string{"sync"}
	if err := shared.Validate(); err == nil || !strings.Contains(err.Error(), `unknown wiring mod "sync"`) {
		t.Fatalf("shared_mods sync: Validate = %v, want it refused by name", err)
	}
	for _, feature := range []string{"not_a_feature", "replication-quic", "replication-kcp", "replication-udp", "event"} {
		manifest := DefaultManifest("planet", "example.com/planet", []string{"game"},
			[]string{"syncbus"}, []string{feature})
		if err := manifest.Validate(); err == nil || !strings.Contains(err.Error(), fmt.Sprintf("unknown feature %q", feature)) {
			t.Fatalf("feature %q: Validate = %v, want it refused by name", feature, err)
		}
	}
}

// Managers are selected per service type, so a process-wide manager mod would
// start one service's singletons inside every service. shared_mods must reject
// it with a message that says where it belongs.
func TestValidateRejectsManagerModInSharedMods(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", []string{"game"},
		[]string{"nest"}, []string{"nest"})
	manifest.SharedMods = []string{"lock", "manager"}

	err := manifest.Validate()
	if err == nil {
		t.Fatal("shared_mods accepted the manager mod")
	}
	if !strings.Contains(err.Error(), "per service") {
		t.Fatalf("error does not explain where the manager mod belongs: %v", err)
	}
}

// The manager mod is part of the default set, and a default manifest must
// still validate — a default that fails its own validator is worse than no
// default.
func TestDefaultManifestIncludesManagerModAndValidates(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	if err := manifest.Validate(); err != nil {
		t.Fatalf("the default manifest does not validate: %v", err)
	}
	mods := manifest.Services["game"].Mods
	if !contains(mods, "manager") {
		t.Fatalf("default mods do not include the manager mod: %v", mods)
	}
}

// The manager mod's constructor must be wired per service, taking that
// service's own manager list. A shared constructor would give every service
// the same managers, which is exactly what the per-service rule prevents.
func TestBootstrapWiresManagerModPerService(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet",
		[]string{"game", "gate"}, []string{"manager"}, []string{"nest"})
	rendered := renderBootstrap(manifest)

	for _, want := range []string{
		`kitmanager "github.com/tjbdwanghaibo/roost-core/wiring/manager"`,
		"kitmanager.NewManagerMod(serviceGame.Managers()...)",
		"kitmanager.NewManagerMod(serviceGate.Managers()...)",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("bootstrap is missing %q:\n%s", want, rendered)
		}
	}
	// A bare constructor would mean the service's own list was ignored.
	if strings.Contains(rendered, "kitmanager.NewManagerMod()") {
		t.Fatalf("bootstrap wired a manager mod with no managers:\n%s", rendered)
	}
}

// The generated hook must share service.go's package clause, or the two files
// cannot live in the same directory.
func TestServiceManagersHookMatchesServicePackageClause(t *testing.T) {
	for _, name := range []string{"game", "world_boss"} {
		var m Manifest
		service := renderService(m.servicePackage(name), name)
		managers := renderServiceManagers(m.servicePackage(name))
		clause := func(source string) string {
			line, _, _ := strings.Cut(source, "\n")
			return line
		}
		if clause(service) != clause(managers) {
			t.Fatalf("%s: service.go says %q but managers.go says %q",
				name, clause(service), clause(managers))
		}
		if !strings.Contains(managers, "func Managers() []app.IManager") {
			t.Fatalf("%s: hook does not expose Managers()", name)
		}
	}
}

// v1.47.0：game 服务（玩家接入所在、或唯一跑 Nest 的业务服务）的 manager 登记点在 game/managers/managers.go，
// bootstrap 调 gamemanagers.Managers()；别的服务仍是 internal/service/<name>/managers.go。服务目录里已经有旧的
// managers.go 时沿用它，不改写业务文件。
func TestGameServiceManagersLiveInGameManagers(t *testing.T) {
	root := t.TempDir()
	manifest := DefaultManifest("planet", "example.com/planet",
		[]string{"game", "gate"}, []string{"manager"}, []string{"nest"})
	game := manifest.Services["game"]
	game.Mods = append(game.Mods, "nest")
	manifest.Services["game"] = game
	m := manifest.withServicePackages(root)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	file, ok := plan["game/managers/managers.go"]
	if !ok {
		t.Fatalf("game/managers/managers.go is not generated")
	}
	for _, want := range []string{"package managers", "func Managers() []app.IManager", "DependsOn", "Start 时 World 还不存在", "什么时候用"} {
		if !strings.Contains(string(file.Body), want) {
			t.Errorf("game/managers/managers.go lacks %q:\n%s", want, file.Body)
		}
	}
	if _, ok := plan["internal/service/game/managers.go"]; ok {
		t.Errorf("the game service still gets internal/service/game/managers.go")
	}
	if _, ok := plan["internal/service/gate/managers.go"]; !ok {
		t.Errorf("another service lost its internal/service/gate/managers.go")
	}
	bootstrap := renderBootstrap(m)
	for _, want := range []string{`gamemanagers "example.com/planet/game/managers"`, "kitmanager.NewManagerMod(gamemanagers.Managers()...)", "kitmanager.NewManagerMod(serviceGate.Managers()...)"} {
		if !strings.Contains(bootstrap, want) {
			t.Errorf("bootstrap lacks %q", want)
		}
	}
	// 旧工程：服务目录里已有 managers.go 时沿用。
	if err := os.MkdirAll(filepath.Join(root, "internal", "service", "game"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal", "service", "game", "managers.go"), []byte("package game\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := manifest.withServicePackages(root).gameManagers; got != "" {
		t.Errorf("an existing internal/service/game/managers.go must keep the old location, gameManagers = %q", got)
	}
}
