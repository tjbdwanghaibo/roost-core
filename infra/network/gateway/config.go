package gateway

import (
	"context"
	"errors"
	"fmt"
	"time"
)

// Config 约束独立 Gate/Game 接入的驻留资源；与 Nest worker 或客户端同步频率无关。
type Config struct {
	Namespace            string
	MaxBindings          int
	MaxPayloadBytes      int
	ForwardWorkers       int
	MaxForwardRequests   int
	PerGameInFlight      int
	MaxForwardBytes      int64
	PerBindingRequests   int
	ControlWorkers       int
	MaxControlRequests   int
	MaxControlBytes      int64
	SubscriptionMessages int
	SubscriptionBytes    int
	RequestTimeout       time.Duration
	ClockSkew            time.Duration
	Lease                time.Duration
	RenewInterval        time.Duration
	MaxTombstones        int
	TombstoneTTL         time.Duration
	Outbound             OutboundLimits
	MaxBroadcastPlayers  int
	MaxBroadcastRecords  int
	BroadcastTTL         time.Duration
	BroadcastWorkers     int
}

type OutboundLimits struct {
	MaxSessions     int
	QueueEntries    int
	MaxPacketBytes  int
	PerSessionBytes int
	ResidentBytes   int64
	MaxAge          time.Duration
	SendTimeout     time.Duration
}

func DefaultConfig() Config {
	return Config{
		Namespace: "roost.access", MaxBindings: 4096, MaxPayloadBytes: 512 << 10,
		ForwardWorkers: 256, MaxForwardRequests: 4096, PerGameInFlight: 128, MaxForwardBytes: 64 << 20, PerBindingRequests: 8,
		ControlWorkers: 64, MaxControlRequests: 4096, MaxControlBytes: 16 << 20, SubscriptionMessages: 1024, SubscriptionBytes: 16 << 20,
		RequestTimeout: 3 * time.Second, ClockSkew: 5 * time.Millisecond, Lease: 15 * time.Second, RenewInterval: 5 * time.Second,
		MaxTombstones: 8192, TombstoneTTL: 30 * time.Second,
		Outbound:            OutboundLimits{MaxSessions: 4096, QueueEntries: 32, MaxPacketBytes: 1 << 20, PerSessionBytes: 2 << 20, ResidentBytes: 128 << 20, MaxAge: 250 * time.Millisecond, SendTimeout: 250 * time.Millisecond},
		MaxBroadcastPlayers: 4096, MaxBroadcastRecords: 4096, BroadcastTTL: 30 * time.Second, BroadcastWorkers: 8,
	}
}

