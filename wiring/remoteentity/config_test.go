package remoteentity

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	coreremote "github.com/tjbdwanghaibo/roost-core/framework/remoteentity"
)

// B2：remote_entity.cached_max_staleness 严格读取。不配置时为 0（core 取 snapshot_cache_ttl）；
// 带单位的时长原样交给 core；不带单位的数字、0、负数与解析不了的值在 Init 拒绝，错误点名配置项。
func TestCachedMaxStalenessConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value  any
		want   time.Duration
		reject bool
	}{
		{nil, 0, false},
		{"5s", 5 * time.Second, false},
		{"1m30s", 90 * time.Second, false},
		{15, 0, true},
		{"15", 0, true},
		{"0s", 0, true},
		{"-1s", 0, true},
		{"soon", 0, true},
	} {
		cfg := viper.New()
		if tc.value != nil {
			cfg.Set("remote_entity.cached_max_staleness", tc.value)
		}
		mod := NewRemoteEntityMod(1000)
		err := mod.Init(cfg)
		if tc.reject {
			if err == nil || !strings.Contains(err.Error(), "remote_entity.cached_max_staleness") {
				t.Fatalf("value=%v: Init err=%v, want a rejection naming the key", tc.value, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("value=%v: %v", tc.value, err)
		}
		if mod.cfg.CachedMaxStaleness != tc.want {
			t.Fatalf("value=%v: CachedMaxStaleness=%v, want %v", tc.value, mod.cfg.CachedMaxStaleness, tc.want)
		}
	}
}

func TestClusterRequiresNonEmptyLockHashTag(t *testing.T) {
	for _, key := range []string{"", "e", "e{", "e{}", "e{}{valid}"} {
		t.Run(key, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("redis.cluster_addrs", "127.0.0.1:17380")
			cfg.Set("remote_entity.lock_key", key)
			err := NewRemoteEntityMod(1000).Init(cfg)
			if err == nil || !strings.Contains(err.Error(), "hash tag") {
				t.Fatalf("cluster key %q accepted: %v", key, err)
			}
		})
	}
	for _, cluster := range []bool{false, true} {
		cfg := viper.New()
		key := "e"
		if cluster {
			cfg.Set("redis.cluster_addrs", "127.0.0.1:17380")
			key = "e{remote}"
		}
		cfg.Set("remote_entity.lock_key", key)
		mod := NewRemoteEntityMod(1000)
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		if mod.cfg.LockKey != key {
			t.Fatal("lock identity silently rewritten")
		}
	}
}

// A4 ①：0 本身有含义的键（写许可 0 = 不限、墓碑副本 0 = 关闭、每节点配额 0 = 按 subs 推出）不能用“0 取缺省”，
// 它们的声明写出 default；这个 default 必须与 core 的 DefaultConfig、Mirror 的停机预算常量相同，否则生成器写出、
// doctor 认可的缺省值与运行时实际用的分叉。

func TestRemoteEntityDeclaredDefaultsMatchCoreDefaults(t *testing.T) {
	defaults := coreremote.DefaultConfig()
	entity := NewRemoteEntityMod(1).ConfigSchema()
	mirror := NewRemoteMirrorMod(1).ConfigSchema()
	for _, tc := range []struct {
		name, key, want string
	}{
		{"entity", "remote_entity.max_concurrent_writes", strconv.Itoa(defaults.MaxConcurrentWrites)},
		{"entity", "remote_entity.snapshot_l2_tombstone_wait_replicas", strconv.Itoa(defaults.SnapshotL2TombstoneWaitReplicas)},
		{"entity", "remote_entity.snapshot_l2_tombstone_wait_timeout", defaults.SnapshotL2TombstoneWaitTimeout.String()},
		{"entity", "remote_entity.snapshot_l2_tombstone_wait_timeout max", coreremote.MaxSnapshotL2TombstoneWaitTimeout.String()},
		{"mirror", "remote_entity.mirror.shutdown_timeout", defaultMirrorShutdownTimeout.String()},
	} {
		schema := entity
		if tc.name == "mirror" {
			schema = mirror
		}
		name, wantMax := tc.key, false
		if n := len(name); n > 4 && name[n-4:] == " max" {
			name, wantMax = name[:n-4], true
		}
		key, ok := schema.Lookup(name)
		got := key.Default
		if wantMax {
			got = key.Max
		}
		if !ok || got != tc.want {
			t.Errorf("%s: declared %q, core says %q", tc.key, got, tc.want)
		}
	}
	if defaults.SnapshotInterestPerConsumer != 0 {
		t.Errorf("DefaultConfig().SnapshotInterestPerConsumer = %d; the declaration leaves it 0 (derived from subs)", defaults.SnapshotInterestPerConsumer)
	}
}

