package match

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"

	"github.com/tjbdwanghaibo/roost-core/kit/service/servicemetrics"
)

// Mod wires a match Store into an app and registers it as a capability.
//
// It takes no matchmaking policy. The store holds the queue and makes Commit
// atomic; deciding which waiting tickets form a match is the caller's job —
// a matchmaker in the game process reads Candidates, applies a Grouping and
// Commits. An earlier version accepted a Grouping here and in Config and
// then never executed it, so a project that injected its own policy got the
// caller's pairing anyway (U-0217, RR-20260916-05). A parameter the code
// cannot honour is worse than none.
type Mod struct {
	metrics servicemetrics.Reporter

	prefix    string
	ticketTTL time.Duration
	sweep     []Queue
	store     Store
}

// parseSweepQueues reads `match.sweep_queues`, a list of "mode:group_size" or
// "mode:group_size:partition" entries. Each is validated the way a queue is
// validated everywhere else, so a typo stops the process at Init instead of
// being swept as a queue that does not exist.
func parseSweepQueues(entries []string) ([]Queue, error) {
	queues := make([]Queue, 0, len(entries))
	for _, entry := range entries {
		parts := strings.Split(strings.TrimSpace(entry), ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("match mod: match.sweep_queues entry %q must be mode:group_size[:partition]", entry)
		}
		size, err := strconv.Atoi(parts[1])
		if err != nil {
			return nil, fmt.Errorf("match mod: match.sweep_queues entry %q has a non-numeric group size", entry)
		}
		queue := Queue{Mode: parts[0], GroupSize: size}
		if len(parts) == 3 {
			queue.Partition = parts[2]
		}
		if err := queue.Validate(); err != nil {
			return nil, fmt.Errorf("match mod: match.sweep_queues entry %q: %w", entry, err)
		}
		queues = append(queues, queue)
	}
	return queues, nil
}

// NewMod returns a match Mod. Matchmaking policy is not a parameter: drive
// it from a matchmaker with Candidates → Grouping → Commit.
func NewMod(reporter servicemetrics.Reporter) *Mod {
	return &Mod{metrics: reporter}
}

// Name implements app.Mod.
// Name implements app.Mod. It returns the generated CapabilityName, so the
// Mod's name and the capability it publishes are one fact rather than two.
func (m *Mod) Name() app.ModName { return CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// config 是 match.* 的声明（维护者决定 A4 ①）。
type config struct {
	mods.ServiceMetricsConfig
	Match struct {
		KeyPrefix   string        `config:"key_prefix" required:"true" example:"roost:{project}:match" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
		TicketTTL   time.Duration `config:"ticket_ttl" default:"5m" min:"1ns" example:"60s"`
		SweepQueues []string      `config:"sweep_queues" example:"[]"`
	} `config:"match"`
}

// ConfigSchema 声明 match.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	match:
//	  key_prefix: roost:match   # required, no default
//	  ticket_ttl: 2m            # optional, defaults to DefaultTicketTTL
func (m *Mod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("match mod: %w", err)
	}
	// service_metrics.enabled: false turns the collaborator's reporter off (C6).
	settings.ApplyServiceMetrics(&m.metrics)
	c := settings.Match
	if err := mods.CheckKeyPrefix("match", c.KeyPrefix); err != nil {
		return err
	}
	sweep, err := parseSweepQueues(c.SweepQueues)
	if err != nil {
		return err
	}
	m.prefix, m.ticketTTL, m.sweep = c.KeyPrefix, c.TicketTTL, sweep
	return nil
}

// Provide builds the store and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	// Ticket times are business time (D-L3, maintainer round 8): a ticket's
	// creation, its deadline and the "the longer you wait, the wider the
	// window" policy in the game's matchmaker all read the business clock, so
	// a test environment that moves time.logic_offset moves them together.
	// The offset only takes effect at start, so a wait is still real time.
	store, err := NewRedisStore(client, m.prefix, Config{
		TicketTTL: m.ticketTTL, Metrics: m.metrics, SweepQueues: m.sweep,
		Now: app.BusinessClock(r).Now,
	})
	if err != nil {
		return fmt.Errorf("match mod: %w", err)
	}
	m.store = store
	// Two capabilities, from one generated call so they cannot be published
	// apart: the interface consumers look up, and the owner-only name the
	// Server looks up to know this process holds the implementation.
	return mods.RegisterAll(r, OwnerCapabilities(store)...)
}

// Start implements app.Mod.
//
// Nothing is started here, and that is worth being explicit about: expired
// tickets are resolved by Sweep, which the caller drives. This package does
// not own a ticker, because "how often does this deployment sweep" is a
// deployment decision — and because a background goroutine that a Mod starts
// silently is one nobody can see failing.
func (m *Mod) Start() error { return nil }

// Stop implements app.Mod. Nothing to stop, for the same reason.
func (m *Mod) Stop() {}

var _ app.Mod = (*Mod)(nil)
