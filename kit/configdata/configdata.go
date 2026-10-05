package configdata

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/app"
	fconfigdata "github.com/tjbdwanghaibo/roost-core/configdata"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	"github.com/tjbdwanghaibo/roost-core/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/metrics"

	"github.com/spf13/viper"
)

const (
	cfgKeyDir = "config_data.dir"
)

// Mod loads business configuration data into an immutable configdata Snapshot.
type Mod struct {
	store       *fconfigdata.Store
	dir         string
	metrics     *metrics.Registry
	lifecycle   *lifecycle.Registry
	unregisters []func()
}

func NewConfigDataMod() *Mod {
	return &Mod{}
}

func (m *Mod) Name() app.ModName { return mods.ModConfigData }

func (m *Mod) Init(cfg *viper.Viper) error {
	m.dir = "configs/data"
	if cfg != nil && cfg.IsSet(cfgKeyDir) {
		m.dir = cfg.GetString(cfgKeyDir)
	}
	m.store = fconfigdata.NewStore(fconfigdata.DefaultRegistry(), m.dir)
	return nil
}

func (m *Mod) Provide(r *app.Registry) error {
	if r == nil {
		return nil
	}
	var ok bool
	if m.metrics, ok = app.Lookup[*metrics.Registry](r, mods.ModMetrics); !ok || m.metrics == nil {
		return fmt.Errorf("configdata mod: capability %q not found", mods.ModMetrics)
	}
	if m.lifecycle, ok = app.Lookup[*lifecycle.Registry](r, mods.ModLifecycle); !ok || m.lifecycle == nil {
		return fmt.Errorf("configdata mod: capability %q not found", mods.ModLifecycle)
	}
	m.store.SetLifecycleRegistry(m.lifecycle)
	// Metrics come from the store's outcome report, not from a ReloadHook: a
	// reload that fails while building or validating never reaches a
	// listener (N07 C-O5), and a listener's AfterApply counted "ok" for a
	// reload a later listener then reverted. Each Load / Reload / Rollback is
	// reported exactly once. Labels are low-cardinality only; the operator's
	// free-text reason goes to the store's log line (C-O6).
	m.unregisters = append(m.unregisters, m.store.OnReloadOutcome(func(outcome fconfigdata.ReloadOutcome) {
		switch {
		case outcome.Rollback && outcome.Err == nil:
			m.metrics.IncCounter("configdata.rollback.total", metrics.Labels{"trigger": "operator"}, 1)
		case outcome.Rollback:
			// A failed operator rollback leaves the live generation alone;
			// the store logs it.
		case outcome.Err == nil:
			m.metrics.IncCounter("configdata.reload.total", metrics.Labels{"result": "ok"}, 1)
		default:
			m.metrics.IncCounter("configdata.reload.total", metrics.Labels{"result": "failed"}, 1)
			if outcome.Reverted() {
				m.metrics.IncCounter("configdata.rollback.total", metrics.Labels{"trigger": "apply_failed"}, 1)
			}
		}
		// The generation actually serving: after a reverted reload it is the
		// old one again, after a failed one it never moved.
		if outcome.Live != 0 {
			m.metrics.SetGauge("configdata.version", nil, int64(outcome.Live))
		}
	}))
	return r.Register(mods.ModConfigData, m.store)
}

func (m *Mod) Start() error {
	if m.store == nil {
		return fmt.Errorf("configdata mod: store is nil")
	}
	_, err := m.store.Load(fctx.BaseContext())
	return err
}

func (m *Mod) Stop() {
	_ = m.StopWithContext(context.Background())
}

func (m *Mod) StopWithContext(_ context.Context) error {
	if m == nil {
		return nil
	}
	for i := len(m.unregisters) - 1; i >= 0; i-- {
		if m.unregisters[i] != nil {
			m.unregisters[i]()
		}
	}
	m.unregisters = nil
	return nil
}

var _ app.ModStopperWithContext = (*Mod)(nil)

func (m *Mod) Store() *fconfigdata.Store {
	if m == nil {
		return nil
	}
	return m.store
}
