package remoteentity

import (
	"github.com/spf13/viper"
	"testing"
)

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
