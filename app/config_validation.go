package app

import (
	"errors"
	"fmt"
	"math"
	"net"
	"strings"

	"github.com/spf13/viper"
)

// ValidateServiceConfig 在任何 Mod Init 之前检查服务配置（App.run 调用）。除了各项语义检查，它把
// frameworkBoolKeys / frameworkDurationKeys / frameworkIntKeys 登记的框架键按严格规则读一遍（维护者决定 A4）：写成 `on` 的开关、
// 不带单位的时长、`8k` 这样的整数都在启动时点名报错，而不是被宽松读取静默读成 false / 纳秒 / 0。
// 同一个键的同一条错误只报一次。
func ValidateServiceConfig(cfg *viper.Viper) error {
	if cfg == nil {
		return errors.New("config: viper is nil")
	}
	var errs []error
	boolSetting := func(key string) bool {
		value, _ := ConfigBool(cfg, key) // 类型错误由下面的 frameworkBoolKeys 循环报告
		return value
	}
	serverType := strings.TrimSpace(cfg.GetString("server_type"))
	if serverType == "" {
		errs = append(errs, errors.New("config: server_type is required"))
	}
	if sid, err := ConfigInt64(cfg, "sid"); err == nil && (sid <= 0 || sid > math.MaxInt32) {
		errs = append(errs, errors.New("config: sid must be positive"))
	}
	if boolSetting("ops.admin_enabled") {
		token := strings.TrimSpace(cfg.GetString("ops.admin_token"))
		if token == "" {
			errs = append(errs, errors.New("config: ops.admin_token is required when ops.admin_enabled=true"))
		}
		if strings.HasPrefix(token, "dev-") && !boolSetting("ops.allow_dev_token") {
			errs = append(errs, errors.New("config: dev ops.admin_token requires ops.allow_dev_token=true"))
		}
	}
	validateTransport(&errs, cfg, "nats.rpc.transport", "core", "nats", "jetstream", "js")
	if isJetStreamTransport(cfg.GetString("nats.rpc.transport")) {
		validateDurationIfSet(&errs, cfg, "nats.rpc.ack_wait")
		validateDurationIfSet(&errs, cfg, "nats.rpc.request_ttl")
		validateDurationIfSet(&errs, cfg, "nats.rpc.call_timeout")
		validateDurationIfSet(&errs, cfg, "nats.rpc.stream_max_age")
		validateDurationIfSet(&errs, cfg, "nats.rpc.duplicates")
		validateDurationIfSet(&errs, cfg, "nats.rpc.setup_timeout")
		validatePositiveIntIfSet(&errs, cfg, "nats.rpc.max_deliver")
		validateNonNegativeIntIfSet(&errs, cfg, "nats.rpc.replicas")
		validateNonNegativeIntIfSet(&errs, cfg, "nats.rpc.max_bytes")
	}
	if boolSetting("nats.reliable.enabled") {
		validateDurationIfSet(&errs, cfg, "nats.reliable.inbox_ttl")
		validateDurationIfSet(&errs, cfg, "nats.reliable.dlq_ttl")
	}
	validateTransport(&errs, cfg, "sync.transport", "nats", "jetstream", "js")
	if isJetStreamTransport(cfg.GetString("sync.transport")) {
		validateDurationIfSet(&errs, cfg, "sync.ack_wait")
		validateDurationIfSet(&errs, cfg, "sync.stream_max_age")
		validateDurationIfSet(&errs, cfg, "sync.duplicates")
		validateDurationIfSet(&errs, cfg, "sync.setup_timeout")
		validateDurationIfSet(&errs, cfg, "sync.publish_timeout")
		validatePositiveIntIfSet(&errs, cfg, "sync.max_deliver")
	}
	validateDurationIfSet(&errs, cfg, "remote_entity.lock_ttl")
	validateDurationIfSet(&errs, cfg, "remote_entity.op_timeout")
	errs = append(errs, readSingletonSettings(cfg).validate()...)
	errs = append(errs, checkFrameworkConfigTypes(cfg)...)
	validateProductionServiceConfig(&errs, cfg, serverType)
	return errors.Join(uniqueErrors(errs)...)
}

// uniqueErrors 去掉文本相同的重复错误：类型检查与语义检查可能对同一个写错的键各报一次。
func uniqueErrors(errs []error) []error {
	seen := make(map[string]bool, len(errs))
	out := errs[:0]
	for _, err := range errs {
		if err == nil || seen[err.Error()] {
			continue
		}
		seen[err.Error()] = true
		out = append(out, err)
	}
	return out
}

