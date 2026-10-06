package mods

import (
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
)

func TestPersistenceConfigDefaultsToDataEngine(t *testing.T) {
	var defaults PersistenceConfig
	if err := app.LoadConfig(viper.New(), &defaults); err != nil {
		t.Fatal(err)
	}
	if defaults.Engine != PersistenceDataEngine || !defaults.DataEngineEnabled {
		t.Fatalf("defaults=%+v", defaults)
	}
	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("dataengine.enabled", true)
	var selected PersistenceConfig
	if err := app.LoadConfig(cfg, &selected); err != nil || selected.Engine != PersistenceDataEngine || !selected.DataEngineEnabled {
		t.Fatalf("selected=%+v err=%v", selected, err)
	}
}

func TestPersistenceConfigRejectsOtherOrDisabledEngines(t *testing.T) {
	for _, engine := range []string{"checkpoint", "other"} {
		cfg := viper.New()
		cfg.Set("persistence.engine", engine)
		var selection PersistenceConfig
		if err := app.LoadConfig(cfg, &selection); err == nil || !strings.Contains(err.Error(), "persistence.engine must be one of dataengine") {
			t.Fatalf("engine %s: err=%v", engine, err)
		}
	}
	cfg := viper.New()
	cfg.Set("dataengine.enabled", false)
	var selection PersistenceConfig
	if err := app.LoadConfig(cfg, &selection); !errors.Is(err, ErrPersistenceEngineSelection) {
		t.Fatalf("dataengine.enabled=false: err=%v", err)
	}
}
