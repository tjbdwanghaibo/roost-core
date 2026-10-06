package roost

// 维护者决定 A4 ① 收尾（2026-10-07）：生成工程里的代码同样“读配置只经声明”。
//
// 框架的守卫（app 的 TestFrameworkModsReadConfigOnlyThroughDeclarations / TestEveryDeclaredConfigFieldIsRead）只扫
// app 与 kit；生成进工程的业务代码不在范围内。修前（基线 82dfe672）新生成的 game-demo 有 16 处直接读 viper：
// game 读 activity.* / platform.* / sid / server_type / saga.* / dataengine.*，生成的 game/lifecycle/world_singleton.go
// 读 sid。其中 activity.* / platform.* 没有任何 game 的声明，`roost project doctor` 因此对 game 报一行 WARN
// （config-schema:game … no mod of game declares activity.groups_file, activity.key_prefix, platform.key_prefix,
// platform.payment_secret）。
//
// 这里用与框架守卫同一份实现（internal/configschema/guard.go）扫生成工程：
//   - 读了没声明：每个包里没有直接调 viper 读方法 / app.Config* 单键读取的地方；
//   - 声明了没读：带 config tag 的字段在整个工程里被读过（声明集中在 game/settings、由别的包读，所以按工程算）。

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/internal/configschema"
)

