package mods

import (
	"fmt"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/app"
	fredis "github.com/tjbdwanghaibo/roost-core/redis"
	"github.com/tjbdwanghaibo/roost-core/servicemetrics"
)

// Redis returns the Redis capability, or an error naming what is missing.
//
// Every service Mod in this repository needs it, and every one of them should
// fail the same way when it is absent: with the capability name, at Provide
// time, rather than with a nil dereference on the first request.
func Redis(r *app.Registry) (fredis.IRedis, error) {
	client, ok := app.Lookup[fredis.IRedis](r, ModRedis)
	if !ok || client == nil {
		return nil, fmt.Errorf("mods: capability %q not found; add kit/redis.NewRedisMod()", ModRedis)
	}
	return client, nil
}

// CheckKeyPrefix checks a service's Redis key prefix (<service>.key_prefix).
//
// Each service Mod declares the key as required with no default, which is a
// deliberate choice rather than an oversight. A default would be the same
// string in every deployment, so two services of the same kind sharing one
// Redis — a staging environment beside production, two shards, a replay
// harness — would silently share state. Refusing at startup makes that a
// configuration error instead of a data corruption. The declaration refuses
// an empty value; this refuses embedded whitespace.
func CheckKeyPrefix(service, prefix string) error {
	if strings.ContainsAny(prefix, " \t\n") {
		return fmt.Errorf("servicemods: %s.key_prefix contains whitespace: %q", service, prefix)
	}
	return nil
}

// ValidateClusterKeyPrefix validates the common hash tag needed by services
// with atomic multi-key writes. Redis uses the first opening brace and the
// first closing brace after it; an empty first pair disables tag hashing even
// when a later pair is valid. Single-server prefixes (no redis.cluster_addrs,
// declared by kit/redis.ClusterConfig) are unchanged.
func ValidateClusterKeyPrefix(clusterAddrs []string, service, prefix string) error {
	if len(clusterAddrs) == 0 {
		return nil
	}
	if start := strings.IndexByte(prefix, '{'); start >= 0 {
		if end := strings.IndexByte(prefix[start+1:], '}'); end > 0 {
			return nil
		}
	}
	return fmt.Errorf("servicemods: %s.key_prefix (%q) requires a non-empty closed first hash tag for Redis Cluster; use e.g. {roost:%s} so atomic keys share a slot", service, prefix, service)
}

// ServiceMetricsConfig is service_metrics.enabled (decision C6), embedded by
// every kit service Mod's config so they share one declaration: false
// replaces the reporter with nil, so the service reports nothing; unset or
// true leaves it as the collaborator supplied it.
//
// Generated collaborators return servicemetrics.NewMetricsReporter by
// default, so this is how a deployment turns every service's metrics off in
// config without editing code (returning nil from Metrics() still works too).
type ServiceMetricsConfig struct {
	ServiceMetricsEnabled bool `config:"service_metrics.enabled" default:"true" help:"false 关掉本进程全部服务的业务指标"`
}

// ApplyServiceMetrics applies service_metrics.enabled to the reporter a
// service Mod will hand its service.
func (c ServiceMetricsConfig) ApplyServiceMetrics(reporter *servicemetrics.Reporter) {
	if !c.ServiceMetricsEnabled {
		*reporter = nil
	}
}
