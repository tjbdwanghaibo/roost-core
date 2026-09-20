package configdata

import (
	"context"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/app"
	fconfigdata "github.com/tjbdwanghaibo/roost-core/configdata"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/lifecycle"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-kit/mods"

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
	m.unregisters = append(m.unregisters, m.store.AddReloadListener(fconfigdata.ReloadHook{
		HookName: "configdata.metrics",
		AfterApply: func(_ context.Context, event fconfigdata.ReloadEvent) error {
			m.metrics.IncCounter("configdata.reload.total", metrics.Labels{"result": "ok", "reason": event.Reason}, 1)
			m.metrics.SetGauge("configdata.version", nil, int64(event.New.Version))
			return nil
		},
		Rollback: func(_ context.Context, event fconfigdata.ReloadEvent, _ error) {
			m.metrics.IncCounter("configdata.reload.total", metrics.Labels{"result": "rollback", "reason": event.Reason}, 1)
			// The gauge was set to New.Version in AfterApply (or the apply
			// was aborted before ours ran); the store is back on Old — the
			// gauge must not keep advertising a rolled-back generation.
			// Old is never nil here: nil-Old reloads skip rollback callbacks.
			m.metrics.SetGauge("configdata.version", nil, int64(event.Old.Version))
		},
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