// O4（docs/feature/MIRROR-STEP-4-AND-O4-2026-10-06.md）：remote_entity.snapshot_interest_per_consumer 严格读取。
// 不配置时为 0（core 取 snapshot_interest_subs / 16）；超过 snapshot_interest_subs、负数与写错类型的值在
// Init 拒绝，错误点名配置项。
func TestInterestPerConsumerConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value  any
		subs   any
		want   int
		reject bool
	}{
		{nil, nil, 0, false},
		{4096, nil, 4096, false},
		{100, 100, 100, false},
		{101, 100, 0, true},
		{-1, nil, 0, true},
		{"4k", nil, 0, true},
	} {
		cfg := viper.New()
		if tc.value != nil {
			cfg.Set("remote_entity.snapshot_interest_per_consumer", tc.value)
		}
		if tc.subs != nil {
			cfg.Set("remote_entity.snapshot_interest_subs", tc.subs)
		}
		mod := NewRemoteEntityMod(1000)
		err := mod.Init(cfg)
		if tc.reject {
			if err == nil || !strings.Contains(err.Error(), "remote_entity.snapshot_interest_per_consumer") {
				t.Fatalf("value=%v subs=%v: Init err=%v, want a rejection naming the key", tc.value, tc.subs, err)
			}
			continue
		}
		if err != nil {
			t.Fatalf("value=%v: %v", tc.value, err)
		}
		if mod.cfg.SnapshotInterestPerConsumer != tc.want {
			t.Fatalf("value=%v: SnapshotInterestPerConsumer=%d, want %d", tc.value, mod.cfg.SnapshotInterestPerConsumer, tc.want)
		}
	}
}

