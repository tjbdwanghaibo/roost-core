package remoteentity

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
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
