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
	cc     jetstream.ConsumeContext
	Pubs   atomic.Uint64
}

func NewClient(ctx context.Context, js jetstream.JetStream, l Layout) (*Client, error) {
	c := &Client{js: js, l: l, latest: map[string]*Saga{}, seenAt: map[string]time.Time{}, wait: map[string]chan struct{}{}}
	cons, err := js.OrderedConsumer(ctx, l.Stream(), jetstream.OrderedConsumerConfig{FilterSubjects: []string{l.DestFilter("done")}})
	if err != nil {
		return nil, err
	}
	c.cc, err = cons.Consume(func(msg jetstream.Msg) {
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
	return c, err
}

func (c *Client) Close() {
	if c.cc != nil {
		c.cc.Stop()
	}
}

// Start 写启动意图并返回一个在首次看到终态时关闭的通道。
func (c *Client) Start(ctx context.Context, id string, data TransferData) (<-chan struct{}, error) {
	ch := make(chan struct{})
	c.mu.Lock()
	if _, done := c.latest[id]; done {
		close(ch)
	} else {
		c.wait[id] = ch
	}
	c.mu.Unlock()
	raw, _ := json.Marshal(StartRequest{V: WireVersion, ID: id, Data: data})
	for {
		pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		_, err := c.js.Publish(pctx, c.l.StartSubject(c.l.PartitionOf(id)), raw, jetstream.WithMsgID("start."+id))
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