// validateProductionServiceConfig 是 env / app.env / environment 为 prod / production 时追加的检查。
// 这里只要求有读取方、确实生效的设置（RR-20261005-NC-192，维护者决定 C1 方案 1）：ops 端点不暴露在
// 公网、各服务真正读取的密钥不是空的或 dev- 开头、依赖 Redis 的服务写了 redis.addr、admin_gateway 的
// 令牌、业务时钟偏移 time.logic_offset 为 0（D-L3）。以前这里还要求 player.login_auth_required、player_protocol.rate_limit.enabled、save_load.wal.*、
// instance.*、account.ops_token、account.redis_required、global / match_group.redis_required 等开关，
// 它们没有任何代码读取，写上只是为了让校验放行，却让人以为限流、登录鉴权、WAL 持久已经打开。
// 以后接入真实的限流 / 鉴权开关时，再把它们的键加回这里。USER_GUIDE“生产环境校验”一节与本函数同步。
func validateProductionServiceConfig(errs *[]error, cfg *viper.Viper, serverType string) {
	if !isProductionServiceConfig(cfg) {
		return
	}
	validateProductionOpsExposure(errs, cfg)
	validateProductionLogicOffset(errs, cfg)
	serverType = strings.ToLower(strings.TrimSpace(serverType))
	switch serverType {
	case "game", "instance", "account", "match_group", "global":
		if strings.TrimSpace(cfg.GetString("redis.addr")) == "" {
			*errs = append(*errs, fmt.Errorf("config: production %s requires redis.addr", serverType))
		}
	}
	switch serverType {
	case "account":
		validateProductionSecret(errs, cfg, "account.session_secret")
	case "platform":
		validateProductionSecret(errs, cfg, "platform.session_secret")
		validateProductionSecret(errs, cfg, "platform.payment_secret")
	case "admin_gateway":
		validateProductionAdminGateway(errs, cfg)
	}
}

func validateProductionAdminGateway(errs *[]error, cfg *viper.Viper) {
	var raw struct {
		DefaultOpsAdminToken string `mapstructure:"default_ops_admin_token"`
		Tokens               []struct {
			Token string `mapstructure:"token"`
		} `mapstructure:"tokens"`
		Targets []struct {
			OpsAddr      string `mapstructure:"ops_addr"`
			OpsToken     string `mapstructure:"ops_token"`
			DispatchMode string `mapstructure:"dispatch_mode"`
		} `mapstructure:"targets"`
	}
	if err := cfg.UnmarshalKey("admin_gateway", &raw); err != nil {
		*errs = append(*errs, fmt.Errorf("config: admin_gateway production config invalid: %w", err))
		return
	}
	if len(raw.Tokens) == 0 {
		*errs = append(*errs, errors.New("config: production admin_gateway requires admin_gateway.tokens"))
	}
	for i, token := range raw.Tokens {
		if isUnsafeProductionSecret(token.Token) {
			*errs = append(*errs, fmt.Errorf("config: production admin_gateway requires non-dev admin_gateway.tokens[%d].token", i))
		}
	}
	needsDefaultOpsToken := false
	for _, target := range raw.Targets {
		mode := strings.ToLower(strings.TrimSpace(target.DispatchMode))
		if mode == "" || mode == "local_ops" {
			if strings.TrimSpace(target.OpsAddr) != "" && strings.TrimSpace(target.OpsToken) == "" {
				needsDefaultOpsToken = true
				break
			}
		}
	}
	if needsDefaultOpsToken && isUnsafeProductionSecret(raw.DefaultOpsAdminToken) {
		*errs = append(*errs, errors.New("config: production admin_gateway requires admin_gateway.default_ops_admin_token for local_ops targets without ops_token"))
	}
	if strings.TrimSpace(raw.DefaultOpsAdminToken) != "" && hasDevTokenPrefix(raw.DefaultOpsAdminToken) {
		*errs = append(*errs, errors.New("config: production admin_gateway must not use a dev admin_gateway.default_ops_admin_token"))
	}
}

func validateProductionSecret(errs *[]error, cfg *viper.Viper, key string) {
	if isUnsafeProductionSecret(cfg.GetString(key)) {
		*errs = append(*errs, fmt.Errorf("config: production requires non-dev %s", key))
	}
}

