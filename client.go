package nats

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"

	gonats "github.com/nats-io/nats.go"
)

const (
	publishRetries   = 3
	publishRetryWait = 20 * time.Millisecond
)

// natsClient implements fnats.IClient by wrapping nats-io/nats.go.
type natsClient struct {
	conn  *gonats.Conn
	cfg   *fnats.Config
	state *natsLifecycleState
}

type natsLifecycleState struct {
	draining atomic.Bool
	closing  atomic.Bool
}

func (s *natsLifecycleState) expectedDisconnect() bool {
	return s != nil && (s.draining.Load() || s.closing.Load())
}

func newNatsClient(cfg *fnats.Config) (*natsClient, error) {
	if cfg == nil || strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("nats: configuration and URL are required")
	}
	state := &natsLifecycleState{}
	opts := buildNatsOptions(cfg, state)
	conn, err := gonats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats: connect %s: %w", cfg.URL, err)
	}
	slog.Info("nats: connected", "url", cfg.URL)
	return &natsClient{conn: conn, cfg: cfg, state: state}, nil
}

func (c *natsClient) Publish(subject string, data []byte) error {
	if err := c.validateSubject(subject); err != nil {
		return err
	}
	var err error
	for i := 0; i < publishRetries; i++ {
		err = c.conn.Publish(subject, data)
		if err == nil {
			return nil
		}
		time.Sleep(publishRetryWait)
	}
	return fmt.Errorf("nats: publish to %s failed after %d retries: %w", subject, publishRetries, err)
}

func (c *natsClient) Request(subject string, data []byte, timeout time.Duration) ([]byte, error) {
	if err := c.validateSubject(subject); err != nil {
		return nil, err
	}
	if timeout <= 0 {
		return nil, fmt.Errorf("nats: request timeout must be positive")
	}
	msg, err := c.conn.Request(subject, data, timeout)
	if err != nil {
		return nil, c.wrapError(err)
	}
	return msg.Data, nil
}

func (c *natsClient) requestWithContext(ctx context.Context, subject string, data []byte) ([]byte, error) {
	if err := c.validateSubject(subject); err != nil {
		return nil, err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	msg, err := c.conn.RequestWithContext(ctx, subject, data)
	if err != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, fnats.ErrTimeout
		}
		if ctx.Err() != nil {
			return nil, fnats.ErrCancelled
		}
		return nil, c.wrapError(err)
	}
	return msg.Data, nil
}

func (c *natsClient) Subscribe(subject string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
	if err := c.validateSubscription(subject, "", handler); err != nil {
		return nil, err
	}
	sub, err := c.conn.Subscribe(subject, func(msg *gonats.Msg) {
		invokeNatsHandler(handler, &fnats.Msg{
			Subject: msg.Subject,
			Reply:   msg.Reply,
			Data:    append([]byte(nil), msg.Data...),
		})
	})
	if err != nil {
		return nil, err
	}
	return &subscription{sub: sub}, nil
}

func (c *natsClient) QueueSubscribe(subject string, queue string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
	if err := c.validateSubscription(subject, queue, handler); err != nil {
		return nil, err
	}
	if strings.TrimSpace(queue) == "" {
		return nil, fmt.Errorf("nats: queue is required")
	}
	sub, err := c.conn.QueueSubscribe(subject, queue, func(msg *gonats.Msg) {
		invokeNatsHandler(handler, &fnats.Msg{
			Subject: msg.Subject,
			Reply:   msg.Reply,
			Data:    append([]byte(nil), msg.Data...),
		})
	})
	if err != nil {
		return nil, err
	}
	return &subscription{sub: sub}, nil
}

func (c *natsClient) Drain() error {
	ctx := context.Background()
	cancel := func() {}
	if c != nil && c.cfg != nil && c.cfg.DrainTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.cfg.DrainTimeout)
	}
	defer cancel()
	return c.DrainWithContext(ctx)
}

func (c *natsClient) DrainWithContext(ctx context.Context) error {
	if c == nil || c.conn == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.state != nil {
		c.state.draining.Store(true)
	}
	if err := c.conn.Drain(); err != nil {
		return err
	}
	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()
	for !c.conn.IsClosed() {
		select {
		case <-ctx.Done():
			c.conn.Close()
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func (c *natsClient) Close() {
	if c == nil {
		return
	}
	if c.state != nil {
		c.state.closing.Store(true)
	}
	if c.conn != nil {
		c.conn.Close()
	}
}

func (c *natsClient) Connected() bool {
	return c != nil && c.conn != nil && c.conn.IsConnected()
}

func (c *natsClient) wrapError(err error) error {
	if err == gonats.ErrTimeout {
		return fnats.ErrTimeout
	}
	if err == gonats.ErrNoResponders {
		return fnats.ErrNoResponders
	}
	if err == gonats.ErrConnectionClosed || err == gonats.ErrConnectionDraining {
		return fnats.ErrClosed
	}
	return err
}

// conn returns the underlying gonats.Conn (for rpcClient internal use).
func (c *natsClient) natsConn() *gonats.Conn {
	if c == nil {
		return nil
	}
	return c.conn
}

func (c *natsClient) validateSubject(subject string) error {
	if c == nil || c.conn == nil {
		return fnats.ErrClosed
	}
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(subject) != subject {
		return fmt.Errorf("nats: invalid subject %q", subject)
	}
	return nil
}

func (c *natsClient) validateSubscription(subject, queue string, handler fnats.MsgHandler) error {
	if err := c.validateSubject(subject); err != nil {
		return err
	}
	if handler == nil {
		return fmt.Errorf("nats: subscription handler is nil")
	}
	if queue != "" && (strings.TrimSpace(queue) == "" || strings.TrimSpace(queue) != queue) {
		return fmt.Errorf("nats: invalid queue %q", queue)
	}
	return nil
}

func invokeNatsHandler(handler fnats.MsgHandler, msg *fnats.Msg) {
	defer func() {
		if recovered := recover(); recovered != nil {
			subject := ""
			if msg != nil {
				subject = msg.Subject
			}
			slog.Error("nats: subscription handler panic", "subject", subject, "panic", recovered)
			metrics.IncCounter("nats.subscription.handler_panic.total", nil, 1)
		}
	}()
	handler(msg)
}

var _ fnats.IClient = (*natsClient)(nil)
