package mods

import (
	"fmt"
	"strings"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/app"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// Redis returns the Redis capability, or an error naming what is missing.
//
// Every service Mod in this repository needs it, and every one of them should
// fail the same way when it is absent: with the capability name, at Provide
// time, rather than with a nil dereference on the first request.
func Redis(r *app.Registry) (fredis.IRedis, error) {
	client, ok := app.Lookup[fredis.IRedis](r, ModRedis)
	if !ok || client == nil {
		return nil, fmt.Errorf("servicemods: capability %q not found; add roost-kit/redis.NewRedisMod()", ModRedis)
	}
	return client, nil
}

// KeyPrefix reads a service's Redis key prefix from configuration.
//
// The prefix is REQUIRED to be non-empty and there is no default, which is a
// deliberate choice rather than an oversight. A default would be the same
// string in every deployment, so two services of the same kind sharing one
// Redis — a staging environment beside production, two shards, a replay
// harness — would silently share state. Refusing at startup makes that a
// configuration error instead of a data corruption.
func KeyPrefix(cfg *viper.Viper, service string) (string, error) {
	key := service + ".key_prefix"
	prefix := strings.TrimSpace(cfg.GetString(key))
	if prefix == "" {
		return "", fmt.Errorf("servicemods: %s is required and has no default; two deployments "+
			"sharing one redis would otherwise share state", key)
	}
	if strings.ContainsAny(prefix, " \t\n") {
		return "", fmt.Errorf("servicemods: %s contains whitespace: %q", key, prefix)
	}
	return prefix, nil
}

// ValidateClusterKeyPrefix validates the common hash tag needed by services
// with atomic multi-key writes. Redis uses the first opening brace and the
// first closing brace after it; an empty first pair disables tag hashing even
// when a later pair is valid. Single-server prefixes are unchanged.
func ValidateClusterKeyPrefix(cfg *viper.Viper, service, prefix string) error {
	if len(RedisClusterAddrs(cfg)) == 0 {
		return nil
	}
	if start := strings.IndexByte(prefix, '{'); start >= 0 {
		if end := strings.IndexByte(prefix[start+1:], '}'); end > 0 {
			return nil
		}
	}
	return fmt.Errorf("servicemods: %s.key_prefix (%q) requires a non-empty closed first hash tag for Redis Cluster; use e.g. {roost:%s} so atomic keys share a slot", service, prefix, service)
}

// RedisClusterAddrs 返回 redis.cluster_addrs 里的 Cluster 种子地址，空表示单机。接受逗号分隔的
// 字符串与 YAML 列表两种写法，去掉每项两端空白、丢弃空项。Redis Mod、单实例锁连接与各服务的
// Cluster hash tag 校验都经它判断是否是 Cluster：旧实现用 GetString，列表写法读成空串，Redis Mod
// 退回 localhost:6379 单机、hash tag 校验被跳过（RR-20261005-NC-190）。
func RedisClusterAddrs(cfg *viper.Viper) []string {
	if cfg == nil || !cfg.IsSet("redis.cluster_addrs") {
		return nil
	}
	var items []string
	switch raw := cfg.Get("redis.cluster_addrs").(type) {
	case string:
		items = strings.Split(raw, ",")
	default:
		items = cfg.GetStringSlice("redis.cluster_addrs")
	}
	addrs := make([]string, 0, len(items))
	for _, item := range items {
		if item = strings.TrimSpace(item); item != "" {
			addrs = append(addrs, item)
		}
	}
	if len(addrs) == 0 {
		return nil
	}
	return addrs
}

// Secret reads a required secret from configuration.
//
// Empty is refused at startup. The implementation this repository replaces
// checked its payment secret at call time, so an unset secret turned every
// provider callback into an invalid-signature refusal — a silent outage that
// looked like an attack.
func Secret(cfg *viper.Viper, key string) (string, error) {
	secret := cfg.GetString(key)
	if strings.TrimSpace(secret) == "" {
		return "", fmt.Errorf("servicemods: %s is required and must not be empty", key)
	}
	return secret, nil
}

// Duration reads an optional duration, falling back to a default.
//
// A negative value is refused rather than clamped: a caller that wrote -1
// meant something, and silently reading it as the default hides the mistake.
//
// The value must carry a unit: a bare number used to be read as nanoseconds,
// and an unparseable one as zero and so the fallback (RR-20261005-NC-190).
func Duration(cfg *viper.Viper, key string, fallback time.Duration) (time.Duration, error) {
	if !cfg.IsSet(key) {
		return fallback, nil
	}
	value, err := app.ConfigDuration(cfg, key)
	if err != nil {
		return 0, fmt.Errorf("servicemods: %w", err)
	}
	if value < 0 {
		return 0, fmt.Errorf("servicemods: %s must not be negative, got %s", key, value)
	}
	if value == 0 {
		return fallback, nil
	}
	return value, nil
}

// RequiredDuration reads a duration that has no sensible default.
func RequiredDuration(cfg *viper.Viper, key string) (time.Duration, error) {
	if !cfg.IsSet(key) {
		return 0, fmt.Errorf("servicemods: %s is required and has no default", key)
	}
	value, err := app.ConfigDuration(cfg, key)
	if err != nil {
		return 0, fmt.Errorf("servicemods: %w", err)
	}
	if value <= 0 {
		return 0, fmt.Errorf("servicemods: %s must be positive, got %s", key, value)
	}
	return value, nil
}
