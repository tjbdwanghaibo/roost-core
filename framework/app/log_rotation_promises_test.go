package app

import (
	"github.com/spf13/viper"
	"strings"
	"testing"
)

func TestNegativeLogRotationIsRejected(t *testing.T) {
	cfg := viper.New()
	cfg.Set("server_type", "game")
	cfg.Set("sid", 7)
	cfg.Set("log.rotate_interval", "-1s")
	var value appConfig
	if err := LoadConfig(cfg, &value); err == nil {
		t.Fatal("negative rotation interval silently disabled file rotation")
	} else if !strings.Contains(err.Error(), "log.rotate_interval") {
		t.Fatalf("unrelated config failure: %v", err)
	}
}
