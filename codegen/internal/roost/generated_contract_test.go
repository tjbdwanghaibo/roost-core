package roost

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
)

const generatedConfigValidationTest = `package configcheck

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"{{MODULE}}/internal/bootstrap"
	kitremoteentity "github.com/tjbdwanghaibo/roost-core/wiring/remoteentity"
	coreremote "github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
	"gopkg.in/yaml.v3"
)

func TestA4GeneratedConfigsPassValidation(t *testing.T) {
	root := "../.."
	type config struct{ name, service, body string; prod bool }
	var configs []config
	service, _ := filepath.Glob(filepath.Join(root, "configs", "service", "config.*.yaml"))
	for _, path := range service {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		name := filepath.Base(path)
		svc := strings.TrimSuffix(strings.TrimPrefix(name, "config."), ".yaml")
		prod := strings.HasSuffix(svc, ".prod.example")
		configs = append(configs, config{name, strings.TrimSuffix(svc, ".prod.example"), string(body), prod})
	}
	secrets, _ := filepath.Glob(filepath.Join(root, "deploy", "k8s", "base", "secret.*.example.yaml"))
	for _, path := range secrets {
		body, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		var secret struct {
			StringData map[string]string ` + "`yaml:\"stringData\"`" + `
		}
		if err := yaml.Unmarshal(body, &secret); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		name := filepath.Base(path)
		svc := strings.TrimSuffix(strings.TrimPrefix(name, "secret."), ".example.yaml")
		configs = append(configs, config{name, svc, secret.StringData["config.yaml"], true})
	}
	if len(service) < 4 || len(secrets) < 2 {
		t.Fatalf("found %d service configs and %d secret examples; the project layout moved", len(service), len(secrets))
	}
	load := func(t *testing.T, c config) *viper.Viper {
		cfg := viper.New()
		cfg.SetConfigType("yaml")
		if err := cfg.ReadConfig(strings.NewReader(c.body)); err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		cfg.Set("server_type", c.service) // App.run 按命令设置
		return cfg
	}
	// A4 ①：每个服务按它在生成 bootstrap 里注册的全部 Mod 的声明检查（与 App 启动前、--check-config 相同）。
	process, err := bootstrap.New()
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range configs {
		t.Run(c.name, func(t *testing.T) {
			if err := process.CheckServiceConfig(app.ServiceName(c.service), load(t, c)); err != nil {
				t.Errorf("%s does not pass the declarations of %s's mods:\n%v", c.name, c.service, err)
			}
			if !c.prod {
				return
			}
			cfg := load(t, c)
			cfg.Set("env", "production")
			cfg.Set("ops.allow_public_addr", true)
			if err := process.CheckServiceConfig(app.ServiceName(c.service), cfg); err != nil {
				t.Errorf("%s with env: production is refused:\n%v", c.name, err)
			}
		})
		if strings.Contains(c.body, "\nremote_entity:\n") {
			t.Run(c.name+"/remote_entity", func(t *testing.T) { checkRemoteEntityKeys(t, c.name, load(t, c)) })
		}
	}
}

// checkRemoteEntityKeys：A8（收尾第 2 批）。生成的 remote_entity 段写出 B2 / O4 / O-M6-3 / Mirror 第 5 步的键，
// 取值等于 core DefaultConfig（或 kit 的缺省）的语义，并通过两个 Mod 的 Init（值域检查在 Init 里，
// ValidateServiceConfig 只查类型）。
func checkRemoteEntityKeys(t *testing.T, name string, cfg *viper.Viper) {
	for _, key := range []string{
		"remote_entity.cached_max_staleness",
		"remote_entity.snapshot_interest_per_consumer",
		"remote_entity.snapshot_l2_tombstone_wait_replicas",
		"remote_entity.snapshot_l2_tombstone_wait_timeout",
		"remote_entity.mirror.shutdown_timeout",
	} {
		if !cfg.IsSet(key) {
			t.Errorf("%s does not set %s", name, key)
		}
	}
	defaults := coreremote.DefaultConfig()
	if got := cfg.GetDuration("remote_entity.cached_max_staleness"); got != cfg.GetDuration("remote_entity.snapshot_cache_ttl") {
		t.Errorf("%s: cached_max_staleness = %v, want snapshot_cache_ttl (%v): unset it follows snapshot_cache_ttl", name, got, cfg.GetDuration("remote_entity.snapshot_cache_ttl"))
	}
	if got := cfg.GetInt("remote_entity.snapshot_interest_per_consumer"); got != defaults.SnapshotInterestPerConsumer {
		t.Errorf("%s: snapshot_interest_per_consumer = %d, want DefaultConfig %d", name, got, defaults.SnapshotInterestPerConsumer)
	}
	if got := cfg.GetInt("remote_entity.snapshot_l2_tombstone_wait_replicas"); got != defaults.SnapshotL2TombstoneWaitReplicas {
		t.Errorf("%s: snapshot_l2_tombstone_wait_replicas = %d, want DefaultConfig %d", name, got, defaults.SnapshotL2TombstoneWaitReplicas)
	}
	if got := cfg.GetDuration("remote_entity.snapshot_l2_tombstone_wait_timeout"); got != defaults.SnapshotL2TombstoneWaitTimeout {
		t.Errorf("%s: snapshot_l2_tombstone_wait_timeout = %v, want DefaultConfig %v", name, got, defaults.SnapshotL2TombstoneWaitTimeout)
	}
	if err := kitremoteentity.NewRemoteEntityMod(0).Init(cfg); err != nil {
		t.Errorf("%s: RemoteEntityMod.Init refuses the generated remote_entity section: %v", name, err)
	}
	mirror := kitremoteentity.NewRemoteMirrorMod(0)
	if err := mirror.Init(cfg); err != nil {
		t.Errorf("%s: RemoteMirrorMod.Init refuses the generated remote_entity section: %v", name, err)
	}
	unset := viper.New()
	unset.Set("sid", 1000)
	defaultMirror := kitremoteentity.NewRemoteMirrorMod(0)
	if err := defaultMirror.Init(unset); err != nil {
		t.Fatal(err)
	}
	if mirror.StopBudget() != defaultMirror.StopBudget() {
		t.Errorf("%s: mirror.shutdown_timeout = %v, want the kit default %v", name, mirror.StopBudget(), defaultMirror.StopBudget())
	}
}
`

