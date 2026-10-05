package app

import (
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// 维护者决定 A4（2026-10-05）：框架里的布尔、时长、整数配置一律严格读取，ValidateServiceConfig 在
// 任何 Mod Init 之前按 frameworkBoolKeys / frameworkDurationKeys / frameworkIntKeys 检查一遍。
// 旧行为（NC-190 只修了 singleton、启动校验已列出的键、saga 步骤预算与 mods.Duration）：kit 其余约 80 处
// 直接用 cfg.GetDuration / GetInt / GetBool 读取，`nest.request_timeout: 3` 读成 3ns、`nest.worker_num: 8k`
// 读成 0（取默认 8）、`dataengine.wal.queue_capacity: 1.5` 截成 1、`syncbus.ack_wait: 30` 读成 30ns，
// 启动校验一律放行。

func TestValidateServiceConfigRejectsFrameworkValuesOfTheWrongType(t *testing.T) {
	for _, tc := range []struct {
		name, body, key string
	}{
		{"nest_request_timeout_unitless", "nest:\n  request_timeout: 3\n", "nest.request_timeout"},
		{"dataengine_outbox_lease_unitless", "dataengine:\n  outbox:\n    lease_duration: 30\n", "dataengine.outbox.lease_duration"},
		{"saga_lease_unitless", "saga:\n  lease_duration: \"15\"\n", "saga.lease_duration"},
		{"mail_send_ttl_unitless", "mail:\n  send_ttl: 60\n", "mail.send_ttl"},
		{"syncbus_ack_wait_unitless", "syncbus:\n  ack_wait: 30\n", "syncbus.ack_wait"},
		{"legacy_room_section", "room:\n  publish_timeout: 3\n", "room.publish_timeout"},
		{"player_tcp_idle_unitless", "player_access:\n  tcp:\n    idle_timeout: 90\n", "player_access.tcp.idle_timeout"},
		{"player_tcp_enabled_on", "player_access:\n  tcp:\n    enabled: on\n", "player_access.tcp.enabled"},
		{"project_rpc_call_timeout", "shop:\n  call_timeout: 5\n", "shop.call_timeout"},
		{"nest_worker_num_suffix", "nest:\n  worker_num: 8k\n", "nest.worker_num"},
		{"wal_queue_fraction", "dataengine:\n  wal:\n    queue_capacity: 1.5\n", "dataengine.wal.queue_capacity"},
		{"remote_writes_duration", "remote_entity:\n  max_concurrent_writes: 10s\n", "remote_entity.max_concurrent_writes"},
		{"redis_pool_word", "redis:\n  pool_size: many\n", "redis.pool_size"},
		{"etcd_lease_bool", "etcd:\n  lease_ttl: true\n", "etcd.lease_ttl"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateServiceConfig(yamlConfig(t, tc.body))
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("ValidateServiceConfig = %v; want an error naming %s", err, tc.key)
			}
		})
	}
}

func TestValidateServiceConfigAcceptsFrameworkValuesWrittenCorrectly(t *testing.T) {
	cfg := yamlConfig(t, `nest:
  request_timeout: 3s
  worker_num: 8
  max_delay: 24h
dataengine:
  wal:
    queue_capacity: 8192
    segment_bytes: 268435456
    group_commit_interval: 2ms
  projection:
    checkpoint_interval: 0
syncbus:
  ack_wait: 30s
  replicas: 3
player_access:
  tcp:
    enabled: false
    idle_timeout: 90s
    max_payload_bytes: 1048576
shop:
  call_timeout: 5s
etcd:
  lease_ttl: 10
redis:
  pool_size: "32"
mongo:
  max_pool_size: 1e2
`)
	if err := ValidateServiceConfig(cfg); err != nil {
		t.Fatalf("ValidateServiceConfig = %v, want accepted", err)
	}
}

