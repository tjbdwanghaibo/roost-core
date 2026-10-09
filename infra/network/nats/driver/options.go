package driver

import (
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"log/slog"

	gonats "github.com/nats-io/nats.go"
)

// ClientOptions are the kit-level knobs that are not part of the core
// fnats.Config contract.
type ClientOptions struct {
	// IgnoreDiscoveredServers keeps the client on the URLs it was configured
	// with. By default nats.go learns every cluster member's advertised
	// address from INFO gossip and reconnects to whichever answers — which is
	// right in a flat network and wrong behind a proxy, NAT, or a fault
	// injector: the client silently escapes the path the operator configured.
	IgnoreDiscoveredServers bool
	// InboxPrefix 让部署 ACL 能限制本服务自己的回信命名空间。
	InboxPrefix string
	// ReconnectBufferBytes 是断线期间的本地发布字节上限；-1 禁止缓冲，0 采用库默认。
	ReconnectBufferBytes int
}

func buildNatsOptions(cfg *fnats.Config, state *natsLifecycleState, extra ClientOptions) []gonats.Option {
	opts := []gonats.Option{
		gonats.ReconnectWait(cfg.ReconnectWait),
		gonats.MaxReconnects(cfg.MaxReconnects),
		gonats.PingInterval(cfg.PingInterval),
		gonats.DisconnectErrHandler(func(c *gonats.Conn, err error) {
			handleNatsDisconnect(state, cfg, err)
		}),
		gonats.ReconnectHandler(func(c *gonats.Conn) {
			slog.Info("nats: reconnected", "url", c.ConnectedUrl())
			if cfg.OnReconnect != nil {
				cfg.OnReconnect()
			}
		}),
		gonats.ClosedHandler(func(c *gonats.Conn) {
			slog.Info("nats: connection closed")
		}),
		gonats.ErrorHandler(func(c *gonats.Conn, sub *gonats.Subscription, err error) {
			if state != nil {
				state.rawError(sub, err)
			}
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			slog.Error("nats: async error", "subject", subject, "err", err)
		}),
	}
	if extra.IgnoreDiscoveredServers {
		opts = append(opts, gonats.IgnoreDiscoveredServers())
	}
	if extra.InboxPrefix != "" {
		opts = append(opts, gonats.CustomInboxPrefix(extra.InboxPrefix))
	}
	if extra.ReconnectBufferBytes != 0 {
		opts = append(opts, gonats.ReconnectBufSize(extra.ReconnectBufferBytes))
	}
	return opts
}

func handleNatsDisconnect(state *natsLifecycleState, cfg *fnats.Config, err error) {
	if state != nil && state.expectedDisconnect() || err == nil {
		slog.Info("nats: disconnected", "err", err)
	} else {
		slog.Error("nats: disconnected", "err", err)
	}
	if cfg != nil && cfg.OnDisconnect != nil {
		cfg.OnDisconnect(err)
	}
}
