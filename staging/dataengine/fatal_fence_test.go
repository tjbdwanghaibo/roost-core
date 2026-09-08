package dataengine

import (
	"errors"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"github.com/tjbdwanghaibo/roost-kit/mods"
)

func TestModFatalFencesNestAndSignalsApplication(t *testing.T) {
	registry := app.NewRegistry(viper.New())
	engine := corenest.NewEngine()
	if err := registry.Register(mods.ModNest, engine); err != nil {
		t.Fatal(err)
	}
	mod := &Mod{registry: registry}
	cause := errors.New("indeterminate physical write")
	mod.onFatal(cause)
	if err := engine.FenceError(); !errors.Is(err, corenest.ErrNestFenced) || !errors.Is(err, cause) {
		t.Fatalf("fence=%v", err)
	}
	failure := app.MustLookup[*app.RuntimeFailure](registry, app.ModRuntimeFailure)
	if err := failure.Err(); !errors.Is(err, cause) {
		t.Fatalf("runtime failure=%v", err)
	}
}
