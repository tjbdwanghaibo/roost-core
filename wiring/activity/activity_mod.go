package activity

import domain "github.com/tjbdwanghaibo/roost-core/service/activity"

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitredis "github.com/tjbdwanghaibo/roost-core/wiring/redis"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
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
	groups           *domain.Groups

	service *domain.Service
}

// NewMod returns an activity Mod.
func NewMod(reporter servicemetrics.Reporter) *Mod {
	return &Mod{metrics: reporter}
}

// Name implements app.Mod.
func (m *Mod) Name() app.ModName { return domain.CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// config 是 activity.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	kitredis.ClusterConfig
	Activity struct {
		KeyPrefix        string        `config:"key_prefix" required:"true" example:"roost:{project}:activity" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
		ReservationTTL   time.Duration `config:"reservation_ttl" required:"true" min:"1ns" example:"30m" help:"Required, with no default. It must exceed the longest client retry horizon: past it a replayed\nprogress request is indistinguishable from a new one and the progress is applied twice."`
		GraceWindow      time.Duration `config:"grace_window" default:"60s" min:"1s" example:"60s"`
		DispatchAttempts int           `config:"dispatch_attempts" default:"5" min:"1" example:"5"`
		DispatchBackoff  time.Duration `config:"dispatch_backoff" default:"5s" min:"1ns" example:"5s"`
		GroupsFile       string        `config:"groups_file" required:"true" example:"configs/activity_groups.yaml" help:"活动组文件（C4）：game 服务在这些组里开窗口，本进程启动时校验、清扫它们"`
		SweepGroups      []string      `config:"sweep_groups" help:"另外要清扫的组；不写取 groups_file 里的全部组"`
	} `config:"activity"`
}

// ConfigSchema 声明 activity.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	activity:
//	  key_prefix: roost:activity   # required, no default
//	  reservation_ttl: 30m         # required; see below
//	  grace_window: 60s            # optional
//	  dispatch_attempts: 5         # optional
//	  dispatch_backoff: 5s         # optional
//	  groups_file: configs/activity_groups.yaml  # required; see below
//	  sweep_groups: [alliance-a]   # groups whose grace windows THIS process back-stops
//
// groups_file is the activity groups file the game servers read too
// (LoadGroupsFile, decision C4). It is required: a coordinator without it has
// nothing to check a window's expected set against, so Init refuses to start
// and names the key and the path the generator writes (the declaration's
// example, configs/activity_groups.yaml).
// It was optional until 2026-10-06 so projects generated before C4 kept
// starting; the maintainer dropped that compatibility (nothing is deployed
// yet, and one way to configure the coordinator is simpler than two).
//
// The file is validated here, so a coordinator never starts beside a group
// definition no window could open with, and an empty sweep_groups means
// "every group in the file" — the group ids are then written once, in that
// file, instead of again in this config. An explicit sweep_groups still
// decides, for a deployment that splits the back-stop across replicas.
// OpenActivity checks every window against the file: the group must be in it
// and every expected game a member of that group (RR-20261006-17).
//
// Redis Cluster requires a common non-empty hash tag in key_prefix, for
// example {roost:activity}; dispatch records and their owed index share a CAS.
// Changing an existing prefix requires a separate data migration.
//
// These keys were under `global:` while the service lived in that package.
// The prefix may still point at the same root — each service owning its own
// keyspace setting is the point, not that the keyspaces have to differ.
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("activity mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Activity
	if err := mods.CheckKeyPrefix("activity", c.KeyPrefix); err != nil {
		return err
	}
	if err := mods.ValidateClusterKeyPrefix(settings.ClusterAddrs, "activity", c.KeyPrefix); err != nil {
		return err
	}
	groups, err := domain.LoadGroupsFile(c.GroupsFile)
	if err != nil {
		return fmt.Errorf("activity mod: activity.groups_file: %w", err)
	}
	m.prefix, m.reservationTTL = c.KeyPrefix, c.ReservationTTL
	m.graceWindow, m.dispatchAttempts, m.dispatchBackoff = c.GraceWindow, c.DispatchAttempts, c.DispatchBackoff
	m.groups = &groups
	m.sweepGroups = c.SweepGroups
	if len(m.sweepGroups) == 0 {
		m.sweepGroups = groups.IDs()
	}
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	stores, err := domain.NewRedisStores(client, m.prefix, m.reservationTTL)
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
func (m *Mod) newService(stores domain.RedisStores, now func() time.Time) (*domain.Service, error) {
	return domain.New(domain.Config{
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
