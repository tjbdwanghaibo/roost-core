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
	// that ends this long after the frame was read. It is
	// meant to match nest.request_timeout — the budget one Nest call inside
	// the handler already has — and defaults to it when the key is absent.
	DispatchTimeout time.Duration
	// LoginTimeout is the part of a login request's DispatchTimeout that
	// taking the player into service (ownership claim + cold load) may use.
	// It is not applied by the transport: the login endpoint reads it through
	// TCPRuntime.LoginTimeout, so what is left of the dispatch budget still
	// covers the answer and the placement steps after the load.
	LoginTimeout time.Duration
	// OutboundResidentBytes 是 player_access.tcp.outbound.resident_bytes：全进程所有连接出站队列
	// “排队 + 写协程已取走未写完”的字节合计上限（与独立 Gate 的 outbound.resident_bytes 同一口径）。
	// 一帧入队会超过它时被拒（ErrResidentBudgetExhausted），不关闭连接；读循环自己的回包与心跳 ACK 计入但不被拒。
	// 0 表示取缺省值（NewTCPServer 补上，与 DispatchTimeout 相同）。
	OutboundResidentBytes int64
}

const (
	defaultDispatchTimeout = 3 * time.Second
	defaultLoginTimeout    = 2 * time.Second
	// defaultOutboundResidentBytes 取 256 MiB：独立 Gate 的缺省 128 MiB 对应 4096 个连接，嵌入接入缺省
	// max_connections 是 10000，按比例约 312 MiB，取整到 256 MiB。正常负载下写协程把帧很快交给 socket，
	// 全进程驻留接近 0；10000 个连接各有一帧 26 KiB 同时在途才用满，场景 Sync / Lockstep 每次每连接只推几帧、
	// 一帧通常几 KiB。超出时 Sync 整轮重试、不断开，代价只是延迟。修前最坏是 max_connections × 每连接字节上限（约 20 GiB）。
	defaultOutboundResidentBytes = 256 << 20
	maxOutboundResidentBytes     = 1 << 40
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
		OutboundResidentBytes: defaultOutboundResidentBytes,
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
	// 下界是一个最大帧：预算放不下一个 max_payload_bytes 的帧时，那样的帧永远发不出去（与 Gate 的
	// resident_bytes >= max_packet_bytes 同一规则）。0 是代码构造的 TCPConfig 没有设置，NewTCPServer 取缺省值；
	// 配置文件里写 0 已被声明的 min 在加载时拒绝。
	frameBytes := int64(result.MaxPayloadBytes) + headerSize
	switch {
	case result.OutboundResidentBytes < 0 || result.OutboundResidentBytes > maxOutboundResidentBytes:
		refuse("%soutbound.resident_bytes = %d is outside 1..%d", key, result.OutboundResidentBytes, int64(maxOutboundResidentBytes))
	case result.OutboundResidentBytes > 0 && result.MaxPayloadBytes > 0 && result.OutboundResidentBytes < frameBytes:
		refuse("%soutbound.resident_bytes = %d is below one largest frame (%smax_payload_bytes + %d = %d)", key, result.OutboundResidentBytes, key, headerSize, frameBytes)
	}
	return errors.Join(problems...)
}