func TestConfigIntAcceptsWholeNumbersOnly(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int64
		ok   bool
	}{
		{"8", 8, true}, {"\"8\"", 8, true}, {"-3", -3, true}, {"1e3", 1000, true}, {"0", 0, true},
		{"1.5", 0, false}, {"8k", 0, false}, {"10s", 0, false}, {"true", 0, false}, {"[1]", 0, false},
	} {
		cfg := yamlConfig(t, "n: "+tc.body+"\n")
		got, err := ConfigInt64(cfg, "n")
		if tc.ok && (err != nil || got != tc.want) {
			t.Errorf("n: %s: ConfigInt64 = %d, %v; want %d", tc.body, got, err, tc.want)
		}
		if !tc.ok && (err == nil || !strings.Contains(err.Error(), "n must be a whole number")) {
			t.Errorf("n: %s: ConfigInt64 = %d, %v; want an error naming n", tc.body, got, err)
		}
	}
	if got, err := ConfigInt64(yamlConfig(t, ""), "missing"); got != 0 || err != nil {
		t.Errorf("unset key: ConfigInt64 = %d, %v; want 0, nil", got, err)
	}
}

func TestConfigReaderReportsEveryBadKeyAtOnce(t *testing.T) {
	read := NewConfigReader(yamlConfig(t, "a: on\nb: 15\nc: 8k\nd: 2s\n"))
	read.Bool("a")
	read.Duration("b")
	read.Int("c")
	if got := read.Duration("d"); got.String() != "2s" {
		t.Fatalf("Duration(d) = %s, want 2s", got)
	}
	err := read.Err()
	for _, key := range []string{"a must be true or false", "b = 15 needs a unit", "c must be a whole number"} {
		if err == nil || !strings.Contains(err.Error(), key) {
			t.Errorf("Err() = %v; want it to contain %q", err, key)
		}
	}
	if err := NewConfigReader(nil).Err(); err != nil {
		t.Errorf("nil config: Err() = %v, want nil", err)
	}
}

// 守住登记与实际读取点同步：app、kit 与生成模板里每个按时长 / 整数读取的键都要在对应清单里。
func TestEveryFrameworkDurationAndIntKeyIsCheckedStrictly(t *testing.T) {
	for _, read := range scanFrameworkConfigReads(t) {
		if read.kind != "bool" && !registeredConfigKey(read.kind, read.key) {
			list := map[string]string{"duration": "frameworkDurationKeys", "int": "frameworkIntKeys"}[read.kind]
			t.Errorf("%s reads %q as a %s; add it to %s so a value of the wrong type is refused at startup", read.file, read.key, read.kind, list)
		}
	}
}

// 框架代码（app、kit）不再用 viper 的宽松 getter 读类型化的配置。例外：
//   - sid：ValidateServiceConfig 严格检查过，读取点遍布各 Mod；
//   - 生成文件（kit/service 的 *_rpc_assembly_gen.go）：生成工程要兼容已发布的 roost-core，读的 <service>.call_timeout
//     由 frameworkDurationSuffixes 在启动时检查；
//   - kit/redis/redis_mod.go 的三个整数键：kit/redis 正由维护者决定 A2 的驱动改造修改，A2 合入后再改成严格读取，
//     现在由 frameworkIntKeys 在启动时检查。
func TestFrameworkCodeDoesNotReadConfigLeniently(t *testing.T) {
	lenient := regexp.MustCompile(`\.(GetBool|GetDuration|GetInt|GetInt8|GetInt16|GetInt32|GetInt64|GetUint|GetUint8|GetUint16|GetUint32|GetUint64|GetFloat32|GetFloat64|GetSizeInBytes)\(([^)]*)\)`)
	pendingA2 := map[string]bool{
		"../kit/redis/redis_mod.go:redis.db": true, "../kit/redis/redis_mod.go:redis.pool_size": true, "../kit/redis/redis_mod.go:redis.min_idle_conns": true,
	}
	for _, file := range frameworkGoSources(t) {
		body := readSource(t, file)
		generated := strings.Contains(body, "// Code generated")
		for _, line := range strings.Split(body, "\n") {
			for _, match := range lenient.FindAllStringSubmatch(line, -1) {
				key := strings.Trim(match[2], `"`)
				switch {
				case strings.Contains(line, "Flags()."): // cobra 命令行参数，不是配置
				case key == "sid":
				case generated && matchesDurationSuffix(key):
				case pendingA2[file+":"+key]:
				default:
					t.Errorf("%s: %s reads config leniently (%s); use app.ConfigBool / ConfigDuration / ConfigInt or an app.ConfigReader", file, strings.TrimSpace(line), match[1])
				}
			}
		}
	}
}

type frameworkConfigRead struct{ file, kind, key string }

