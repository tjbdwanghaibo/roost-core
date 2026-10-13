package mods

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

func TestRegisterAllPreflightPreventsPartialPublication(t *testing.T) {
	registry := app.NewRegistry(viper.New())
	if err := registry.Register("occupied", 1); err != nil {
		t.Fatal(err)
	}
	err := RegisterAll(registry,
		Capability{Name: "new_capability", Value: 2},
		Capability{Name: "occupied", Value: 3},
	)
	if err == nil {
		t.Fatal("RegisterAll succeeded with occupied capability")
	}
	if _, exists := registry.Get("new_capability"); exists {
		t.Fatal("RegisterAll published a partial capability set")
	}
}

// 批量注册拒绝 nil 注册表、空能力名、批内重名；Redis 能力缺失时报错并提示加 Mod。
func TestRegisterAllAndLookupsRefuseMissingInputs(t *testing.T) {
	if err := RegisterAll(nil, Capability{Name: "x", Value: 1}); err == nil || !strings.Contains(err.Error(), "nil registry") {
		t.Fatalf("RegisterAll(nil) = %v", err)
	}
	registry := app.NewRegistry(viper.New())
	if err := RegisterAll(registry, Capability{Name: "", Value: 1}); err == nil || !strings.Contains(err.Error(), "capability name is empty") {
		t.Fatalf("RegisterAll with an unnamed capability = %v", err)
	}
	if err := RegisterAll(registry, Capability{Name: "x", Value: 1}, Capability{Name: "x", Value: 2}); err == nil || !strings.Contains(err.Error(), `duplicate capability "x"`) {
		t.Fatalf("RegisterAll with a duplicate = %v", err)
	}
	if _, ok := registry.Get("x"); ok {
		t.Fatal("a refused batch published a capability")
	}
	if _, err := Redis(registry); err == nil || !strings.Contains(err.Error(), "add wiring/redis.NewRedisMod()") {
		t.Fatalf("Redis without the redis mod = %v", err)
	}
}

// C6（docs/feature/C6-DEFAULT-SERVICE-METRICS-2026-10-06.md）：生成工程默认给每个服务装上
// servicemetrics.NewMetricsReporter，service_metrics.enabled: false 在配置里把它关掉；未设置或 true 保持
// collaborator 给的 Reporter；写成 off 这类非布尔值启动报错并点名键（与其他框架开关一样严格，A4 ① 起由声明检查）。

func TestServiceMetricsSwitch(t *testing.T) {
	supplied := servicemetrics.NewRecorder()
	apply := func(t *testing.T, value any) (servicemetrics.Reporter, error) {
		t.Helper()
		cfg := viper.New()
		if value != nil {
			cfg.Set("service_metrics.enabled", value)
		}
		var settings ServiceMetricsConfig
		if err := app.LoadConfig(cfg, &settings); err != nil {
			return nil, err
		}
		var reporter servicemetrics.Reporter = supplied
		settings.ApplyServiceMetrics(&reporter)
		return reporter, nil
	}
	for _, tc := range []struct {
		name  string
		value any
		on    bool
	}{{"unset", nil, true}, {"true", true, true}, {"false", false, false}, {"false-string", "false", false}} {
		t.Run(tc.name, func(t *testing.T) {
			reporter, err := apply(t, tc.value)
			if err != nil {
				t.Fatal(err)
			}
			if on := reporter != nil; on != tc.on {
				t.Fatalf("service_metrics.enabled=%v left the reporter on=%v, want %v", tc.value, on, tc.on)
			}
		})
	}
	if _, err := apply(t, "off"); err == nil || !strings.Contains(err.Error(), "service_metrics.enabled") {
		t.Fatalf("service_metrics.enabled: off: %v, want a refusal naming the key", err)
	}
}

// A capability name is a global key in the registry, so a duplicate is a
// runtime failure that two separately-correct packages can cause together.
// The table exists to make that visible by reading — which only works if
// something checks it.
func TestEveryCapabilityNameIsUnique(t *testing.T) {
	seen := map[string]int{}
	for _, name := range All {
		if strings.TrimSpace(string(name)) == "" {
			t.Fatal("the table contains an empty capability name")
		}
		seen[string(name)]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("capability %q appears %d times in the table", name, count)
		}
	}
	if len(seen) != len(All) {
		t.Fatalf("the table holds %d entries but only %d distinct names", len(All), len(seen))
	}
}

// Every name is namespaced, so a service cannot collide with a roost-kit
// infrastructure capability — a service named "chat" and a hypothetical kit
// transport named "chat" would otherwise be the same key.
func TestEveryCapabilityNameIsNamespaced(t *testing.T) {
	for _, name := range All {
		if !strings.HasPrefix(string(name), "service.") {
			t.Fatalf("capability %q is not namespaced under \"service.\"", name)
		}
	}
}

func TestKeyPrefixRejectsWhitespace(t *testing.T) {
	for _, prefix := range []string{"roost mail", "roost\tmail"} {
		if err := CheckKeyPrefix("mail", prefix); err == nil || !strings.Contains(err.Error(), "mail.key_prefix") {
			t.Fatalf("CheckKeyPrefix(%q) = %v", prefix, err)
		}
	}
	if err := CheckKeyPrefix("mail", "roost:mail"); err != nil {
		t.Fatal(err)
	}
}

func TestClusterKeyPrefixUsesFirstRedisHashTag(t *testing.T) {
	for _, prefix := range []string{"plain", "{}", "{open", "{}:{valid}", "{valid}", "before}{valid}:suffix", "{{nested}"} {
		for _, cluster := range []bool{false, true} {
			t.Run(prefix+"/"+map[bool]string{false: "single", true: "cluster"}[cluster], func(t *testing.T) {
				var addrs []string
				if cluster {
					addrs = []string{"127.0.0.1:1"}
				}
				err := ValidateClusterKeyPrefix(addrs, "rank", prefix)
				invalid := cluster && (prefix == "plain" || prefix == "{}" || prefix == "{open" || prefix == "{}:{valid}")
				if (err != nil) != invalid {
					t.Fatalf("prefix=%q cluster=%v err=%v", prefix, cluster, err)
				}
			})
		}
	}
}

func TestPersistenceConfigDefaultsToDataEngine(t *testing.T) {
	var defaults PersistenceConfig
	if err := app.LoadConfig(viper.New(), &defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.Engine != PersistenceDataEngine || !defaults.DataEngineEnabled {
		t.Fatalf("defaults=%+v", defaults)
	}
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.enabled", true)
	var selected PersistenceConfig
	if err := app.LoadConfig(cfg, &selected); err != nil || selected.Engine != PersistenceDataEngine || !selected.DataEngineEnabled {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

func TestPersistenceConfigRejectsOtherOrDisabledEngines(t *testing.T) {
	for _, engine := range []string{"checkpoint", "other"} {
		cfg := viper.New()
		cfg.Set("persistence.engine", engine)
		var selection PersistenceConfig
		if err := app.LoadConfig(cfg, &selection); err == nil || !strings.Contains(err.Error(), "persistence.engine must be one of dataengine") {
			t.Fatalf("engine %s: err=%v", engine, err)
		}
	}
	cfg := viper.New()
	cfg.Set("dataengine.enabled", false)
	var selection PersistenceConfig
	if err := app.LoadConfig(cfg, &selection); !errors.Is(err, ErrPersistenceEngineSelection) {
		t.Fatalf("dataengine.enabled=false: err=%v", err)
	}
}