// validateProductionOpsExposure keeps the ops endpoint off public interfaces
// in production. Loopback is only the default, and the endpoint carries both
// an unauthenticated /metrics and — when admin is enabled — the ability to run
// every registered admin command, so binding it to every interface has to be
// a deliberate, declared decision rather than a forgotten default.
func validateProductionOpsExposure(errs *[]error, cfg *viper.Viper) {
	enabled, _ := ConfigBool(cfg, "ops.enabled")
	if !enabled {
		return
	}
	addr := strings.TrimSpace(cfg.GetString("ops.addr"))
	if allowPublic, _ := ConfigBool(cfg, "ops.allow_public_addr"); addr == "" || isLoopbackListenAddr(addr) || allowPublic {
		return
	}
	*errs = append(*errs, fmt.Errorf(
		"config: production ops.addr %q is not loopback; bind 127.0.0.1 or set ops.allow_public_addr=true after putting the endpoint behind an authenticated proxy", addr))
}

// isLoopbackListenAddr reports whether a listen address is reachable only from
// the host. A bare port or an empty/wildcard host means "every interface".
func isLoopbackListenAddr(addr string) bool {
	host := addr
	if parsed, _, err := net.SplitHostPort(addr); err == nil {
		host = parsed
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func isProductionServiceConfig(cfg *viper.Viper) bool {
	if cfg == nil {
		return false
	}
	for _, key := range []string{"env", "app.env", "environment"} {
		switch strings.ToLower(strings.TrimSpace(cfg.GetString(key))) {
		case "prod", "production":
			return true
		}
	}
	return false
}

func isUnsafeProductionSecret(value string) bool {
	return strings.TrimSpace(value) == "" || hasDevTokenPrefix(value)
}

func hasDevTokenPrefix(value string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(value)), "dev-")
}

func validateTransport(errs *[]error, cfg *viper.Viper, key string, allowed ...string) {
	value := strings.ToLower(strings.TrimSpace(cfg.GetString(key)))
	if value == "" {
		return
	}
	for _, item := range allowed {
		if value == item {
			return
		}
	}
	*errs = append(*errs, fmt.Errorf("config: %s has unsupported transport %q", key, value))
}

func isJetStreamTransport(value string) bool {
	value = strings.ToLower(strings.TrimSpace(value))
	return value == "jetstream" || value == "js"
}

