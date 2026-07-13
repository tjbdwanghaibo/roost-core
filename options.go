package nats

import (
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	"log/slog"

	gonats "github.com/nats-io/nats.go"
)

func buildNatsOptions(cfg *fnats.Config, state *natsLifecycleState) []gonats.Option {
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
			subject := ""
			if sub != nil {
				subject = sub.Subject
			}
			slog.Error("nats: async error", "subject", subject, "err", err)
		}),
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
