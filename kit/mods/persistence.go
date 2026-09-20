package mods

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/viper"
)

const (
	PersistenceDataEngine = "dataengine"
)

var ErrPersistenceEngineSelection = errors.New("persistence: dataengine is the only supported engine")

type PersistenceSelection struct {
	Engine            string
	DataEngineEnabled bool
}

func ResolvePersistenceEngine(cfg *viper.Viper) (PersistenceSelection, error) {
	if cfg == nil {
		cfg = viper.New()
	}
	engine := strings.ToLower(strings.TrimSpace(cfg.GetString("persistence.engine")))
	if engine == "" {
		engine = PersistenceDataEngine
	}
	if engine != PersistenceDataEngine {
		return PersistenceSelection{}, fmt.Errorf("%w: unsupported engine %q", ErrPersistenceEngineSelection, engine)
	}
	if cfg.IsSet("checkpoint.enabled") {
		return PersistenceSelection{}, fmt.Errorf("%w: checkpoint.enabled was removed", ErrPersistenceEngineSelection)
	}
	if cfg.IsSet("dataengine.enabled") && !cfg.GetBool("dataengine.enabled") {
		return PersistenceSelection{}, fmt.Errorf("%w: dataengine.enabled=false", ErrPersistenceEngineSelection)
	}
	return PersistenceSelection{Engine: PersistenceDataEngine, DataEngineEnabled: true}, nil
}
