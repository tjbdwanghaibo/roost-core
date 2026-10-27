package jslogproto

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go/jetstream"
)

// Client 是发起方 + 终态观察者：启动意图写到分区 in.start（MsgID = saga ID，流内去重），终态从 <p>.*.ev.done
// 的有序消费者读（只读已落库的事件；不用核心订阅——被期望序号拒掉的发布也会送到核心订阅者）。
type Client struct {
	js     jetstream.JetStream
	l      Layout
	mu     sync.Mutex
	latest map[string]*Saga
	seenAt map[string]time.Time
	wait   map[string]chan struct{}
	ccs    []jetstream.ConsumeContext
	Pubs   atomic.Uint64
	admit  atomic.Pointer[AdmitState]
	stopKV func()
	nonce  atomic.Uint64
}

// watchAdmission 跟随协调器发布的准入状态（只用于快速失败；协调器处理启动意图时的判定才是权威）。
func (c *Client) watchAdmission(ctx context.Context) error {
	kv, err := c.js.KeyValue(ctx, c.l.LoadBucket())
	if err != nil {
		return err
	}
	w, err := kv.Watch(context.Background(), admitKey)
	if err != nil {
		return err
	}
	c.stopKV = func() { _ = w.Stop() }
	go func() {
		for e := range w.Updates() {
			if e == nil || e.Operation() != jetstream.KeyValuePut {
				continue
			}
			var st AdmitState
			if json.Unmarshal(e.Value(), &st) == nil {
				c.admit.Store(&st)
			}
		}
	}()
	return nil
}

// AdmissionClosed 报告最近一次（3s 内）看到的准入状态是否关闭。
func (c *Client) AdmissionClosed() (bool, string) {
	st := c.admit.Load()
	if st == nil || st.Open || time.Since(time.UnixMilli(st.At)) > 3*time.Second {
		return false, ""
	}
	return true, st.Reason
}

func NewClient(ctx context.Context, js jetstream.JetStream, l Layout) (*Client, error) {
	c := &Client{js: js, l: l, latest: map[string]*Saga{}, seenAt: map[string]time.Time{}, wait: map[string]chan struct{}{}}
	for _, name := range l.Streams() {
		if err := c.watch(ctx, name); err != nil {
			c.Close()
			return nil, err
		}
	}
	if err := c.watchAdmission(ctx); err != nil {
		c.Close()
		return nil, err
	}
	return c, nil
}

func (c *Client) watch(ctx context.Context, stream string) error {
	cons, err := c.js.OrderedConsumer(ctx, stream, jetstream.OrderedConsumerConfig{FilterSubjects: []string{c.l.DestFilter("done")}})
	if err != nil {
		return err
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		var ev Event
		if json.Unmarshal(msg.Data(), &ev) != nil || ev.Saga == nil {
			return
		}
		now := time.Now()
		c.mu.Lock()
		defer c.mu.Unlock()
		if old := c.latest[ev.Saga.ID]; old != nil && old.Version >= ev.Saga.Version {
			return
		}
		c.latest[ev.Saga.ID] = ev.Saga
		if _, ok := c.seenAt[ev.Saga.ID]; !ok {
			c.seenAt[ev.Saga.ID] = now
		}
		if ch := c.wait[ev.Saga.ID]; ch != nil {
			close(ch)
			delete(c.wait, ev.Saga.ID)
		}
	}, jetstream.PullMaxMessages(1024))
	if err != nil {
		return err
	}
	c.ccs = append(c.ccs, cc)
	return nil
}

func (c *Client) Close() {
	for _, cc := range c.ccs {
		cc.Stop()
	}
	if c.stopKV != nil {
		c.stopKV()
	}
}

// Start 写启动意图并返回一个在首次看到终态时关闭的通道。准入关闭时直接返回 ErrBackpressure（不写意图）；
// 意图写进去之后才被协调器拒绝的，终态是 Rejected（Reason 以 backpressure 开头），同样按可重试处理。
func (c *Client) Start(ctx context.Context, id string, data TransferData) (<-chan struct{}, error) {
	if closed, reason := c.AdmissionClosed(); closed {
		return nil, fmt.Errorf("%w (%s)", ErrBackpressure, reason)
	}
	return c.StartIntent(ctx, id, data)
}

// StartIntent 不看准入提示，直接写启动意图（测试协调器侧的权威判定用）。
func (c *Client) StartIntent(ctx context.Context, id string, data TransferData) (<-chan struct{}, error) {
	ch := make(chan struct{})
	c.mu.Lock()
	if old, done := c.latest[id]; done && old.Status != StatusRejected {
		close(ch)
	} else {
		delete(c.latest, id) // 被拒之后再次发起：等新的终态
		c.wait[id] = ch
	}
	c.mu.Unlock()
	raw, _ := json.Marshal(StartRequest{V: WireVersion, ID: id, Data: data})
	// MsgID 带本次调用的序号：同一次调用里的重发靠它去重；被拒之后用同一 saga ID 再发起不会被流的去重窗口吞掉。
	msgID := fmt.Sprintf("start.%s.%d", id, c.nonce.Add(1))
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := c.js.Publish(pctx, c.l.StartSubject(c.l.PartitionOf(id)), raw, jetstream.WithMsgID(msgID))
		cancel()
		c.Pubs.Add(1)
		if err == nil {
			return ch, nil
		}
		if ctx.Err() != nil {
			return nil, fmt.Errorf("start %s: %w", id, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// Latest 返回观察到的最新终态（同一 saga 可能被重开后再次到终态，取版本最大的）。
func (c *Client) Latest() map[string]*Saga {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]*Saga, len(c.latest))
	for id, s := range c.latest {
		out[id] = s
	}
	return out
}
