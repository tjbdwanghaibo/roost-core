package remoteentity

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

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
