package jslogproto

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// Client 是发起方 + 终态观察者：启动意图写到分区 in.start，终态从 <p>.*.ev.done 的有序消费者读
// （只读已落库的事件；不用核心订阅——被期望序号拒掉的发布也会送到核心订阅者）。
//
// 启动尝试身份（意图的 MsgID）= start.<saga ID>.<实例随机标识>.<实例内序号>，必须全局唯一（RV-20261028-01）：
// 流按 MsgID 在去重窗口（2 分钟）内去重，身份一旦和窗口里的另一次尝试相同，意图就被吞掉、PubAck 照样成功，
// 等待者永远等不到。只用实例内序号不够——Client 重建或另一个 Client 的序号都从 1 开始，被拒之后用同一个 saga ID 重试会撞上。
// 同一次 StartIntent 里的网络重发沿用同一个身份，靠流去重保证只追加一条。同一 saga ID 是否已经在跑由协调器判定（DupStarts），不靠 MsgID。
//
// 等待者（RV-20261028-02）：同一个 ID 的每一次 StartIntent 都登记自己的通道，下一个被观察到的终态（版本大于此前观察到的）
// 唤醒该 ID 的全部等待者。latest 只接受版本更大的终态，所以旧一轮的拒绝回执（版本不大于已观察到的）不会结束新一轮。
// 局限：新 Client 回放完历史终态之前对一个 ID 发起，回放中的旧终态会提前唤醒它（调用方看到旧的 Rejected 再重试即可，不丢不重）。
type Client struct {
	js     jetstream.JetStream
	l      Layout
	mu     sync.Mutex
	latest map[string]*Saga
	seenAt map[string]time.Time
	wait   map[string][]chan struct{} // saga ID → 等下一个终态的调用方
	ccs    []jetstream.ConsumeContext
	Pubs   atomic.Uint64
	admit  atomic.Pointer[AdmitState]
	stopKV func()
	nonce  atomic.Uint64
	inst   string // 本实例的随机标识（128 位，rand.Text）：启动尝试身份的一部分

	// dropAck 返回真时，这次启动意图照常发出、但不等回复，按超时返回（结果未知、实际已落库）。测试用。
	dropAck func() bool
}

// ErrStartIdentityReused：启动意图第一次发布就被流按 MsgID 判成重复——启动尝试身份撞上了去重窗口里的另一次尝试，
// 意图没有入流。不可能在正常运行中出现（身份全局唯一）；出现说明身份生成坏了，不能当成功返回。
var ErrStartIdentityReused = errors.New("jslogproto: start attempt identity already used within the stream's duplicate window")

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
	c := &Client{js: js, l: l, latest: map[string]*Saga{}, seenAt: map[string]time.Time{}, wait: map[string][]chan struct{}{}, inst: rand.Text()}
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
		// 走到这里的终态版本大于此前观察到的任何版本，也就大于每位等待者登记时看到的版本：全部唤醒。
		for _, ch := range c.wait[ev.Saga.ID] {
			close(ch)
		}
		delete(c.wait, ev.Saga.ID)
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
//
// 返回的通道在这个 ID 下一个被观察到的终态到达时关闭；已经到过非 Rejected 终态的直接返回已关闭的通道。
// ctx 只管发布：发布失败（返回错误）时撤掉本次登记的等待者；返回通道之后调用方不再等（ctx 取消）时不撤，
// 通道照常在终态到达时关闭并被清理。被拒（Rejected）之后再发起，等的是比那次拒绝更新的终态。
func (c *Client) StartIntent(ctx context.Context, id string, data TransferData) (<-chan struct{}, error) {
	ch := make(chan struct{})
	c.mu.Lock()
	if old, done := c.latest[id]; done && old.Status != StatusRejected {
		close(ch)
	} else {
		c.wait[id] = append(c.wait[id], ch)
	}
	c.mu.Unlock()
	raw, _ := json.Marshal(StartRequest{V: WireVersion, ID: id, Data: data})
	subject := c.l.StartSubject(c.l.PartitionOf(id))
	msgID := fmt.Sprintf("start.%s.%s.%d", id, c.inst, c.nonce.Add(1))
	maybeStored := false // 之前某次发送结果未知（可能已落库）：之后的 Duplicate 是正常结果
	for {
		var ack *jetstream.PubAck
		var err error
		if c.dropAck != nil && c.dropAck() {
			err = c.publishDroppingAck(subject, msgID, raw)
		} else {
			pctx, cancel := context.WithTimeout(ctx, 5*time.Second)
			ack, err = c.js.Publish(pctx, subject, raw, jetstream.WithMsgID(msgID))
			cancel()
		}
		c.Pubs.Add(1)
		if err == nil {
			if ack != nil && ack.Duplicate && !maybeStored {
				c.dropWaiter(id, ch)
				return nil, fmt.Errorf("start %s (%s): %w", id, msgID, ErrStartIdentityReused)
			}
			return ch, nil
		}
		maybeStored = true
		if ctx.Err() != nil {
			c.dropWaiter(id, ch)
			return nil, fmt.Errorf("start %s: %w", id, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// dropWaiter 撤掉一次失败发起登记的等待者（按通道身份找）。
func (c *Client) dropWaiter(id string, ch chan struct{}) {
	c.mu.Lock()
	defer c.mu.Unlock()
	ws := c.wait[id]
	for i, w := range ws {
		if w == ch {
			ws = append(ws[:i], ws[i+1:]...)
			break
		}
	}
	if len(ws) == 0 {
		delete(c.wait, id)
	} else {
		c.wait[id] = ws
	}
}

// publishDroppingAck 照常发出启动意图（带同一个 MsgID）但不等 PubAck，按超时返回：模拟“已落库、回复丢了”。
func (c *Client) publishDroppingAck(subject, msgID string, raw []byte) error {
	m := nats.NewMsg(subject)
	m.Header.Set(jetstream.MsgIDHeader, msgID)
	m.Data = raw
	if err := c.js.Conn().PublishMsg(m); err != nil {
		return err
	}
	if err := c.js.Conn().Flush(); err != nil {
		return err
	}
	time.Sleep(20 * time.Millisecond)
	return context.DeadlineExceeded
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
