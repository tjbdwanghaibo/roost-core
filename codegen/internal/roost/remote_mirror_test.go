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

// DataEngine 的 Remote 投影只开在装了 remote_entity 的服务上：同一工程里另一个服务（只读 Mirror 进程，或根本
// 不碰 Remote 的服务）装 dataengine 时不能带 WithRemoteProjection(true)——那个服务没有 remote_entity 能力，
// 起服时 dataengine Provide 报 capability "remote_entity" not found。game-demo 联盟方案第 4 阶段
// （game 只装 remote_mirror、alliance 装 remote_entity）第一次真正起进程时撞到。
func TestDataEngineRemoteProjectionFollowsTheServicesOwnRemoteEntity(t *testing.T) {
	t.Parallel()
	m := Manifest{Services: map[string]ServiceSpec{
		"game":     {Mods: []string{"dataengine", "remote_mirror"}},
		"alliance": {Mods: []string{"dataengine", "remote_entity"}},
		"plain":    {Mods: []string{"dataengine"}},
	}}
	allMods := allProjectMods(m)
	for service, want := range map[string]bool{"alliance": true, "game": false, "plain": false} {
		mods, err := resolveMods(effectiveServiceMods(m, service))
		if err != nil {
			t.Fatal(err)
		}
		got := ""
		for _, constructor := range serviceModConstructors(m, service, allMods) {
			if strings.HasPrefix(constructor, "kitdataengine.NewMod(") {
				got = constructor
			}
		}
		if got == "" {
			t.Fatalf("%s (mods %v) assembles no dataengine Mod", service, mods)
		}
		if strings.Contains(got, "WithRemoteProjection(true)") != want {
			t.Errorf("%s (mods %v): %s; remote projection wanted=%v", service, mods, got, want)
		}
	}
}

// doctor 按配置核对停机窗口时也要计入只读 Mirror 的预算（remote_entity.mirror.shutdown_timeout，生成配置写 5s）：
// 生成器的计划算了它，doctor 却把它当作“声明了 0s”，比计划少 5s，于是差一秒盖不住的 total 被判 OK。
func TestDoctorCountsTheMirrorStopBudget(t *testing.T) {
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
	plan := serviceShutdownPlan(m, "game")
	dev := "configs/service/config.game.yaml"
	short := plan.total - plan.margin - time.Second
	setShutdownTotal(t, root, dev, seconds(short))
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusWarn || !strings.Contains(item.Detail, dev+": total_timeout "+seconds(short)+" cannot cover") {
		t.Fatalf("total_timeout %s, one second short of the plan (%s declared, the mirror's 5s among them): %s %s, want a WARN naming %s",
			seconds(short), seconds(plan.declared), item.Status, item.Detail, dev)
	}
	setShutdownTotal(t, root, dev, seconds(plan.total))
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK {
		t.Fatalf("total_timeout %s as generated: %s %s, want OK", seconds(plan.total), item.Status, item.Detail)
	}
}