var (
	// ConfigReader 的变量按约定叫 read（flag.Duration 这类同名方法不是配置读取）。
	boolReadPattern     = regexp.MustCompile(`(?:GetBool|ConfigBool|read\.Bool)\((?:[\w.]+, )?"([^"]+)"\)`)
	durationReadPattern = regexp.MustCompile(`(?:GetDuration|ConfigDuration|read\.Duration|mods\.Duration|mods\.RequiredDuration)\((?:[\w.]+, )?"([^"]+)"`)
	intReadPattern      = regexp.MustCompile(`(?:GetU?[Ii]nt(?:8|16|32|64)?|ConfigInt(?:64)?|read\.Int(?:64)?)\((?:[\w.]+, )?"([^"]+)"\)`)
	// kit/syncbus 先按段优先级解析键名再读：cfgDuration(cfg, read, "ack_wait")。
	syncBusReadPattern = regexp.MustCompile(`cfg(Duration|Int|Int64)\(cfg, read, "([^"]+)"\)`)
	// 生成的 player TCP 接入层：const key = "player_access.tcp."，cfg.GetDuration(key + "idle_timeout")。
	playerTCPReadPattern = regexp.MustCompile(`Get(Bool|Duration|Int|Uint32)\(key \+ "([^"]+)"\)`)
)

// scanFrameworkConfigReads 找出 app、kit 与生成模板（player TCP 接入层、RPC 客户端 Mod）里按类型读取的配置键。
func scanFrameworkConfigReads(t *testing.T) []frameworkConfigRead {
	t.Helper()
	var reads []frameworkConfigRead
	files := append(frameworkGoSources(t), "../codegen/internal/roost/render_player_tcp.go", "../codegen/internal/servicerpc/template.go")
	for _, file := range files {
		body := readSource(t, file)
		for kind, pattern := range map[string]*regexp.Regexp{"bool": boolReadPattern, "duration": durationReadPattern, "int": intReadPattern} {
			for _, match := range pattern.FindAllStringSubmatch(body, -1) {
				reads = append(reads, frameworkConfigRead{file, kind, match[1]})
			}
		}
		for _, match := range syncBusReadPattern.FindAllStringSubmatch(body, -1) {
			kind := map[string]string{"Duration": "duration", "Int": "int", "Int64": "int"}[match[1]]
			for _, section := range syncBusSections {
				reads = append(reads, frameworkConfigRead{file, kind, section + "." + match[2]})
			}
		}
		for _, match := range playerTCPReadPattern.FindAllStringSubmatch(body, -1) {
			kind := map[string]string{"Bool": "bool", "Duration": "duration", "Int": "int", "Uint32": "int"}[match[1]]
			reads = append(reads, frameworkConfigRead{file, kind, "player_access.tcp." + match[2]})
		}
	}
	if len(reads) < 150 {
		t.Fatalf("found only %d typed config reads; the scan patterns no longer match the source", len(reads))
	}
	return reads
}

func registeredConfigKey(kind, key string) bool {
	if strings.HasPrefix(key, "singleton.") { // readSingletonSettings
		return true
	}
	switch kind {
	case "bool":
		return slices.Contains(frameworkBoolKeys, key)
	case "duration":
		if matchesDurationSuffix(key) || slices.Contains(frameworkDurationKeys, key) {
			return true
		}
		section, field, _ := strings.Cut(key, ".")
		return slices.Contains(syncBusSections, section) && slices.Contains(syncBusDurationFields, field)
	case "int":
		if slices.Contains(frameworkIntKeys, key) {
			return true
		}
		section, field, _ := strings.Cut(key, ".")
		return slices.Contains(syncBusSections, section) && slices.Contains(syncBusIntFields, field)
	}
	return false
}

func matchesDurationSuffix(key string) bool {
	for _, suffix := range frameworkDurationSuffixes {
		if strings.HasSuffix(key, suffix) {
			return true
		}
	}
	return false
}

// frameworkGoSources 返回 app 与 kit 的非测试 Go 源文件（相对仓库根的斜杠路径前加 ../，app 包内的不加）。
func frameworkGoSources(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, root := range []string{".", "../kit"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() && entry.Name() == "testdata" {
				return filepath.SkipDir
			}
			if entry.IsDir() || !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
				return nil
			}
			files = append(files, filepath.ToSlash(path))
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return files
}

func readSource(t *testing.T, path string) string {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(body)
}
