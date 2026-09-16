package mail

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	kitmods "github.com/tjbdwanghaibo/roost-kit/mods"
)

func expectWiringErr(t *testing.T, err error, want string) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("error = %v, want it to contain %q", err, want)
	}
}

// U-0103 (C2, B-24 尾项): the generated transport wiring refuses each
// mis-assembled process by name. The same template serves every service in
// this repository, so pinning it once here pins the generator's contract.
func TestGeneratedWiringRefusesEachMisassembledProcess(t *testing.T) {
	ctx := context.Background()
	service := newHarness(t).service

	expectWiringErr(t, RegisterHandlers(nil, service), "mail: bus is nil")
	expectWiringErr(t, RegisterHandlers(newFakeBus(), nil), "mail: service is nil")
	if _, err := NewBusClient(nil, "", 0); err == nil || !strings.Contains(err.Error(), "mail: bus is nil") {
		t.Fatalf("NewBusClient(nil) = %v", err)
	}
	var unconfigured *BusClient
	if _, err := unconfigured.Send(ctx, directTo(7)); err == nil || !strings.Contains(err.Error(), "client is not configured") {
		t.Fatalf("nil client Send = %v", err)
	}

	t.Run("server on a bus client forwards to itself", func(t *testing.T) {
		registry := app.NewRegistry(viper.New())
		if err := registry.Register(kitmods.ModBus, newFakeBus()); err != nil {
			t.Fatal(err)
		}
		client := NewClientMod()
		if err := client.Init(viper.New()); err != nil {
			t.Fatal(err)
		}
		if err := client.Provide(registry); err != nil {
			t.Fatal(err)
		}
		expectWiringErr(t, NewServer().Init(registry), "is published but")
	})
	t.Run("server without the local service", func(t *testing.T) {
		registry := app.NewRegistry(viper.New())
		expectWiringErr(t, NewServer().Init(registry), "add mail.NewMod()")
	})
	t.Run("server without a bus", func(t *testing.T) {
		registry := app.NewRegistry(viper.New())
		if err := registry.Register(LocalCapabilityName, service); err != nil {
			t.Fatal(err)
		}
		expectWiringErr(t, NewServer().Init(registry), "needs a bus")
	})
	t.Run("client mod with a negative timeout", func(t *testing.T) {
		cfg := viper.New()
		cfg.Set("mail.call_timeout", -time.Second)
		expectWiringErr(t, NewClientMod().Init(cfg), "mail.call_timeout must not be negative")
	})
	t.Run("client mod without a bus", func(t *testing.T) {
		mod := NewClientMod()
		if err := mod.Init(viper.New()); err != nil {
			t.Fatal(err)
		}
		expectWiringErr(t, mod.Provide(app.NewRegistry(viper.New())), "needs a bus")
	})
}

// TestRedisStoresRefuseInvalidConfigAndEmptyIDs moved to roost-core/service/mail
// (redis_guards_promises_test.go) with the stores it asserts about (M-07).
