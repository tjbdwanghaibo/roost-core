package mods

import (
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

func TestRegisterAllPreflightPreventsPartialPublication(t *testing.T) {
	registry := app.NewRegistry(viper.New())
	if err := registry.Register("occupied", 1); err != nil {
		t.Fatal(err)
	}
	err := RegisterAll(registry,
		Capability{Name: "new_capability", Value: 2},
		Capability{Name: "occupied", Value: 3},
	)
	if err == nil {
		t.Fatal("RegisterAll succeeded with occupied capability")
	}
	if _, exists := registry.Get("new_capability"); exists {
		t.Fatal("RegisterAll published a partial capability set")
	}
}
