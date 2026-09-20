package account

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

// The generated RPC glue is the same template in every roost-service package;
// its three refusals are pinned once, here. A process that holds only a
// client to Accounts must not be allowed to serve Accounts (it would forward
// every request to itself), a process with neither must be told what to add,
// and a negative call timeout is a mistake, not a default.
func TestGeneratedServerInitRefusesAClientOnlyProcessAndNamesTheMissingMod(t *testing.T) {
	empty := app.NewRegistry(viper.New())
	err := NewServer().Init(empty)
	if err == nil || !strings.Contains(err.Error(), `capability "`+string(LocalCapabilityName)+`" not found; add account.NewMod()`) {
		t.Fatalf("Init on an empty registry = %v", err)
	}

	clientOnly := app.NewRegistry(viper.New())
	if err := clientOnly.Register(CapabilityName, &Service{}); err != nil {
		t.Fatal(err)
	}
	err = NewServer().Init(clientOnly)
	if err == nil || !strings.Contains(err.Error(), "is published but") || !strings.Contains(err.Error(), "forward every request to itself") {
		t.Fatalf("Init on a client-only registry = %v", err)
	}
}

func TestGeneratedClientModRefusesANegativeCallTimeout(t *testing.T) {
	cfg := viper.New()
	cfg.Set("account.call_timeout", -time.Second)
	err := NewClientMod().Init(cfg)
	if err == nil || !strings.Contains(err.Error(), "account.call_timeout must not be negative, got -1s") {
		t.Fatalf("Init with a negative timeout = %v", err)
	}
	cfg.Set("account.call_timeout", 0)
	if err := NewClientMod().Init(cfg); err != nil {
		t.Fatalf("zero means default and must be accepted: %v", err)
	}
}
