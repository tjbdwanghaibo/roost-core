package match

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-kit/mods"

	"github.com/tjbdwanghaibo/roost-kit/service/servicemetrics"
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

// Init reads configuration.
//
//	match:
//	  key_prefix: roost:match   # required, no default
//	  ticket_ttl: 2m            # optional, defaults to DefaultTicketTTL
func (m *Mod) Init(cfg *viper.Viper) error {
	prefix, err := mods.KeyPrefix(cfg, "match")
	if err != nil {
		return err
	}
	ttl, err := mods.Duration(cfg, "match.ticket_ttl", DefaultTicketTTL)
	if err != nil {
		return err
	}
	sweep, err := parseSweepQueues(cfg.GetStringSlice("match.sweep_queues"))
	if err != nil {
		return err
	}
	m.prefix, m.ticketTTL, m.sweep = prefix, ttl, sweep
	return nil
}

// Provide builds the store and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	store, err := NewRedisStore(client, m.prefix, Config{
		TicketTTL: m.ticketTTL, Metrics: m.metrics, SweepQueues: m.sweep,
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
