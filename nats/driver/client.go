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
//
// 已关闭判据（REFACTOR-2026-10-06-nats-driver-closed-state）：Client 自己持有唯一的“已关闭”状态
// state.closed，Close 一开始就置位，排空正常结束时也置位。Client、Assembly、JetStreamClient、
// RPCClient 与订阅句柄的公开方法入口先经 admit 查它（关闭类的 Close / Stop 幂等返回 nil，状态查询返回
// false），已关闭直接返回 fnats.ErrClosed、不碰 nats.go；
// 调用过了入口、在 nats.go 里失败时，wrapError 也先看它——这期间 Close 了就是 fnats.ErrClosed。所以与
// Close 并发的调用要么在 Close 之前完成，要么返回 fnats.ErrClosed。nats.go 的连接状态与错误只在未关闭时
// 作辅助分类（排空中、nats.go 自己放弃重连后关闭）。
type Client struct {
	conn  *gonats.Conn
	cfg   *fnats.Config
	state *natsLifecycleState

	// testAfterAdmit 只给测试用（生产为 nil）：调用通过 admit 的已关闭检查之后、碰 nats.go 之前执行，
	// 用来确定性地让 Close 插进“已过入口、还没调 nats.go”的窗口。
	testAfterAdmit func()
}

// natsLifecycleState 是驱动自己的连接生命周期状态，与 nats.go 的回调共享（回调在 Client 之前创建）。
// closed 是唯一的已关闭判据；draining 只用来把排空 / 关闭引起的断开记成 Info 日志。
type natsLifecycleState struct {
	draining atomic.Bool
	closed   atomic.Bool
}

func (s *natsLifecycleState) isClosed() bool {
	return s != nil && s.closed.Load()
}

// markClosed 置已关闭，返回是否由这次调用置位（nil 状态只在测试里出现，按置位成功处理）。
func (s *natsLifecycleState) markClosed() bool {
	return s == nil || s.closed.CompareAndSwap(false, true)
}

func (s *natsLifecycleState) expectedDisconnect() bool {
	return s != nil && (s.draining.Load() || s.closed.Load())
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

// closed 是驱动的已关闭判据：Close 已开始或排空已完成；nil Client 与没有连接的 Client 同样算已关闭。
func (c *Client) closed() bool {
	return c == nil || c.conn == nil || c.state.isClosed()
}

// admit 是每个公开方法的入口检查：已关闭返回 fnats.ErrClosed，不碰 nats.go。
func (c *Client) admit() error {
	if c.closed() {
		return fnats.ErrClosed
	}
	if c.testAfterAdmit != nil {
		c.testAfterAdmit()
	}
	return nil
}

func (c *Client) Publish(subject string, data []byte) error {
	if err := c.validateSubject(subject); err != nil {
		return err
	}
	var err error
	for i := 0; i < publishRetries; i++ {
		err = c.wrapError(c.conn.Publish(subject, data))
		if err == nil {
			return nil
		}
		if errors.Is(err, fnats.ErrClosed) {
			// 已关闭 / 正在排空，重试不会成功：立即返回（RR-20261006-10）。
			return fmt.Errorf("nats: publish to %s: %w", subject, err)
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
		switch {
		case c.state.isClosed():
			return nil, fnats.ErrClosed
		case ctx.Err() != nil:
			// 保留 ctx 语义（RR-20261006-73）：之前只回 fnats.ErrTimeout / ErrCancelled，调用方用
			// errors.Is(err, context.DeadlineExceeded / Canceled) 判断不到。
			return nil, contextRPCError(ctx)
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
		return nil, c.wrapError(err)
	}
	return &subscription{sub: sub, client: c}, nil
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
		return nil, c.wrapError(err)
	}
	return &subscription{sub: sub, client: c}, nil
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

// DrainWithContext 排空连接：正常结束时由它置已关闭并返回 nil；ctx 先结束就 Close（硬关）并返回
// ctx 错误；排空期间被别人 Close 返回 fnats.ErrClosed（连接没排空）。已关闭时返回 fnats.ErrClosed。
func (c *Client) DrainWithContext(ctx context.Context) error {
	if err := c.admit(); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if c.state != nil {
		c.state.draining.Store(true)
	}
	if err := c.conn.Drain(); err != nil {
		return c.wrapError(err)
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
	if !c.state.markClosed() {
		return fnats.ErrClosed // 排空期间被 Close 硬关
	}
	return nil
}

// Close 先置已关闭、再关 nats.go 连接；之后 Connected 一直为 false、其他调用一直返回 fnats.ErrClosed。
// nats.go 在硬关打断排空后会把状态翻回 DRAINING_PUBS、约 5s 内 IsConnected 为 true（RR-20261006-26），
// 驱动不看它。
func (c *Client) Close() {
	if c == nil {
		return
	}
	c.state.markClosed()
	if c.conn != nil {
		c.conn.Close()
	}
}

// Connected 已关闭时返回 false，未关闭时才看 nats.go 的连接状态。
func (c *Client) Connected() bool {
	return !c.closed() && c.conn.IsConnected()
}

// wrapError 把 nats.go 的失败翻译成驱动契约的错误。先看驱动自己的状态：已关闭（调用过了入口检查之后
// 才 Close）一律 fnats.ErrClosed，不管 nats.go 返回了什么。未关闭时按 nats.go 的错误辅助分类：
// 超时 / 无响应者映射到 fnats 的对应错误；连接已关闭 / 正在排空（排空进行中，或 nats.go 放弃重连后
// 自己关闭）映射到 fnats.ErrClosed；其他原样返回。
func (c *Client) wrapError(err error) error {
	switch {
	case err == nil:
		return nil
	case c != nil && c.state.isClosed():
		return fnats.ErrClosed
	case errors.Is(err, gonats.ErrTimeout):
		return fnats.ErrTimeout
	case errors.Is(err, gonats.ErrNoResponders):
		return fnats.ErrNoResponders
	case errors.Is(err, gonats.ErrConnectionClosed), errors.Is(err, gonats.ErrConnectionDraining):
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
	if err := c.admit(); err != nil {
		return err
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
