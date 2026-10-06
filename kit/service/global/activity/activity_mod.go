package activity

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Mod wires the activity coordination service into an app and registers it as
// a capability.
//
// It was half of global.Mod. Splitting it out gave it something it did not
// have there: a Mod of its own, so "what this Mod is called" and "what it
// publishes" are one fact. In global.Mod the activity capability was a literal
// on its own line — the comment there called it out as the exception — because
// a Mod has one name and was publishing two capabilities.
//
// It needs no policy collaborator: every decision is a compare-and-set against
// state, not a judgement about a caller.
type Mod struct {
	metrics servicemetrics.Reporter

	prefix           string
	reservationTTL   time.Duration
	graceWindow      time.Duration
	dispatchAttempts int
	dispatchBackoff  time.Duration
	sweepGroups      []string
	groups           *Groups

	service *Service
}

// NewMod returns an activity Mod.
func NewMod(reporter servicemetrics.Reporter) *Mod {
	return &Mod{metrics: reporter}
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// Init reads configuration.
//
//	activity:
//	  key_prefix: roost:activity   # required, no default
//	  reservation_ttl: 30m         # required; see below
//	  grace_window: 60s            # optional
//	  dispatch_attempts: 5         # optional
//	  dispatch_backoff: 5s         # optional
//	  groups_file: configs/activity_groups.yaml  # optional; see below
//	  sweep_groups: [alliance-a]   # groups whose grace windows THIS process back-stops
//
// groups_file is the activity groups file the game servers read too
// (LoadGroupsFile, decision C4). When it is set it is validated here, so a
// coordinator never starts beside a group definition no window could open
// with, and an empty sweep_groups means "every group in the file" — the group
// ids are then written once, in that file, instead of again in this config.
// An explicit sweep_groups still decides, for a deployment that splits the
// back-stop across replicas. With the file set, OpenActivity also checks every
// window against it: the group must be in the file and every expected game a
// member of that group (RR-20261006-17). Without it, nothing about groups is
// checked, as before C4 — the setting is optional so projects generated before
// the file existed keep working unchanged; projects generated since write it.
//
// Redis Cluster requires a common non-empty hash tag in key_prefix, for
// example {roost:activity}; dispatch records and their owed index share a CAS.
// Changing an existing prefix requires a separate data migration.
//
// These keys were under `global:` while the service lived in that package.
// The prefix may still point at the same root — each service owning its own
// keyspace setting is the point, not that the keyspaces have to differ.
func (m *Mod) Init(cfg *viper.Viper) error {
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	if err := mods.ServiceMetrics(cfg, &m.metrics); err != nil {
		return err
	}
	prefix, err := mods.KeyPrefix(cfg, "activity")
	if err != nil {
		return err
	}
	if err := mods.ValidateClusterKeyPrefix(cfg, "activity", prefix); err != nil {
		return err
	}
	// Required, with no default. It must exceed the longest client retry
	// horizon: past it a replayed progress request is indistinguishable from a
	// new one and the progress is applied twice. That horizon belongs to the
	// caller's transport, so this service cannot pick it.
	reservationTTL, err := mods.RequiredDuration(cfg, "activity.reservation_ttl")
	if err != nil {
		return err
	}
	graceWindow, err := mods.Duration(cfg, "activity.grace_window", DefaultGraceWindow)
	if err != nil {
		return err
	}
	dispatchBackoff, err := mods.Duration(cfg, "activity.dispatch_backoff", DefaultDispatchBackoff)
	if err != nil {
		return err
	}
	dispatchAttempts := DefaultDispatchAttempts
	if cfg.IsSet("activity.dispatch_attempts") {
		dispatchAttempts, err = app.ConfigInt(cfg, "activity.dispatch_attempts")
		if err != nil {
			return fmt.Errorf("activity mod: %w", err)
		}
		if dispatchAttempts <= 0 {
			return fmt.Errorf("activity mod: activity.dispatch_attempts must be positive, got %d", dispatchAttempts)
		}
	}
	m.prefix, m.reservationTTL = prefix, reservationTTL
	m.graceWindow, m.dispatchAttempts, m.dispatchBackoff = graceWindow, dispatchAttempts, dispatchBackoff
	m.sweepGroups = nil
	for _, group := range cfg.GetStringSlice("activity.sweep_groups") {
		if group = strings.TrimSpace(group); group != "" {
			m.sweepGroups = append(m.sweepGroups, group)
		}
	}
	m.groups = nil
	if path := strings.TrimSpace(cfg.GetString("activity.groups_file")); path != "" {
		groups, err := LoadGroupsFile(path)
		if err != nil {
			return fmt.Errorf("activity mod: activity.groups_file: %w", err)
		}
		m.groups = &groups
		if len(m.sweepGroups) == 0 {
			m.sweepGroups = groups.IDs()
		}
	}
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	stores, err := NewRedisStores(client, m.prefix, m.reservationTTL)
	if err != nil {
		return err
	}
	// 业务时钟（D-L3）：窗口截止、宽限、过期都与 game 一端的窗口 id 同钟，偏移非 0 时两端一起前移。
	service, err := m.newService(stores, app.BusinessClock(r).Now)
	if err != nil {
		return err
	}
	m.service = service
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface consumers look up, and the owner-only name the
	// Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(service)...)
}

// newService builds the Service from what Init read. It is Provide without the
// Redis lookup, so a test can wire the same configuration over memory stores.
func (m *Mod) newService(stores RedisStores, now func() time.Time) (*Service, error) {
	return New(Config{
		Activities: stores.Activities, Participants: stores.Participants,
		Ledger: stores.Ledger, Audits: stores.Audits,
		Dispatches: stores.Dispatches, Windows: stores.Windows,
		GraceWindow: m.graceWindow, ReservationTTL: m.reservationTTL,
		DispatchBackoff: m.dispatchBackoff, DispatchMaxAttempts: m.dispatchAttempts,
		SweepGroups: m.sweepGroups, Groups: m.groups, Metrics: m.metrics,
		Now: now,
	})
}

// Start implements app.Mod.
//
// Nothing is started here. Expired activities are advanced and due dispatches
// retried by the Server's run hook, which runs in the process that OWNS this
// service — a Mod cannot own that loop, because a process that merely holds an
// activity client would then be advancing activities it does not own.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