// 框架配置键的类型登记（维护者决定 A4，接 RR-20261005-NC-190）。
//
// 这里列出框架代码——app、kit 的各 Mod 与生成器写进工程的接入层（player TCP、RPC 客户端 Mod）——用
// 类型化读取的全部键。ValidateServiceConfig 在任何 Mod Init 之前按严格规则读一遍：宽松读取会把
// `enabled: on` 读成 false、`ttl: 15` 读成 15ns、`workers: 8k` 读成 0（取默认），保护静默失效。
// kit 与生成的接入层（生成器 Core 下限 v1.20.2 起）自己也用 app.ConfigReader / ConfigDuration 严格读取，
// 这份登记的作用是：错误在启动第一步一次报全；下限之前生成、未重新生成的工程代码仍用 viper 的 getter，
// 靠这里在新版本上得到同样的检查。
//
// app 的 TestFrameworkCodeDoesNotReadConfigLeniently 与 TestEvery*IsCheckedStrictly 扫描 app、kit 与生成模板的源码：读到未登记的
// 键、或在框架代码里新增宽松读取，测试变红。singleton.* 由 readSingletonSettings 检查，不在这里重复。
var (
	frameworkBoolKeys = []string{
		"log.json", "log.stdout", "log.file", "log.caller",
		"ops.enabled", "ops.admin_enabled", "ops.allow_dev_token", "ops.allow_public_addr",
		"mongo.require_replica_set", "mongo.index.allow_recreate",
		"nats.ignore_discovered_servers", "nats.reliable.enabled",
		"dataengine.enabled", "nest.pipelined.async", "stats_log.enabled",
		"player_access.tcp.enabled",
		"service_metrics.enabled", // kit/mods.ServiceMetricsEnabledKey（C6）
	}
	frameworkDurationKeys = []string{
		"time.logic_offset", "log.rotate_interval", "shutdown.total_timeout",
		"ops.admin_timeout",
		"stats_log.interval",
		"etcd.register_retry_min_interval", "etcd.register_retry_max_interval",
		"mongo.connect_timeout", "mongo.max_idle_time", "mongo.transaction_timeout",
		"nats.reliable.inbox_ttl", "nats.reliable.dlq_ttl",
		"nats.rpc.ack_wait", "nats.rpc.request_ttl", "nats.rpc.call_timeout", "nats.rpc.stream_max_age",
		"nats.rpc.duplicates", "nats.rpc.setup_timeout",
		"nest.tick_duration", "nest.request_timeout", "nest.max_delay", "nest.entity_load_timeout",
		"sync.entity.interval",
		"dataengine.startup_timeout", "dataengine.shutdown_timeout",
		"dataengine.transaction_receipt_ttl", "dataengine.receipt_ttl",
		"dataengine.wal.group_commit_interval", "dataengine.wal.max_unacked_age",
		"dataengine.projection.retry_min", "dataengine.projection.retry_max", "dataengine.projection.checkpoint_interval",
		"dataengine.outbox.lease_duration", "dataengine.outbox.poll_interval", "dataengine.outbox.retry_min",
		"dataengine.outbox.retry_max", "dataengine.outbox.max_oldest_age", "dataengine.outbox.backlog_interval",
		"dataengine.effects.max_age", "dataengine.effects.duplicate_window",
		"remote_entity.lock_ttl", "remote_entity.retry_delay", "remote_entity.op_timeout", "remote_entity.version_ttl",
		"remote_entity.unlock_retry_interval", "remote_entity.finalize_retry_interval",
		"remote_entity.finalize_projection_timeout", "remote_entity.snapshot_cache_ttl", "remote_entity.snapshot_l2_ttl",
		"remote_entity.snapshot_interest_ttl", "remote_entity.marker_cache_ttl", "remote_entity.snapshot_load_timeout",
		"remote_entity.transaction_track_ttl", "remote_entity.wrapper_idle_ttl", "remote_entity.mongo.transaction_ttl",
		"remote_entity.cached_max_staleness", "remote_entity.mirror.shutdown_timeout",
		"remote_entity.snapshot_l2_tombstone_wait_timeout",
		"saga.completion_receipt_ttl", "saga.lease_duration", "saga.store_timeout", "saga.poll_interval",
		"saga.publish_timeout", "saga.publish_backoff_min", "saga.publish_backoff_max",
		"saga.stream_max_age", "saga.duplicate_window",
		"saga.result_ack_wait", "saga.result_process_timeout", "saga.result_nak_backoff_min", "saga.result_nak_backoff_max",
		"saga.result_effect_ack_wait", "saga.result_effect_process_timeout",
		"saga.result_effect_nak_backoff_min", "saga.result_effect_nak_backoff_max",
		"saga.start_effect_ack_wait", "saga.start_effect_process_timeout",
		"saga.start_effect_nak_backoff_min", "saga.start_effect_nak_backoff_max",
		"account.session_ttl", "account.claim_ttl", "activity.reservation_ttl", "activity.grace_window",
		"activity.dispatch_backoff", "chat.retention_age", "directory.reservation_ttl", "mail.send_ttl",
		"mail.claim_lease", "match.ticket_ttl", "platform.session_ttl", "platform.delivery_backoff",
		"session.run_ttl", "session.request_ttl",
		"player_access.tcp.handshake_timeout", "player_access.tcp.idle_timeout", "player_access.tcp.write_timeout",
		"player_access.tcp.shutdown_timeout", "player_access.tcp.dispatch_timeout", "player_access.tcp.login_timeout",
	}
	frameworkIntKeys = []string{
		"sid", "metrics.max_series_per_metric",
		"etcd.lease_ttl",
		"redis.db", "redis.pool_size", "redis.min_idle_conns",
		"mongo.max_pool_size", "mongo.min_pool_size",
		"nats.worker_num", "nats.rpc.max_deliver", "nats.rpc.replicas", "nats.rpc.max_bytes",
		"nest.fast.workers", "nest.fast.queue_capacity", "nest.slow.workers", "nest.slow.queue_capacity",
		"nest.worker_num", "nest.heartbeat_worker_num", "nest.remote_workers", "nest.queue_capacity",
		"nest.delayed_capacity", "nest.unload_resync.workers", "nest.unload_resync.attempts",
		"nest.unload_resync.queue_capacity", "nest.pipelined.async_workers", "nest.pipelined.async_queue_capacity",
		"sync.entity.max_frozen_bytes",
		"dataengine.wal.writer_version", "dataengine.wal.segment_bytes", "dataengine.wal.queue_capacity",
		"dataengine.wal.max_disk_bytes",
		"dataengine.projection.remote_workers", "dataengine.projection.batch_records", "dataengine.projection.batch_bytes",
		"dataengine.projection.read_bytes", "dataengine.projection.checkpoint_records",
		"dataengine.projection.max_unacked_records", "dataengine.projection.warn_unacked_records",
		"dataengine.outbox.workers", "dataengine.outbox.batch_size", "dataengine.outbox.max_pending",
		"dataengine.effects.replicas", "dataengine.effects.max_bytes",
		"remote_entity.retry_count", "remote_entity.unlock_retry_count", "remote_entity.max_write_batch",
		"remote_entity.snapshot_cache_shards", "remote_entity.snapshot_cache_entries", "remote_entity.snapshot_cache_bytes",
		"remote_entity.snapshot_interest_keys", "remote_entity.snapshot_interest_subs", "remote_entity.snapshot_interest_per_consumer",
		"remote_entity.snapshot_max_waiters", "remote_entity.snapshot_l2_tombstone_wait_replicas",
		"remote_entity.async_finalize_capacity", "remote_entity.max_concurrent_writes", "remote_entity.async_finalize_workers",
		"remote_entity.transaction_track_limit", "remote_entity.wrapper_capacity",
		"saga.coordinator_workers", "saga.publisher_workers", "saga.coordinator_claim_batch", "saga.publisher_claim_batch",
		"saga.max_payload_bytes", "saga.replicas", "saga.stream_max_bytes",
		"saga.result_max_deliver", "saga.result_max_ack_pending",
		"saga.result_effect_max_deliver", "saga.result_effect_max_ack_pending",
		"saga.start_effect_max_deliver", "saga.start_effect_max_ack_pending",
		"activity.dispatch_attempts", "platform.delivery_attempts",
		"player_access.tcp.max_connections", "player_access.tcp.max_connections_per_ip", "player_access.tcp.max_handshakes",
		"player_access.tcp.max_handshake_bytes", "player_access.tcp.max_payload_bytes",
	}
	// syncbus Mod 按 syncbus / room / sync 段的优先级读同名字段（kit/syncbus configKey），三段都检查。
	syncBusSections       = []string{"syncbus", "room", "sync"}
	syncBusDurationFields = []string{"ack_wait", "stream_max_age", "duplicates", "setup_timeout", "publish_timeout"}
	syncBusIntFields      = []string{"max_deliver", "replicas", "max_bytes"}
	// frameworkDurationSuffixes 按键名后缀登记：生成的 RPC 客户端 Mod 读 <service>.call_timeout，
	// 服务名属于应用（kit 的服务与工程自己 add rpc 的服务），只能按后缀找。
	frameworkDurationSuffixes = []string{".call_timeout"}
)

