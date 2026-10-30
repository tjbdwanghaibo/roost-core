package driver

import (
	"log/slog"
	"net/url"
	"strings"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"

	gonats "github.com/nats-io/nats.go"
)

// ClientOptions are the driver-level knobs that are not part of the core
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
			slog.Info("nats: reconnected", "url", c.ConnectedUrlRedacted())
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
	if cfg.Username != "" || cfg.Password != "" {
		opts = append(opts, gonats.UserInfo(cfg.Username, cfg.Password))
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

// RedactedURLs 把 NATS 地址（可能是逗号分隔的多个）里的 userinfo 整段换成 ***，供日志与错误使用。nats.url 可以由部署从
// 环境变量注入（R11），可能带 user:password@ 或 token@（token 在用户名的位置，所以用户名也一并盖掉）。
func RedactedURLs(urls string) string {
	parts := strings.Split(urls, ",")
	for i, part := range parts {
		part = strings.TrimSpace(part)
		if parsed, err := url.Parse(part); err == nil && parsed.User != nil {
			parsed.User = nil
			bare := parsed.String()
			if scheme := strings.Index(bare, "://"); scheme >= 0 {
				part = bare[:scheme+len("://")] + "***@" + bare[scheme+len("://"):]
			} else {
				part = "***@" + bare
			}
		} else if at := strings.LastIndexByte(part, '@'); err != nil && at >= 0 {
			part = "***" + part[at:]
		}
		parts[i] = part
	}
	return strings.Join(parts, ",")
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
