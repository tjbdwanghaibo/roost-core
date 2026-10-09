package remoteentity

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

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