func (config Config) Validate() error {
	var problems []error
	if err := ValidateNamespace(config.Namespace); err != nil {
		problems = append(problems, fmt.Errorf("gateway: namespace: %w", err))
	}
	for _, limit := range []struct {
		name       string
		value, max int
	}{
		{"max_bindings", config.MaxBindings, 100000}, {"max_payload_bytes", config.MaxPayloadBytes, hardMaxPayload},
		{"forward_workers", config.ForwardWorkers, 4096}, {"max_forward_requests", config.MaxForwardRequests, 1000000},
		{"per_game_in_flight", config.PerGameInFlight, 4096},
		{"per_binding_requests", config.PerBindingRequests, 1024}, {"control_workers", config.ControlWorkers, 4096},
		{"max_control_requests", config.MaxControlRequests, 1000000},
		{"subscription_messages", config.SubscriptionMessages, 1000000}, {"subscription_bytes", config.SubscriptionBytes, 1 << 30},
		{"max_tombstones", config.MaxTombstones, 1000000}, {"outbound.max_sessions", config.Outbound.MaxSessions, 100000},
		{"outbound.queue_entries", config.Outbound.QueueEntries, 4096}, {"outbound.max_packet_bytes", config.Outbound.MaxPacketBytes, hardMaxPayload},
		{"outbound.per_session_bytes", config.Outbound.PerSessionBytes, 1 << 30},
		{"max_broadcast_players", config.MaxBroadcastPlayers, 100000}, {"max_broadcast_records", config.MaxBroadcastRecords, 1000000}, {"broadcast_workers", config.BroadcastWorkers, 1024},
	} {
		if limit.value <= 0 || limit.value > limit.max {
			problems = append(problems, fmt.Errorf("gateway: %s must be in 1..%d", limit.name, limit.max))
		}
	}
	for _, limit := range []struct {
		name       string
		value, max time.Duration
	}{
		{"request_timeout", config.RequestTimeout, time.Minute}, {"lease", config.Lease, time.Minute}, {"renew_interval", config.RenewInterval, time.Minute},
		{"tombstone_ttl", config.TombstoneTTL, 10 * time.Minute}, {"outbound.max_age", config.Outbound.MaxAge, time.Minute},
		{"outbound.send_timeout", config.Outbound.SendTimeout, time.Minute}, {"broadcast_ttl", config.BroadcastTTL, 10 * time.Minute},
	} {
		if limit.value <= 0 || limit.value > limit.max {
			problems = append(problems, fmt.Errorf("gateway: %s must be in (0,%v]", limit.name, limit.max))
		}
	}
	if config.BroadcastTTL < config.RequestTimeout+config.ClockSkew {
		problems = append(problems, errors.New("gateway: broadcast ttl must cover request budget and skew"))
	}
	if config.Outbound.ResidentBytes < int64(config.Outbound.MaxPacketBytes) || config.MaxForwardBytes < int64(config.MaxPayloadBytes) {
		problems = append(problems, errors.New("gateway: resident budgets cannot hold one complete payload"))
	}
	if config.ClockSkew < 0 || config.ClockSkew > time.Second {
		problems = append(problems, errors.New("gateway: clock_skew must be in [0,1s]"))
	}
	if config.ControlWorkers > config.MaxControlRequests || config.MaxControlBytes < 16<<10 || config.MaxControlBytes > 1<<40 {
		problems = append(problems, errors.New("gateway: control capacity must cover workers and one complete control packet, with bytes at most 1TiB"))
	}
	if config.MaxForwardBytes <= 0 || config.MaxForwardBytes > 1<<40 || config.Outbound.ResidentBytes <= 0 || config.Outbound.ResidentBytes > 1<<40 {
		problems = append(problems, errors.New("gateway: resident byte limits must be in 1..1TiB"))
	}
	if config.PerBindingRequests > config.MaxForwardRequests || config.ForwardWorkers > config.MaxForwardRequests {
		problems = append(problems, errors.New("gateway: per-binding requests and workers must not exceed total requests"))
	}
	if config.PerGameInFlight > config.ForwardWorkers {
		problems = append(problems, errors.New("gateway: per_game_in_flight exceeds global forward workers"))
	}
	if config.Outbound.MaxSessions < config.MaxBindings || config.Outbound.MaxPacketBytes < config.MaxPayloadBytes || config.Outbound.PerSessionBytes < config.Outbound.MaxPacketBytes {
		problems = append(problems, errors.New("gateway: outbound capacity cannot cover configured bindings and a complete packet"))
	}
	if config.RenewInterval*2+config.ClockSkew >= config.Lease || config.TombstoneTTL < config.Lease+config.RequestTimeout+config.ClockSkew {
		problems = append(problems, errors.New("gateway: lease must cover two renewals and tombstones must cover lease plus control request window"))
	}
	return errors.Join(problems...)
}

// ReliableQueue 是对既有 AsyncTransport 的接线契约；gateway 不复制队列状态机。
// Enqueue nil 仅表示本地准入。下游发送必须得到 Gate 准入 ACK，异步失败终止对应绑定。
type ReliableQueue interface {
	Register(uint64, int64) error
	Enqueue(context.Context, uint64, []byte) error
	Remove(uint64) bool
	Drain(context.Context) error
}

type ReliableQueueFactory func(OutboundLimits, func(context.Context, uint64, []byte) error, func(uint64, error)) (ReliableQueue, error)
