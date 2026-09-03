package room

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/app"
	fctx "github.com/tjbdwanghaibo/roost-core/fctx"
	"github.com/tjbdwanghaibo/roost-core/health"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	fsyncbus "github.com/tjbdwanghaibo/roost-core/syncbus"
	"github.com/tjbdwanghaibo/roost-kit/mods"
	"log/slog"
	"strings"
	"time"

	"github.com/spf13/viper"
)

// Config lives under the "room" section. Deployments written before the
// package was renamed used "sync"; both are read so an existing config file
// keeps working, with "room" winning when a key is set in both.
const (
	roomConfigSection   = "room"
	legacyConfigSection = "sync"
)

func configKey(cfg *viper.Viper, key string) string {
	if cfg.IsSet(roomConfigSection + "." + key) {
		return roomConfigSection + "." + key
	}
	if cfg.IsSet(legacyConfigSection + "." + key) {
		slog.Warn("room mod: config section \"sync\" is deprecated, rename it to \"room\"", "key", key)
		return legacyConfigSection + "." + key
	}
	return roomConfigSection + "." + key
}

func cfgGetString(cfg *viper.Viper, key string) string { return cfg.GetString(configKey(cfg, key)) }
func cfgGetInt(cfg *viper.Viper, key string) int       { return cfg.GetInt(configKey(cfg, key)) }
func cfgGetInt64(cfg *viper.Viper, key string) int64   { return cfg.GetInt64(configKey(cfg, key)) }
func cfgGetBool(cfg *viper.Viper, key string) bool     { return cfg.GetBool(configKey(cfg, key)) }
func cfgGetDuration(cfg *viper.Viper, key string) time.Duration {
	return cfg.GetDuration(configKey(cfg, key))
}

// RoomMod implements app.Mod, providing ISyncBus over NATS.
// Depends on: "nats" (fnats.IClient).
type RoomMod struct {
	bus       fsyncbus.ISyncBus
	localSid  int32
	prefix    string
	transport string
	jsCfg     JetStreamSyncConfig
}

func NewRoomMod(localSid int32) *RoomMod {
	return &RoomMod{localSid: localSid}
}

func (m *RoomMod) Name() app.ModName { return mods.ModRoom }
func (m *RoomMod) Init(cfg *viper.Viper) error {
	if m.localSid == 0 {
		m.localSid = cfg.GetInt32("sid")
	}
	m.prefix = cfgGetString(cfg, "prefix")
	if m.prefix == "" {
		m.prefix = "roost.room"
	}
	m.transport = strings.ToLower(strings.TrimSpace(cfgGetString(cfg, "transport")))
	m.jsCfg = JetStreamSyncConfig{
		LocalSid:     m.localSid,
		Prefix:       m.prefix,
		Stream:       cfgGetString(cfg, "stream"),
		Storage:      parseJetStreamSyncStorage(cfgGetString(cfg, "storage")),
		AckWait:      cfgGetDuration(cfg, "ack_wait"),
		MaxDeliver:   cfgGetInt(cfg, "max_deliver"),
		StreamMaxAge: cfgGetDuration(cfg, "stream_max_age"),
		Duplicates:   cfgGetDuration(cfg, "duplicates"),
		Replicas:     cfgGetInt(cfg, "replicas"),
		MaxBytes:     cfgGetInt64(cfg, "max_bytes"),
		SetupTimeout: cfgGetDuration(cfg, "setup_timeout"),
		PublishTime:  cfgGetDuration(cfg, "publish_timeout"),
	}
	return nil
}

func (m *RoomMod) Provide(r *app.Registry) error {
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return fmt.Errorf("room mod: capability %q not found", mods.ModHealth)
	}
	if m.useJetStream() {
		js, ok := app.Lookup[fnats.IJetStream](r, mods.ModNatsJetStream)
		if !ok || js == nil {
			return fmt.Errorf("room mod: required capability %q not found", mods.ModNatsJetStream)
		}
		bus, err := NewJetStreamSyncBus(fctx.BaseContext(), js, m.jsCfg)
		if err != nil {
			return err
		}
		m.bus = bus
		m.registerHealth(healthReg, "jetstream")
		return r.Register(mods.ModRoom, m.bus)
	}
	client, ok := app.Lookup[fnats.IClient](r, mods.ModNats)
	if !ok {
		return fmt.Errorf("room mod: required capability %q not found", mods.ModNats)
	}
	m.bus = NewNatsSyncBus(client, m.localSid, m.prefix)
	m.registerHealth(healthReg, "nats")
	return r.Register(mods.ModRoom, m.bus)
}

func (m *RoomMod) DependsOn() []app.ModName {
	return []app.ModName{mods.ModNats}
}

func (m *RoomMod) Start() error {
	transport := "nats"
	if m.useJetStream() {
		transport = "jetstream"
	}
	slog.Info("room mod: started", "transport", transport)
	return nil
}

func (m *RoomMod) Stop() {
	if err := m.StopWithContext(fctx.BaseContext()); err != nil {
		slog.Warn("room mod: stop interrupted", "err", err)
	}
}

func (m *RoomMod) StopWithContext(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = fctx.BaseContext()
	}
	if stopper, ok := m.bus.(interface{ StopWithContext(context.Context) error }); ok {
		if err := stopper.StopWithContext(ctx); err != nil {
			return err
		}
	} else if stopper, ok := m.bus.(interface{ Stop() }); ok {
		stopper.Stop()
	}
	m.bus = nil
	slog.Info("room mod: stopped")
	return nil
}

func (m *RoomMod) useJetStream() bool {
	return m != nil && (m.transport == "jetstream" || m.transport == "js")
}

func (m *RoomMod) registerHealth(reg *health.Registry, transport string) {
	if reg == nil {
		return
	}
	reg.Register("sync", health.CheckerFunc(func(context.Context) health.Result {
		if m == nil || m.bus == nil {
			return health.Result{Status: health.StatusFail, Message: "bus not initialized"}
		}
		return health.Result{Status: health.StatusOK, Message: transport}
	}))
}

func parseJetStreamSyncStorage(value string) fnats.JetStreamStorage {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case string(fnats.JetStreamStorageMemory):
		return fnats.JetStreamStorageMemory
	case string(fnats.JetStreamStorageFile):
		return fnats.JetStreamStorageFile
	default:
		return ""
	}
}