// remote_entity.snapshot_l2_key_prefix 缺省为空，配置后原样交给 core；带空白或 Redis Cluster hash tag 的值在 Init 拒绝，错误点名配置项。
func TestSnapshotL2KeyPrefixConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value  any
		want   string
		reject bool
	}{{nil, "", false}, {"", "", false}, {"roost:game-a", "roost:game-a", false}, {"roost game", "", true}, {"{roost:game}", "", true}} {
		cfg := viper.New()
		if tc.value != nil {
			cfg.Set("remote_entity.snapshot_l2_key_prefix", tc.value)
		}
		mod := NewRemoteEntityMod(1000)
		err := mod.Init(cfg)
		if tc.reject {
			if err == nil || !strings.Contains(err.Error(), "remote_entity.snapshot_l2_key_prefix") {
				t.Fatalf("value=%v: Init err=%v, want a rejection naming the key", tc.value, err)
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if mod.cfg.SnapshotL2KeyPrefix != tc.want {
			t.Fatalf("value=%v: SnapshotL2KeyPrefix=%q, want %q", tc.value, mod.cfg.SnapshotL2KeyPrefix, tc.want)
		}
	}
}

// O-M6-3（docs/feature/MIRROR-M6-OBSERVATIONS-2026-10-06.md §3）：墓碑 WAIT 的两项配置严格读取，两个 Mod
// （RemoteEntityMod 与只读的 RemoteMirrorMod）共用同一读取函数。不配置时取 core 缺省（1 个副本、50ms）；
// 0 个副本关闭；负数、写错类型、非正超时在 Init 拒绝，错误点名配置项。
func TestTombstoneWaitConfiguration(t *testing.T) {
	for _, tc := range []struct {
		replicas, timeout any
		wantReplicas      int
		wantTimeout       time.Duration
		reject            string
	}{
		{nil, nil, 1, 50 * time.Millisecond, ""},
		{0, nil, 0, 50 * time.Millisecond, ""},
		{2, "200ms", 2, 200 * time.Millisecond, ""},
		{-1, nil, 0, 0, "remote_entity.snapshot_l2_tombstone_wait_replicas"},
		{"one", nil, 0, 0, "remote_entity.snapshot_l2_tombstone_wait_replicas"},
		{nil, "0s", 0, 0, "remote_entity.snapshot_l2_tombstone_wait_timeout"},
		{nil, 50, 0, 0, "remote_entity.snapshot_l2_tombstone_wait_timeout"},
		{nil, "2s", 0, 0, "remote_entity.snapshot_l2_tombstone_wait_timeout"},
	} {
		for _, mod := range []struct {
			name string
			init func(*viper.Viper) (int, time.Duration, error)
		}{
			{"remote_entity", func(cfg *viper.Viper) (int, time.Duration, error) {
				m := NewRemoteEntityMod(1000)
				err := m.Init(cfg)
				if m.cfg == nil {
					return 0, 0, err
				}
				return m.cfg.SnapshotL2TombstoneWaitReplicas, m.cfg.SnapshotL2TombstoneWaitTimeout, err
			}},
			{"mirror", func(cfg *viper.Viper) (int, time.Duration, error) {
				m := NewRemoteMirrorMod(1000)
				err := m.Init(cfg)
				if m.cfg == nil {
					return 0, 0, err
				}
				return m.cfg.SnapshotL2TombstoneWaitReplicas, m.cfg.SnapshotL2TombstoneWaitTimeout, err
			}},
		} {
			cfg := viper.New()
			if tc.replicas != nil {
				cfg.Set("remote_entity.snapshot_l2_tombstone_wait_replicas", tc.replicas)
			}
			if tc.timeout != nil {
				cfg.Set("remote_entity.snapshot_l2_tombstone_wait_timeout", tc.timeout)
			}
			replicas, timeout, err := mod.init(cfg)
			if tc.reject != "" {
				if err == nil || !strings.Contains(err.Error(), tc.reject) {
					t.Fatalf("%s replicas=%v timeout=%v: Init err=%v, want a rejection naming %s", mod.name, tc.replicas, tc.timeout, err, tc.reject)
				}
				continue
			}
			if err != nil {
				t.Fatalf("%s replicas=%v timeout=%v: %v", mod.name, tc.replicas, tc.timeout, err)
			}
			if replicas != tc.wantReplicas || timeout != tc.wantTimeout {
				t.Fatalf("%s replicas=%v timeout=%v: got (%d, %v), want (%d, %v)", mod.name, tc.replicas, tc.timeout, replicas, timeout, tc.wantReplicas, tc.wantTimeout)
			}
		}
	}
}

func TestWriteBudgetConfiguration(t *testing.T) {
	for _, tc := range []struct {
		value  any
		want   int
		reject bool
	}{{nil, 128, false}, {0, 0, false}, {64, 64, false}, {-1, 0, true}} {
		cfg := viper.New()
		if tc.value != nil {
			cfg.Set("remote_entity.max_concurrent_writes", tc.value)
		}
		mod := NewRemoteEntityMod(1000)
		err := mod.Init(cfg)
		if tc.reject {
			if err == nil {
				t.Fatal("negative write budget accepted")
			}
			continue
		}
		if err != nil {
			t.Fatal(err)
		}
		if mod.cfg.MaxConcurrentWrites != tc.want {
			t.Fatalf("value=%v got=%d want=%d", tc.value, mod.cfg.MaxConcurrentWrites, tc.want)
		}
	}
}
