package kit_test

// 维护者决定 A4（2026-10-05）：kit 的 Mod 在 Init 里严格读取类型化配置，写错类型时报错并点名键，而不是
// 读成 0 / 纳秒后静默取默认。旧行为：`remote_entity.lock_ttl: 15` 读成 15ns（> 0，原样生效，锁几乎立刻过期）、
// `remote_entity.snapshot_cache_entries: 10k` 读成 0 取默认、`saga.lease_duration: 15` 读成 15ns、
// `dataengine.outbox.workers: two` 读成 0 取默认 2、`syncbus.ack_wait: 30` 读成 30ns，Init 一律成功。
// App 启动时 ValidateServiceConfig 已先挡住这些键（app 包的用例）；这里守的是直接装配 Mod、不经 App 启动
// 校验的调用方（测试、工具、自定义入口）同样拿到错误。

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/kit/dataengine"
	"github.com/tjbdwanghaibo/roost-core/kit/remoteentity"
	"github.com/tjbdwanghaibo/roost-core/kit/saga"
	"github.com/tjbdwanghaibo/roost-core/kit/syncbus"
)

func TestKitModsRefuseConfigValuesOfTheWrongType(t *testing.T) {
	type initer interface{ Init(*viper.Viper) error }
	for _, tc := range []struct {
		name string
		mod  func() initer
		body string
		key  string
	}{
		{"remote_lock_ttl_unitless", func() initer { return remoteentity.NewRemoteEntityMod(1000) }, "remote_entity:\n  lock_ttl: 15\n", "remote_entity.lock_ttl"},
		{"remote_cache_entries_suffix", func() initer { return remoteentity.NewRemoteEntityMod(1000) }, "remote_entity:\n  snapshot_cache_entries: 10k\n", "remote_entity.snapshot_cache_entries"},
		{"saga_lease_unitless", func() initer { return saga.NewMod() }, "saga:\n  lease_duration: 15\n", "saga.lease_duration"},
		{"dataengine_outbox_workers_word", func() initer { return dataengine.NewMod() }, "dataengine:\n  outbox:\n    workers: two\n", "dataengine.outbox.workers"},
		{"dataengine_pipelined_async_yes", func() initer { return dataengine.NewMod() }, "nest:\n  pipelined:\n    async: yes\n", "nest.pipelined.async"},
		{"syncbus_ack_wait_unitless", func() initer { return syncbus.NewSyncBusMod(1000) }, "syncbus:\n  transport: jetstream\n  ack_wait: 30\n", "syncbus.ack_wait"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := viper.New()
			cfg.SetConfigType("yaml")
			if err := cfg.ReadConfig(strings.NewReader("sid: 1000\n" + tc.body)); err != nil {
				t.Fatal(err)
			}
			err := tc.mod().Init(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.key) {
				t.Fatalf("Init = %v; want an error naming %s", err, tc.key)
			}
		})
	}
}
