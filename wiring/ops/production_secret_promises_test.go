package ops

import (
	"github.com/spf13/viper"
	"testing"
)

func TestProductionRejectsExplicitlyAllowedDevToken(t *testing.T) {
	cfg := viper.New()
	cfg.Set("env", "production")
	cfg.Set("ops.enabled", true)
	cfg.Set("ops.admin_enabled", true)
	cfg.Set("ops.admin_token", "dev-test")
	cfg.Set("ops.allow_dev_token", true)
	if err := NewOpsMod().Init(cfg); err == nil {
		t.Fatal("production accepted dev admin token")
	}
}
func TestAdminTokenIsDeclaredSecret(t *testing.T) {
	for _, key := range NewOpsMod().ConfigSchema().Keys {
		if key.Name == "ops.admin_token" {
			if !key.Secret {
				t.Fatal("admin token lacks the secret declaration")
			}
			return
		}
	}
	t.Fatal("missing token declaration")
}

func TestDisabledProductionAdminDoesNotRequireUnusedToken(t *testing.T) {
	cfg := viper.New()
	cfg.Set("env", "production")
	cfg.Set("ops.enabled", true)
	if err := NewOpsMod().Init(cfg); err != nil {
		t.Fatalf("disabled admin requires unused token: %v", err)
	}
}
