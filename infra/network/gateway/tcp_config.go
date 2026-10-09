// TCP 接入共用运行实现；业务协议与部署接线由调用方注入。
package gateway

import (
	"errors"
	"fmt"
	"math"
	"net"
	"time"
)

type TCPConfig struct {
	HeartbeatEnabled    bool
	RequestRate         float64
	RequestBurst        int
	Enabled             bool
	Addr                string
	MaxConnections      int
	MaxConnectionsPerIP int
	MaxHandshakes       int
	MaxHandshakeBytes   uint32
	MaxPayloadBytes     uint32
	HandshakeTimeout    time.Duration
	IdleTimeout         time.Duration
	WriteTimeout        time.Duration
	ShutdownTimeout     time.Duration
	// DispatchTimeout bounds one request: every Dispatch runs under a context
	// that ends this long after the frame was read (RR-20260926-36). It is
	// meant to match nest.request_timeout — the budget one Nest call inside
	// the handler already has — and defaults to it when the key is absent.
	DispatchTimeout time.Duration
	// LoginTimeout is the part of a login request's DispatchTimeout that
	// taking the player into service (ownership claim + cold load) may use.
	// It is not applied by the transport: the login endpoint reads it through
	// TCPRuntime.LoginTimeout, so what is left of the dispatch budget still
	// covers the answer and the placement steps after the load.
	LoginTimeout time.Duration
}

const (
	defaultDispatchTimeout = 3 * time.Second
	defaultLoginTimeout    = 2 * time.Second
)

func DefaultTCPConfig() TCPConfig {
	return TCPConfig{
		Addr: "0.0.0.0:7000", MaxConnections: 10000,
		HeartbeatEnabled: true, RequestRate: 100, RequestBurst: 200,
		MaxConnectionsPerIP: 128, MaxHandshakes: 1024, MaxHandshakeBytes: 8 << 10,
		MaxPayloadBytes: 1 << 20, HandshakeTimeout: 5 * time.Second,
		IdleTimeout: 90 * time.Second, WriteTimeout: 5 * time.Second,
		ShutdownTimeout: 10 * time.Second,
		DispatchTimeout: defaultDispatchTimeout, LoginTimeout: defaultLoginTimeout,
	}
}

// ValidateTCPConfig refuses every setting outside its bounds, each by its key,
// all at once. A limit bounded by another names that one too: a handshake runs
// on an accepted connection and one IP's connections are the server's, so
// max_handshakes and max_connections_per_ip cannot exceed max_connections, and
// the login runs inside one dispatch. One sentence for every limit used to
// leave the operator guessing which line to change (A9).
func ValidateTCPConfig(result TCPConfig) error {
	const key = "player_access.tcp."
	var problems []error
	refuse := func(format string, args ...any) {
		problems = append(problems, fmt.Errorf("player tcp: "+format, args...))
	}
	if result.Addr == "" {
		refuse("%saddr is empty", key)
	} else if _, _, err := net.SplitHostPort(result.Addr); err != nil {
		refuse("%saddr = %q is not host:port: %w", key, result.Addr, err)
	}
	if math.IsNaN(result.RequestRate) || math.IsInf(result.RequestRate, 0) || result.RequestRate < 0 || result.RequestRate > 100000 {
		refuse("%srequest_rate must be finite and in [0,100000]", key)
	}
	if result.RequestBurst < 1 || result.RequestBurst > 100000 {
		refuse("%srequest_burst must be in [1,100000]", key)
	}
	connectionsValid := result.MaxConnections > 0 && result.MaxConnections <= hardMaxConnections
	if !connectionsValid {
		refuse("%smax_connections = %d is outside 1..%d", key, result.MaxConnections, hardMaxConnections)
	}
	for _, limit := range []struct {
		name  string
		value int
		why   string
	}{
		{"max_connections_per_ip", result.MaxConnectionsPerIP, "one IP's connections are counted inside the server's"},
		{"max_handshakes", result.MaxHandshakes, "every handshake runs on an accepted connection"},
	} {
		switch {
		case limit.value <= 0:
			refuse("%s%s = %d must be positive", key, limit.name, limit.value)
		case connectionsValid && limit.value > result.MaxConnections:
			refuse("%s%s = %d exceeds %smax_connections = %d; %s", key, limit.name, limit.value, key, result.MaxConnections, limit.why)
		}
	}
	for _, limit := range []struct {
		name       string
		value, max uint32
	}{{"max_handshake_bytes", result.MaxHandshakeBytes, 64 << 10}, {"max_payload_bytes", result.MaxPayloadBytes, hardMaxPayload}} {
		if limit.value == 0 || limit.value > limit.max {
			refuse("%s%s = %d is outside 1..%d", key, limit.name, limit.value, limit.max)
		}
	}
	for _, timeout := range []struct {
		name       string
		value, max time.Duration
	}{
		{"handshake_timeout", result.HandshakeTimeout, time.Minute},
		{"idle_timeout", result.IdleTimeout, 24 * time.Hour},
		{"write_timeout", result.WriteTimeout, time.Minute},
		{"shutdown_timeout", result.ShutdownTimeout, 5 * time.Minute},
		{"dispatch_timeout", result.DispatchTimeout, 5 * time.Minute},
	} {
		if timeout.value <= 0 || timeout.value > timeout.max {
			refuse("%s%s = %v is outside (0, %v]", key, timeout.name, timeout.value, timeout.max)
		}
	}
	switch {
	case result.LoginTimeout <= 0:
		refuse("%slogin_timeout = %v must be positive", key, result.LoginTimeout)
	case result.DispatchTimeout > 0 && result.LoginTimeout > result.DispatchTimeout:
		refuse("%slogin_timeout = %v exceeds %sdispatch_timeout = %v; the login runs inside one dispatch", key, result.LoginTimeout, key, result.DispatchTimeout)
	}
	return errors.Join(problems...)
}
