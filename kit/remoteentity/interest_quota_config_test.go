package remoteentity

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

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
