package match

import domain "github.com/tjbdwanghaibo/roost-core/service/match"

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
)

// Mod wires a match Store into an app and registers it as a capability.
//
// It takes no matchmaking policy. The store holds the queue and makes Commit
// atomic; deciding which waiting tickets form a match is the caller's job —
// a matchmaker in the game process reads Candidates, applies a Grouping and
// Commits. An earlier version accepted a Grouping here and in Config and
// then never executed it, so a project that injected its own policy got the
// caller's pairing anyway. A parameter the code
// cannot honour is worse than none.
type Mod struct {
	metrics servicemetrics.Reporter

	prefix    string
	ticketTTL time.Duration
	retention time.Duration
	sweep     []domain.Queue
	store     domain.Store
}

// parseSweepQueues reads `match.sweep_queues`, a list of "mode:size" or
// "mode:size:partition" entries. Each is validated the way a queue is
// validated everywhere else, so a typo stops the process at Init instead of
// being swept as a queue that does not exist.
//
// size 与 Queue.LogicalKey 的人数段同一种写法：固定人数队列写 GroupSize（"ranked:2:eu"）；弹性人数队列写
// "<MinSize>-<GroupSize>@<等待>"（"party:2-8@20s:eu"，等待是整秒的 Go 时长写法），所以条目可以照 Redis 键的 hash tag 抄。
// FillAfterSeconds 不短于 match.ticket_ttl 的弹性队列由 Provide 里的 NewStore 拒绝。
func parseSweepQueues(entries []string) ([]domain.Queue, error) {
	queues := make([]domain.Queue, 0, len(entries))
	for _, entry := range entries {
		parts := strings.Split(strings.TrimSpace(entry), ":")
		if len(parts) < 2 || len(parts) > 3 {
			return nil, fmt.Errorf("match mod: match.sweep_queues entry %q must be mode:size[:partition]", entry)
		}
		queue := domain.Queue{Mode: parts[0]}
		if err := parseQueueSize(parts[1], &queue); err != nil {
			return nil, fmt.Errorf("match mod: match.sweep_queues entry %q: %w", entry, err)
		}
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

// parseQueueSize 解析人数段："8" 是固定人数队列；"2-8@20s" 是 MinSize 2、GroupSize 8、FillAfterSeconds 20 的弹性队列。
// 等待必须是整秒（Queue 按秒记）；取值范围（MinSize、非负）由随后的 Queue.Validate 判定，这里只管形状。
func parseQueueSize(spec string, queue *domain.Queue) error {
	sizes, wait, flexible := strings.Cut(spec, "@")
	if !flexible {
		size, err := strconv.Atoi(spec)
		if err != nil {
			return fmt.Errorf("size %q is neither a group size nor min-max@wait", spec)
		}
		queue.GroupSize = size
		return nil
	}
	low, high, ranged := strings.Cut(sizes, "-")
	minSize, minErr := strconv.Atoi(low)
	maxSize, maxErr := strconv.Atoi(high)
	if !ranged || minErr != nil || maxErr != nil {
		return fmt.Errorf("flexible size %q must be min-max@wait, e.g. 2-8@20s", spec)
	}
	fillAfter, err := time.ParseDuration(wait)
	if err != nil {
		return fmt.Errorf("flexible size %q has an unreadable wait: %w", spec, err)
	}
	if fillAfter%time.Second != 0 {
		return fmt.Errorf("flexible size %q: the wait must be a whole number of seconds", spec)
	}
	queue.MinSize, queue.GroupSize, queue.FillAfterSeconds = minSize, maxSize, int64(fillAfter/time.Second)
	return nil
}

// NewMod returns a match Mod. Matchmaking policy is not a parameter: drive
// it from a matchmaker with Candidates → Grouping → Commit.
func NewMod(reporter servicemetrics.Reporter) *Mod {
	return &Mod{metrics: reporter}
}

// Name implements app.Mod.
// Name implements app.Mod. It returns the generated CapabilityName, so the
// Mod's name and the capability it publishes are one fact rather than two.
func (m *Mod) Name() app.ModName { return domain.CapabilityName }

// DependsOn implements app.ModDependencyProvider.
func (m *Mod) DependsOn() []app.ModName { return []app.ModName{mods.ModRedis} }

// config 是 match.* 的声明（维护者决定）。
type config struct {
	mods.ServiceMetricsConfig
	Match struct {
		KeyPrefix   string        `config:"key_prefix" required:"true" example:"roost:{project}:match" help:"Redis 键前缀：必填、没有缺省（缺省值在每套部署里都一样，共用一个 Redis 的两套部署会静默共享状态）"`
		TicketTTL   time.Duration `config:"ticket_ttl" default:"5m" min:"1s" example:"60s"`
		Retention   time.Duration `config:"retention" default:"10m" min:"1s" example:"10m" help:"已结束的票、比赛与排队请求记录的保留期；超期后读到不存在，同一票集合的 Commit 重放报 ErrTicketMissing"`
		SweepQueues []string      `config:"sweep_queues" example:"[]" help:"本进程后台过期扫描的队列，每条 mode:size[:partition]；size 是固定人数（ranked:2:eu）或弹性人数 min-max@wait（party:2-8@20s:eu），写法同 Queue.LogicalKey"`
	} `config:"match"`
}

// ConfigSchema 声明 match.* 与 service_metrics.enabled。
func (m *Mod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

// Init reads configuration.
//
//	match:
//	  key_prefix: roost:match   # required, no default
//	  ticket_ttl: 2m            # optional, defaults to DefaultTicketTTL
//	  retention: 10m            # optional, defaults to DefaultRetention
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
	m.prefix, m.ticketTTL, m.retention, m.sweep = c.KeyPrefix, c.TicketTTL, c.Retention, sweep
	return nil
}

// Provide builds the store and registers it.
func (m *Mod) Provide(r *app.Registry) error {
	client, err := mods.Redis(r)
	if err != nil {
		return err
	}
	// Ticket times are business time (maintainer decision): a ticket's
	// creation, its deadline and the "the longer you wait, the wider the
	// window" policy in the game's matchmaker all read the business clock, so
	// a test environment that moves time.logic_offset moves them together.
	// The offset only takes effect at start, so a wait is still real time.
	store, err := domain.NewRedisStore(client, m.prefix, domain.Config{
		TicketTTL: m.ticketTTL, Retention: m.retention, Metrics: m.metrics, SweepQueues: m.sweep,
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
