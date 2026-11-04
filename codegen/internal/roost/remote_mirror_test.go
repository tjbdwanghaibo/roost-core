package roost

import (
	"bytes"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// 只读 Mirror Mod（联盟服务方案 §2.10 第 3 项）：只读进程装 remote_mirror、不装 remote_entity。catalog 认识它，
// 依赖 redis / syncbus / mongo 自动带上，配置段来自 Mirror Mod 自己的声明，停机预算按它声明的 5s 计入，
// 与 remote_entity 装在同一个服务时拒绝。
func TestRemoteMirrorModIsACatalogModExclusiveWithRemoteEntity(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "configdata")
	var out bytes.Buffer
	if err := Run([]string{"add", "mod", "remote_mirror", "-service", "game", "--root", root}, &out, &out); err != nil {
		t.Fatalf("add mod remote_mirror: %v\n%s", err, out.String())
	}
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	resolved, err := resolveMods(m.Services["game"].Mods)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"remote_mirror", "redis", "syncbus", "mongo", "nats"} {
		if !contains(resolved, want) {
			t.Errorf("game mods %v lack %s", resolved, want)
		}
	}
	if contains(resolved, "remote_entity") {
		t.Errorf("a read-only process must not get the writing remote_entity Mod: %v", resolved)
	}
	if bootstrap := readProjectFile(t, root, "internal/bootstrap/generated.go"); !strings.Contains(bootstrap, "kitremoteentity.NewRemoteMirrorMod(0)") {
		t.Errorf("bootstrap does not assemble the mirror Mod:\n%s", bootstrap)
	}
	if config := readProjectFile(t, root, "configs/service/config.game.yaml"); !strings.Contains(config, "\nremote_entity:\n") || !strings.Contains(config, "shutdown_timeout: 5s") {
		t.Errorf("config.game.yaml lacks the mirror section:\n%s", config)
	}
	without := serviceShutdownPlan(m, "game")
	if without.declared < 5*time.Second {
		t.Errorf("shutdown plan declared %v; the mirror Mod's 5s budget is not counted", without.declared)
	}

	out.Reset()
	err = Run([]string{"add", "mod", "remote_entity", "-service", "game", "--root", root}, &out, &out)
	if err == nil || !strings.Contains(err.Error(), "remote_mirror") || !strings.Contains(err.Error(), "remote_entity") {
		t.Fatalf("remote_entity next to remote_mirror must be refused naming both, got %v\n%s", err, out.String())
	}

	if testing.Short() {
		return
	}
	useLocalRoostCore(t, root)
	for _, action := range []string{"build", "vet"} {
		check := exec.Command("go", action, "./...")
		check.Dir = root
		check.Env = append(os.Environ(), "GOWORK=off")
		if output, err := check.CombinedOutput(); err != nil {
			t.Fatalf("go %s with remote_mirror: %v\n%s", action, err, output)
		}
	}
}
