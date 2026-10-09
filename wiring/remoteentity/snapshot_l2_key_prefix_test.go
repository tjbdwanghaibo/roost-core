package remoteentity

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// RR-20260927-17：remote_entity.snapshot_l2_key_prefix 缺省为空（L2 键与旧版本相同），配置后原样交给 core；
// 带空白或 Redis Cluster hash tag 的值在 Init 拒绝，错误点名配置项。
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
