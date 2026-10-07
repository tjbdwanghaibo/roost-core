package dataengine

import (
	"strings"
	"testing"

	"github.com/spf13/viper"
)

// RR-20261007-05：由 Mod.Init 的正式配置路径拒绝短于 WAL 告警窗口的事务标记。
func TestTransactionReceiptRetentionExceedsWALWindow(t *testing.T) {
	for _, ttl := range []string{"1h", "24h", "24h500ms"} {
		t.Run(ttl, func(t *testing.T) {
			cfg := viper.New()
			cfg.Set("persistence.engine", "dataengine")
			cfg.Set("dataengine.transaction_receipt_ttl", ttl)
			if err := NewMod().Init(cfg); err == nil || !strings.Contains(err.Error(), "transaction_receipt_ttl") {
				t.Fatalf("transaction_receipt_ttl=%s accepted: %v", ttl, err)
			}
		})
	}
	for _, ttl := range []string{"24h1s", "720h"} {
		cfg := viper.New()
		cfg.Set("persistence.engine", "dataengine")
		cfg.Set("dataengine.transaction_receipt_ttl", ttl)
		if err := NewMod().Init(cfg); err != nil {
			t.Fatalf("valid transaction_receipt_ttl=%s: %v", ttl, err)
		}
	}
}
