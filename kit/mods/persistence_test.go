package mods

import (
	"errors"
	"testing"

	"github.com/spf13/viper"
)

func TestResolvePersistenceEngineDefaultsToDataEngine(t *testing.T) {
	defaults, err := ResolvePersistenceEngine(viper.New())
	if err != nil {
		t.Fatal(err)
	}
	if defaults.Engine != PersistenceDataEngine || !defaults.DataEngineEnabled {
		t.Fatalf("defaults=%+v", defaults)
	}
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	selected, err := ResolvePersistenceEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Engine != PersistenceDataEngine || !selected.DataEngineEnabled {
		t.Fatalf("selected=%+v", selected)
	}
}

func TestResolvePersistenceEngineRejectsRemovedOrDisabledEngines(t *testing.T) {
	for _, configure := range []func(*viper.Viper){
		func(cfg *viper.Viper) { cfg.Set("persistence.engine", "checkpoint") },
		func(cfg *viper.Viper) { cfg.Set("persistence.engine", "other") },
		func(cfg *viper.Viper) { cfg.Set("checkpoint.enabled", true) },
		func(cfg *viper.Viper) { cfg.Set("checkpoint.enabled", false) },
		func(cfg *viper.Viper) { cfg.Set("dataengine.enabled", false) },
	} {
		cfg := viper.New()
		configure(cfg)
		if _, err := ResolvePersistenceEngine(cfg); !errors.Is(err, ErrPersistenceEngineSelection) {
			t.Fatalf("err=%v", err)
		}
	}
}

func TestResolvePersistenceEngineAcceptsLegacyDataEngineEnabledTrue(t *testing.T) {
	cfg := viper.New()
	cfg.Set("dataengine.enabled", true)
	selection, err := ResolvePersistenceEngine(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if selection.Engine != PersistenceDataEngine || !selection.DataEngineEnabled {
		t.Fatalf("selection=%+v", selection)
	}
}
