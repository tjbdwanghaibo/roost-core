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
func Duration(cfg *viper.Viper, key string, fallback time.Duration) (time.Duration, error) {
	if !cfg.IsSet(key) {
		return fallback, nil
	}
	value := cfg.GetDuration(key)
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
	value := cfg.GetDuration(key)
	if value <= 0 {
		return 0, fmt.Errorf("servicemods: %s must be positive, got %s", key, value)
	}
	return value, nil
}