// projectConfigReadViolations 是 root 下生成工程的两类违例。
func projectConfigReadViolations(t *testing.T, root string) (reads, unread []string) {
	t.Helper()
	packages, err := configschema.GoPackages(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(packages) == 0 {
		t.Fatalf("%s has no Go packages", root)
	}
	reads, err = configschema.UndeclaredReads(packages, nil)
	if err != nil {
		t.Fatal(err)
	}
	var all []string
	for _, files := range packages {
		all = append(all, files...)
	}
	unread, err = configschema.UnreadFields(map[string][]string{root: all})
	if err != nil {
		t.Fatal(err)
	}
	return reads, unread
}

func TestGeneratedProjectsReadConfigOnlyThroughDeclarations(t *testing.T) {
	t.Parallel()
	roots := map[string]string{}
	for _, fixture := range []string{"game-demo", "saga", "configdata", "bare"} {
		roots[fixture] = copyOfNewProject(t, fixture)
	}
	// 覆盖面最大的形状：game 模板加上全部带配置的 Mod、player TCP 接入层与 saga。
	plan, err := renderProject(fullConfigManifest(t))
	if err != nil {
		t.Fatal(err)
	}
	full := t.TempDir()
	for rel, file := range plan {
		if strings.HasSuffix(rel, ".go") {
			writeProjectFile(t, full, rel, string(file.Body))
		}
	}
	roots["full-render"] = full
	for name, root := range roots {
		reads, unread := projectConfigReadViolations(t, root)
		for _, read := range reads {
			t.Errorf("%s: %s reads config outside a declaration; declare the key (on the service, see game/settings in game-demo) and read it with app.LoadConfig", name, strings.TrimPrefix(read, root+string(filepath.Separator)))
		}
		for _, field := range unread {
			t.Errorf("%s: %s declares a config key nothing in the project reads", name, strings.TrimPrefix(field, root+string(filepath.Separator)))
		}
	}
}

// 守卫能红：在 game-demo 的副本里加一处直接读 viper、一个没人读的声明字段，两类都被点名。
func TestGeneratedProjectConfigGuardCatchesDrift(t *testing.T) {
	t.Parallel()
	root := copyOfNewProject(t, "game-demo")
	settingsPath := filepath.Join(root, "game", "settings", "settings.go")
	raw, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatal(err)
	}
	edited := strings.Replace(string(raw), "\tPaymentSecret string `", "\tForgotten int `config:\"forgotten\"`\n\tPaymentSecret string `", 1)
	if edited == string(raw) {
		t.Fatal("the settings template changed shape; update this test")
	}
	if err := os.WriteFile(settingsPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	gmPath := filepath.Join(root, "internal", "service", "game", "gm.go")
	raw, err = os.ReadFile(gmPath)
	if err != nil {
		t.Fatal(err)
	}
	edited = strings.Replace(string(raw), "\tlocalSID := identity.Sid\n", "\tlocalSID := identity.Sid\n\t_ = registry.Config().GetString(\"gm.secret_knob\")\n", 1)
	if edited == string(raw) {
		t.Fatal("gm.go changed shape; update this test")
	}
	if err := os.WriteFile(gmPath, []byte(edited), 0o644); err != nil {
		t.Fatal(err)
	}
	reads, unread := projectConfigReadViolations(t, root)
	if len(reads) != 1 || !strings.Contains(reads[0], filepath.Join("internal", "service", "game", "gm.go")) {
		t.Errorf("reads = %v; want only the GetString added to gm.go", reads)
	}
	if len(unread) != 1 || !strings.Contains(unread[0], "Forgotten") {
		t.Errorf("unread = %v; want only Forgotten", unread)
	}
	// doctor 的 config-reads 检查是同一个守卫：对用户工程同样点名两处。
	if item := checkConfigReads(root); item.Status != StatusFail || !strings.Contains(item.Detail, "internal/service/game/gm.go:") || !strings.Contains(item.Detail, "Forgotten (config:\"forgotten\")") {
		t.Errorf("doctor config-reads = %s %q; want a FAIL naming gm.go and Forgotten", item.Status, item.Detail)
	}
	if item := checkConfigReads(copyOfNewProject(t, "game-demo")); item.Status != StatusOK {
		t.Errorf("doctor config-reads on a fresh game-demo = %s %q", item.Status, item.Detail)
	}
}

// 新生成的 game-demo，doctor 的 config-schema 检查零 WARN、零 FAIL：game 的业务代码读的 activity.* / platform.*
// 由 game 服务自己声明（game/settings），doctor 编译工程、经 --print-config-schema 读回这份声明。
// 去掉服务的 ConfigSchema 之后，同一行 WARN 回来（守卫能红）。
func TestDoctorReadsBusinessDeclarationsFromTheProcess(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles the generated game-demo")
	}
	t.Parallel()
	root := buildableGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	source, cleanup := processConfigSchemas(ctx, root)
	defer cleanup()
	items := checkConfigDeclarations(root, m, source)
	if len(items) < 10 {
		t.Fatalf("config-schema items = %d; the game-demo no longer has its framework services", len(items))
	}
	for _, item := range items {
		if item.Status != StatusOK {
			t.Errorf("%s: %s %s", item.Name, item.Status, item.Detail)
		}
		if item.Name == "config-schema:game" && !strings.Contains(item.Detail, "(4 business key(s))") {
			t.Errorf("config-schema:game = %q; want the four keys game/settings declares beyond the framework's (activity.key_prefix, activity.groups_file, platform.key_prefix, platform.payment_secret)", item.Detail)
		}
	}

	// 变异：服务不再声明它读的键。
	servicePath := filepath.Join(root, "internal", "service", "game", "service.go")
	raw, err := os.ReadFile(servicePath)
	if err != nil {
		t.Fatal(err)
	}
	mutated := regexp.MustCompile(`(?m)^func \(\*Service\) ConfigSchema\(\) app\.ConfigSchema \{ return settings\.Schema\(\) \}\n`).ReplaceAllString(string(raw), "")
	mutated = strings.Replace(mutated, "var _ app.ModConfigSchema = (*Service)(nil)\n", "var _ = settings.Schema\n", 1)
	if mutated == string(raw) {
		t.Fatal("service.go changed shape; update this test")
	}
	if err := os.WriteFile(servicePath, []byte(mutated), 0o644); err != nil {
		t.Fatal(err)
	}
	source, cleanup = processConfigSchemas(ctx, root)
	defer cleanup()
	for _, item := range checkConfigDeclarations(root, m, source) {
		if item.Name != "config-schema:game" {
			continue
		}
		if item.Status != StatusWarn || !strings.Contains(item.Detail, "no mod of game declares activity.groups_file, activity.key_prefix, platform.key_prefix, platform.payment_secret") {
			t.Errorf("without the service's declaration: config-schema:game = %s %q; want the WARN naming the four keys", item.Status, item.Detail)
		}
	}
}

// buildableGameDemo 是 game-demo 夹具的私有副本，roost-core replace 到本仓库并 go mod tidy，可以编译。
func buildableGameDemo(t *testing.T) string {
	t.Helper()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := copyOfNewProject(t, "game-demo")
	goModPath := filepath.Join(root, "go.mod")
	goMod, err := os.ReadFile(goModPath)
	if err != nil {
		t.Fatal(err)
	}
	replace := regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`)
	goMod = replace.ReplaceAll(goMod, nil)
	goMod = append(goMod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(goModPath, goMod, 0o644); err != nil {
		t.Fatal(err)
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "mod", "tidy")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go mod tidy in the game-demo copy: %v\n%s", err, out)
	}
	return root
}