func TestGeneratedConfigsPassStrictAndProductionValidation(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and runs a test inside the generated game-demo")
	}
	t.Parallel()
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	if override := os.Getenv("ROOST_A4_CORE_REPLACE"); override != "" {
		repo = override // 取修前证据时指向基线检出
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
	for rel, body := range map[string][]byte{
		"go.mod":                                 goMod,
		"internal/configcheck/a4_config_test.go": []byte(strings.ReplaceAll(generatedConfigValidationTest, "{{MODULE}}", projectModule(t, goMod))),
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, body, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	goName := "go"
	if runtime.GOOS == "windows" {
		goName += ".exe"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, filepath.Join(runtime.GOROOT(), "bin", goName), "test", "-mod=mod", "-count=1", "-v", "-run", "TestA4GeneratedConfigsPassValidation", "./internal/configcheck/")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated configs against app.ValidateServiceConfig: %v\n%s", err, out)
	}
	if !strings.Contains(string(out), "--- PASS: TestA4GeneratedConfigsPassValidation") {
		t.Errorf("TestA4GeneratedConfigsPassValidation did not run and pass:\n%s", out)
	}
}

// projectModule is the module path a generated go.mod declares.
func projectModule(t *testing.T, goMod []byte) string {
	t.Helper()
	match := regexp.MustCompile(`(?m)^module\s+(\S+)`).FindSubmatch(goMod)
	if match == nil {
		t.Fatal("generated go.mod has no module line")
	}
	return string(match[1])
}

const generatedCoreModule = "github.com/tjbdwanghaibo/roost-core"

func TestGeneratedGameDemoBuildsAndVetsAgainstThisCheckout(t *testing.T) {
	if testing.Short() {
		t.Skip("builds a whole generated project")
	}
	if runtime.GOOS == "windows" {
		// 生成形状与平台无关；windows-compatibility 作业的整包时长已经贴着 go test 的默认上限（RR-20260928-14）。
		t.Skip("the generated shape is platform independent; checked on the other runners")
	}
	t.Parallel()
	coreRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(filepath.Join(coreRoot, "go.mod")); err != nil || !strings.HasPrefix(string(raw), "module "+generatedCoreModule+"\n") {
		t.Fatalf("%s is not the roost-core module root (%v)", coreRoot, err)
	}
	root := copyOfNewProject(t, "game-demo")

	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.CommandContext(ctx, "go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOWORK=off", "GOFLAGS=-mod=mod")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("generated game-demo: go %s failed: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("mod", "edit", "-replace", generatedCoreModule+"="+coreRoot)
	run("build", "./...")
	run("vet", "./...")
}
