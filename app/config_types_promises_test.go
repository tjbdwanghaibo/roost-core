package app

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// RR-20261005-NC-190：配置值写错类型时启动校验必须报出键名，不能被 viper 的宽松转换静默读成
// 零值。旧行为：GetBool 把 YAML 里的 `on` / `yes`（yaml.v3 按 YAML 1.2 读成字符串）读成 false，
// `singleton.enabled: on` 让单实例锁静默关闭、`nats.reliable.enabled: yes` 静默关掉可靠总线；
// GetDuration 把不带单位的 `ttl: 15` 读成 15ns，四个 singleton 时长都不带单位时三条时间关系照样
// 成立，校验通过。

func yamlConfig(t *testing.T, body string) *viper.Viper {
	t.Helper()
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader("server_type: game\nsid: 2001\n" + body)); err != nil {
		t.Fatalf("read config: %v", err)
	}
	return cfg
}

func TestValidateServiceConfigRejectsABoolSwitchThatIsNotABool(t *testing.T) {
	for _, tc := range []struct {
		name, body, key string
	}{
		{"singleton_on", "singleton:\n  enabled: on\n  key_prefix: p\n", "singleton.enabled"},
		{"singleton_yes", "singleton:\n  enabled: yes\n  key_prefix: p\n", "singleton.enabled"},
		{"singleton_typo", "singleton:\n  enabled: ture\n  key_prefix: p\n", "singleton.enabled"},
		{"reliable_bus_yes", "nats:\n  reliable:\n    enabled: yes\n", "nats.reliable.enabled"},
		{"replica_set_check_off", "mongo:\n  require_replica_set: off\n", "mongo.require_replica_set"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateServiceConfig(yamlConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("ValidateServiceConfig = %v; want an error naming %s (viper reads the value as false and the switch is silently off)", err, tc.key)
			}
		})
	}
}

func TestValidateServiceConfigAcceptsTheBoolSpellingsItAlwaysAccepted(t *testing.T) {
	for _, value := range []string{"true", "false", "True", "FALSE", "\"true\"", "1", "0"} {
		cfg := yamlConfig(t, "nats:\n  reliable:\n    enabled: "+value+"\n")
		if err := ValidateServiceConfig(cfg); err != nil {
			t.Fatalf("nats.reliable.enabled: %s: ValidateServiceConfig = %v, want accepted", value, err)
		}
	}
}

func TestValidateServiceConfigRejectsADurationWithoutAUnit(t *testing.T) {
	for _, tc := range []struct {
		name, body, key string
	}{
		// 四个都不带单位：读成 15ns / 3ns / 5ns / 30ns，三条关系都成立。
		{"singleton_all_unitless", "singleton:\n  enabled: true\n  key_prefix: p\n  ttl: 15\n  renew_interval: 3\n  guard: 5\n  startup_wait: 30\n", "singleton.ttl"},
		{"singleton_quoted_number", "singleton:\n  enabled: true\n  key_prefix: p\n  ttl: \"15\"\n", "singleton.ttl"},
		{"singleton_unparseable", "singleton:\n  enabled: true\n  key_prefix: p\n  renew_interval: abc\n", "singleton.renew_interval"},
		{"remote_lock_ttl", "remote_entity:\n  lock_ttl: 15\n", "remote_entity.lock_ttl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateServiceConfig(yamlConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("ValidateServiceConfig = %v; want an error naming %s", err, tc.key)
			}
		})
	}
}

func TestValidateServiceConfigAcceptsDurationsWithUnits(t *testing.T) {
	cfg := yamlConfig(t, "singleton:\n  enabled: true\n  key_prefix: p\n  ttl: 15s\n  renew_interval: 3000ms\n  guard: 5s\n  startup_wait: 1m\nremote_entity:\n  lock_ttl: 15s\n")
	if err := ValidateServiceConfig(cfg); err != nil {
		t.Fatalf("ValidateServiceConfig = %v, want accepted", err)
	}
	// 代码里 Set 的 time.Duration 值（测试、默认值）照常可用。
	cfg = yamlConfig(t, "")
	cfg.Set("singleton.enabled", true)
	cfg.Set("singleton.key_prefix", "p")
	cfg.Set("singleton.ttl", 15*time.Second)
	if err := ValidateServiceConfig(cfg); err != nil {
		t.Fatalf("ValidateServiceConfig with a time.Duration value = %v, want accepted", err)
	}
}

// 守住 frameworkBoolKeys 与实际读取点同步（RR-20261005-NC-190，A4 扩到全部读取形式）：app、kit 与生成模板里
// 每个按布尔读取的键——GetBool、ConfigBool、ConfigReader.Bool——都要在清单里（singleton.enabled 由
// singletonSettings 自己严格读取）。新增开关忘了登记时这里报出键名。时长与整数键见
// config_strict_reads_promises_test.go。
func TestEveryFrameworkBoolSwitchIsCheckedStrictly(t *testing.T) {
	for _, read := range scanFrameworkConfigReads(t) {
		if read.kind == "bool" && !registeredConfigKey(read.kind, read.key) {
			t.Errorf("%s reads the switch %q as a bool; add it to frameworkBoolKeys so `%s: on` is refused instead of read as false", read.file, read.key, read.key)
		}
	}
}
