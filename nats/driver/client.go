package driver

import (
	"context"
	"errors"
	"fmt"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	"log/slog"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/metrics"

	gonats "github.com/nats-io/nats.go"
)

const (
	publishRetries   = 3
	publishRetryWait = 20 * time.Millisecond
)

// Client implements fnats.IClient by wrapping nats-io/nats.go.
type Client struct {
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

func NewClient(cfg *fnats.Config, extra ClientOptions) (*Client, error) {
	if cfg == nil || strings.TrimSpace(cfg.URL) == "" {
		return nil, fmt.Errorf("nats: configuration and URL are required")
	}
	state := &natsLifecycleState{}
	opts := buildNatsOptions(cfg, state, extra)
	conn, err := gonats.Connect(cfg.URL, opts...)
	if err != nil {
		return nil, fmt.Errorf("nats: connect %s: %w", cfg.URL, err)
	}
	slog.Info("nats: connected", "url", cfg.URL)
	return &Client{conn: conn, cfg: cfg, state: state}, nil
}

func (c *Client) Publish(subject string, data []byte) error {
	if err := c.validateSubject(subject); err != nil {
		return err
	}
	var err error
	for i := 0; i < publishRetries; i++ {
		err = c.conn.Publish(subject, data)
		if err == nil {
			return nil
		}
		if errors.Is(err, gonats.ErrConnectionClosed) || errors.Is(err, gonats.ErrConnectionDraining) {
			// 连接已关闭 / 正在排空，重试不会成功：立即返回、可 errors.Is 到 fnats.ErrClosed，与 Request
			// 一致（RR-20261006-10；旧实现空等 3 × 20ms 再返回，错误只能 Is 到 gonats 的错误）。
			return fmt.Errorf("nats: publish to %s: %w", subject, closedError(err))
		}
		time.Sleep(publishRetryWait)
	}
	return fmt.Errorf("nats: publish to %s failed after %d retries: %w", subject, publishRetries, err)
}

func (c *Client) Request(subject string, data []byte, timeout time.Duration) ([]byte, error) {
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

func (c *Client) requestWithContext(ctx context.Context, subject string, data []byte) ([]byte, error) {
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

func (c *Client) Subscribe(subject string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
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
		return nil, closedError(err)
	}
	return &subscription{sub: sub}, nil
}

func (c *Client) QueueSubscribe(subject string, queue string, handler fnats.MsgHandler) (fnats.ISubscription, error) {
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
		return nil, closedError(err)
	}
	return &subscription{sub: sub}, nil
}

func (c *Client) Drain() error {
	ctx := context.Background()
	cancel := func() {}
	if c != nil && c.cfg != nil && c.cfg.DrainTimeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, c.cfg.DrainTimeout)
	}
	defer cancel()
	return c.DrainWithContext(ctx)
}

func (c *Client) DrainWithContext(ctx context.Context) error {
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
			c.Close()
			return ctx.Err()
		case <-ticker.C:
		}
	}
	return nil
}

func (c *Client) Close() {
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

// Connected 在 Close（含排空超时后的硬关）之后一直返回 false。不能只看 nats.go 的 IsConnected：排空被硬关
// 打断时，nats.go 的排空协程（drainConnection）在 Close 之后仍会把状态翻回 DRAINING_PUBS、空等一次
// FlushTimeout（最长 5s）再关一次，这段时间 IsConnected 为 true（RR-20261006-26，真实 NATS 上实测）。
// 之后的调用不受影响：DRAINING_PUBS 下 nats.go 返回 ErrConnectionDraining，驱动同样映射为 fnats.ErrClosed。
func (c *Client) Connected() bool {
	return c != nil && c.conn != nil && !(c.state != nil && c.state.closing.Load()) && c.conn.IsConnected()
}

// closedError 把 nats.go 的“连接已关闭 / 正在排空”包成驱动的已关闭错误：errors.Is 同时命中
// fnats.ErrClosed 与 nats.go 的原错误；其他错误原样返回。Close 之后的订阅、JetStream 与 RPC CallAsync
// 经它返回，与 Publish / Request 同一口径（RR-20261006-24；旧实现原样返回 nats.go 的
// ErrConnectionClosed，文本同为 "nats: connection closed"，却 errors.Is 不到 fnats.ErrClosed）。
// 文本只取 nats.go 的原错误（它与 fnats.ErrClosed 同为 "nats: connection closed"，用 %w: %w 拼接会重复一遍）。
func closedError(err error) error {
	if err == nil || errors.Is(err, fnats.ErrClosed) {
		return err
	}
	if errors.Is(err, gonats.ErrConnectionClosed) || errors.Is(err, gonats.ErrConnectionDraining) {
		return connectionClosedError{cause: err}
	}
	return err
}

// connectionClosedError 同时 errors.Is 到 fnats.ErrClosed 与 nats.go 的原错误，文本是原错误的文本。
type connectionClosedError struct{ cause error }

func (e connectionClosedError) Error() string   { return e.cause.Error() }
func (e connectionClosedError) Unwrap() []error { return []error{fnats.ErrClosed, e.cause} }

func (c *Client) wrapError(err error) error {
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

// conn returns the underlying gonats.Conn (for RPCClient internal use).
func (c *Client) natsConn() *gonats.Conn {
	if c == nil {
		return nil
	}
	return c.conn
}

func (c *Client) validateSubject(subject string) error {
	if c == nil || c.conn == nil {
		return fnats.ErrClosed
	}
	if strings.TrimSpace(subject) == "" || strings.TrimSpace(subject) != subject {
		return fmt.Errorf("nats: invalid subject %q", subject)
	}
	return nil
}

func (c *Client) validateSubscription(subject, queue string, handler fnats.MsgHandler) error {
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

var _ fnats.IClient = (*Client)(nil)