// checkFrameworkConfigTypes 按三份登记（含 syncbus 三段与按后缀登记的键）严格读取每个已设置的键，返回类型错误。
func checkFrameworkConfigTypes(cfg *viper.Viper) []error {
	var errs []error
	note := func(err error) {
		if err != nil {
			errs = append(errs, err)
		}
	}
	durationKeys := append([]string(nil), frameworkDurationKeys...)
	intKeys := append([]string(nil), frameworkIntKeys...)
	for _, section := range syncBusSections {
		for _, field := range syncBusDurationFields {
			durationKeys = append(durationKeys, section+"."+field)
		}
		for _, field := range syncBusIntFields {
			intKeys = append(intKeys, section+"."+field)
		}
	}
	for _, key := range cfg.AllKeys() {
		for _, suffix := range frameworkDurationSuffixes {
			if strings.HasSuffix(key, suffix) {
				durationKeys = append(durationKeys, key)
			}
		}
	}
	for _, key := range frameworkBoolKeys {
		_, err := ConfigBool(cfg, key)
		note(err)
	}
	for _, key := range durationKeys {
		_, err := ConfigDuration(cfg, key)
		note(err)
	}
	for _, key := range intKeys {
		_, err := ConfigInt64(cfg, key)
		note(err)
	}
	return errs
}

// validateDurationIfSet 要求设置了的时长带单位且为正：不带单位的数字不再当作纳秒（RR-20261005-NC-190）。
func validateDurationIfSet(errs *[]error, cfg *viper.Viper, key string) {
	if !cfg.IsSet(key) {
		return
	}
	value, err := ConfigDuration(cfg, key)
	if err != nil {
		*errs = append(*errs, err)
		return
	}
	if value <= 0 {
		*errs = append(*errs, fmt.Errorf("config: %s must be positive", key))
	}
}

func validatePositiveIntIfSet(errs *[]error, cfg *viper.Viper, key string) {
	if !cfg.IsSet(key) {
		return
	}
	if value, err := ConfigInt64(cfg, key); err == nil && value <= 0 {
		*errs = append(*errs, fmt.Errorf("config: %s must be positive", key))
	}
}

func validateNonNegativeIntIfSet(errs *[]error, cfg *viper.Viper, key string) {
	if !cfg.IsSet(key) {
		return
	}
	if value, err := ConfigInt64(cfg, key); err == nil && value < 0 {
		*errs = append(*errs, fmt.Errorf("config: %s must be non-negative", key))
	}
}
