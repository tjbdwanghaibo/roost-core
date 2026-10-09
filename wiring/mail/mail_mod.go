package mail

import domain "github.com/tjbdwanghaibo/roost-core/service/mail"

import (
	"fmt"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// Mod wires a mail Service into an app and registers it as a capability.
//
// The broadcast Deliverer is optional, and it is the one optional collaborator
// in this repository that is safe to leave out — because leaving it out does
// not weaken anything: Send refuses AudienceBroadcast outright when it is
// absent, rather than accepting the send and delivering to nobody. A
// deployment that only sends direct mail therefore needs no fanout, and one
// that forgot to wire it up finds out on its first broadcast instead of
// discovering later that a month of announcements went nowhere.
type Mod struct {
	broadcast domain.Deliverer
	metrics   servicemetrics.Reporter

	prefix     string
	sendTTL    time.Duration
	claimLease time.Duration
	service    *domain.Service
}

// NewMod returns a mail Mod. broadcast may be nil; see the type comment.
func NewMod(broadcast domain.Deliverer, reporter servicemetrics.Reporter) *Mod {
	return &Mod{broadcast: broadcast, metrics: reporter}
}

// Name implements app.Mod. It returns the generated CapabilityName rather
// than a second literal: the owning Mod and the client Mod must publish the
// same name for the two to be mutually exclusive, and the generated constant
// is where that name is derived from the interface's marker.
func (m *Mod) Name() app.ModName { return domain.CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// config 是 mail.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	Mail struct {
		KeyPrefix  string        `config:"key_prefix" required:"true" example:"roost:{project}:mail" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
		SendTTL    time.Duration `config:"send_ttl" required:"true" min:"1ns" example:"720h" help:"Required, with no default. It must exceed the longest client retry horizon: past it a retried\nsend is indistinguishable from a new one and the recipient gets the mail twice."`
		ClaimLease time.Duration `config:"claim_lease" default:"30s" min:"1s" example:"30s"`
	} `config:"mail"`
}

// ConfigSchema 声明 mail.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	mail:
//	  key_prefix: roost:mail   # required, no default
//	  send_ttl: 24h            # required; see below
//	  claim_lease: 30s         # optional, defaults to DefaultClaimLease
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("mail mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Mail
	if err := mods.CheckKeyPrefix("mail", c.KeyPrefix); err != nil {
		return err
	}
	m.prefix, m.sendTTL, m.claimLease = c.KeyPrefix, c.SendTTL, c.ClaimLease
	return nil
}

// Provide builds the service and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	// The clock is passed to both halves from one place, so the envelope
	// store's key ttls and the service's expiry comparisons cannot disagree
	// about what time it is. They did once, and the result was a mail that
	// read as live and had already been evicted.
	//
	// 邮件过期和领取预约截止都使用业务单调钟（D-L3）；Redis 物理 TTL 按剩余
	// 时长设置，不能把业务时间戳直接当成系统绝对过期时间。
	now := app.BusinessClock(r).Now
	stores, err := domain.NewRedisStores(client, domain.RedisConfig{
		Prefix: m.prefix, SendTTL: m.sendTTL, Now: now,
	})
	if err != nil {
		return fmt.Errorf("mail mod: %w", err)
	}
	service, err := domain.New(domain.Config{
		Envelopes: stores.Envelopes, Mailboxes: stores.Mailboxes, Sends: stores.Sends,
		Broadcast: m.broadcast, ClaimLease: m.claimLease, Now: now, Metrics: m.metrics,
	})
	if err != nil {
		return fmt.Errorf("mail mod: %w", err)
	}
	m.service = service
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface every consumer looks up, and the owner-only name
	// the Server looks up to know this process holds the implementation.
	//
	// Both are the wrapped interface — NOT *Service. That is what lets a
	// caller be written once and deployed either way: ClientMod registers a
	// client under the same consumer-facing name, so app.Lookup[Mail] resolves
	// in both processes. A capability published as a concrete type binds every
	// consumer to "mail runs here", which is the opposite of the deployment
	// freedom this repository claims.
	return mods.RegisterAll(r, OwnerCapabilities(service)...)
}

// Start implements app.Mod. Nothing to start.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
