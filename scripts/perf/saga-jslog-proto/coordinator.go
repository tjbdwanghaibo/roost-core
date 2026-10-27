package jslogproto

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

// ErrFenced：本实例写分区事件时期望序号对不上——别的写者已推进这个分区。分区立即停下（fail-stop），不重试。
var ErrFenced = errors.New("jslogproto: partition fenced by another writer")

// ErrLeaseLost：所有权租约续不上（被别的实例接管或 KV 不可用）。只影响活性；安全由期望序号保证。
var ErrLeaseLost = errors.New("jslogproto: partition lease lost")

// Setup 建流与两个 KV 桶（快照、所有权）。流 R=replicas、文件存储、允许原子批量、去重窗口 2 分钟。
func Setup(ctx context.Context, js jetstream.JetStream, l Layout, replicas int, leaseTTL time.Duration) error {
	for g := 0; g < l.groups(); g++ {
		if _, err := js.CreateOrUpdateStream(ctx, jetstream.StreamConfig{
			Name: l.StreamOf(g), Subjects: l.streamSubjects(g), Storage: jetstream.FileStorage, Replicas: replicas,
			AllowAtomicPublish: true, Duplicates: 2 * time.Minute, AllowDirect: true,
		}); err != nil {
			return fmt.Errorf("stream: %w", err)
		}
	}
	if _, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: l.SnapshotBucket(), Storage: jetstream.FileStorage, Replicas: replicas, History: 1, MaxValueSize: 8 << 20,
	}); err != nil {
		return fmt.Errorf("snapshot bucket: %w", err)
	}
	if _, err := js.CreateOrUpdateKeyValue(ctx, jetstream.KeyValueConfig{
		Bucket: l.OwnerBucket(), Storage: jetstream.FileStorage, Replicas: replicas, History: 1, TTL: leaseTTL,
	}); err != nil {
		return fmt.Errorf("owner bucket: %w", err)
	}
	return nil
}

// Teardown 删流与桶（压测每档用完清理）。
func Teardown(ctx context.Context, js jetstream.JetStream, l Layout) {
	for _, name := range l.Streams() {
		_ = js.DeleteStream(ctx, name)
	}
	_ = js.DeleteKeyValue(ctx, l.SnapshotBucket())
	_ = js.DeleteKeyValue(ctx, l.OwnerBucket())
}

type CoordinatorOptions struct {
	Layout   Layout
	Instance string
	Budget   Budget
	// MaxBatch 是一轮最多处理的输入条数；一轮产生的全部事件用一次发布（单条或原子批量）提交。
	MaxBatch int
	// SnapshotEvery：每写这么多条事件做一次快照 + 清理（0 关闭）。
	SnapshotEvery int
	// Retention：终态 saga 在内存（与快照）里保留多久，用于迟到结果判定与业务键去重；之后由投影（Mongo）负责。
	Retention time.Duration
	LeaseTTL  time.Duration
	// PurgeGuards：参与方的 durable 消费者名；清理不越过它们的 ack floor（还没取走的命令不能删）。
	PurgeGuards []string
	TickMs      int64
	Log         *slog.Logger

	// 以下只给测试 / 变异用。
	DisableFence  bool           // 变异：发布不带期望序号
	BeforePublish func(part int) // 计算完一轮、发布之前调用（测试用它冻结旧实例）
	// DropAck 返回真时，这次发送照常发出、但不等回复，按超时返回（结果未知、实际已落库）。测试结果未知分支用。
	DropAck func(part int) bool
	Now     func() int64 // 业务时钟（unix ms）
}

func (o *CoordinatorOptions) defaults() {
	if o.MaxBatch == 0 {
		o.MaxBatch = 256
	}
	if o.Budget == (Budget{}) {
		o.Budget = DefaultBudget()
	}
	if o.Retention == 0 {
		o.Retention = 30 * time.Second
	}
	if o.LeaseTTL == 0 {
		o.LeaseTTL = 3 * time.Second
	}
	if o.TickMs == 0 {
		o.TickMs = 5
	}
	if o.Log == nil {
		o.Log = slog.Default()
	}
	if o.Now == nil {
		o.Now = func() int64 { return time.Now().UnixMilli() }
	}
}

// CoordinatorStats 是累计计数（原子读）。
type CoordinatorStats struct {
	EvMsgs, PubCalls, BatchCalls, Unknown, Fenced, Snapshots, Purges, SnapshotConflicts      atomic.Uint64
	Accepted, Duplicate, Ignored, StaleAttempt, LateCompensate, LateAlarm, Starts, DupStarts atomic.Uint64
	ReplayMsgs, ReplayNanos                                                                  atomic.Uint64
	// 结果未知之后的判定：单条重发得到 Duplicate；批重发得到 10201 或序号不符但最后一条是自己的。
	DupAcks, ResolvedByLast atomic.Uint64
}

type Coordinator struct {
	nc    *nats.Conn
	js    jetstream.JetStream
	opts  CoordinatorOptions
	m     machine
	Stats CoordinatorStats

	mu    sync.Mutex
	parts map[int]*partition
	wg    sync.WaitGroup
}

func NewCoordinator(nc *nats.Conn, js jetstream.JetStream, opts CoordinatorOptions) *Coordinator {
	opts.defaults()
	return &Coordinator{nc: nc, js: js, opts: opts, m: machine{budget: opts.Budget}, parts: map[int]*partition{}}
}

type input struct {
	seq  uint64
	kind byte // 's' 启动，'r' 结果
	data []byte
}

type partition struct {
	c      *Coordinator
	k      int
	cancel context.CancelFunc
	done   chan struct{}
	err    atomic.Value // error

	sagas    map[string]*Saga
	lastEv   uint64 // 分区事件过滤（<p>.<k>.ev.>）上最后一条事件的流序号：期望序号
	inHi     uint64 // 已处理到的最大输入序号
	snapRev  uint64
	sinceSnp int
	wheel    *wheel
	inbox    chan input
	pending  []input // 重放时缓冲、尚未处理的输入
	writer   string
	wid      int64
	leaseRev uint64
	seqGen   uint64
}

func (p *partition) stopErr() error {
	if v := p.err.Load(); v != nil {
		return v.(error)
	}
	return nil
}

// Takeover 取得分区所有权（steal 时直接覆盖租约，用于测试 / 运维指定接管），重放，然后开始服务。返回时重放已完成。
func (c *Coordinator) Takeover(ctx context.Context, k int, steal bool) error {
	kv, err := c.js.KeyValue(ctx, c.opts.Layout.OwnerBucket())
	if err != nil {
		return err
	}
	key := c.opts.Layout.SnapshotKey(k)
	var rev uint64
	if steal {
		rev, err = kv.Put(ctx, key, []byte(c.opts.Instance))
	} else {
		rev, err = kv.Create(ctx, key, []byte(c.opts.Instance))
	}
	if err != nil {
		return fmt.Errorf("acquire partition %d: %w", k, err)
	}
	pctx, cancel := context.WithCancel(context.Background())
	p := &partition{c: c, k: k, cancel: cancel, done: make(chan struct{}), sagas: map[string]*Saga{},
		inbox: make(chan input, 1<<16), leaseRev: rev,
		writer: fmt.Sprintf("%s-%d", c.opts.Instance, time.Now().UnixNano()), wid: time.Now().UnixNano()}
	begin := time.Now()
	replayed, err := p.replay(ctx)
	if err != nil {
		cancel()
		return fmt.Errorf("replay partition %d: %w", k, err)
	}
	c.Stats.ReplayMsgs.Add(uint64(replayed))
	c.Stats.ReplayNanos.Add(uint64(time.Since(begin)))
	cons, err := c.js.OrderedConsumer(ctx, c.opts.Layout.StreamOf(k), jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{c.opts.Layout.InFilter(k)}, DeliverPolicy: jetstream.DeliverByStartSequencePolicy,
		OptStartSeq: p.resumeSeq(),
	})
	if err != nil {
		cancel()
		return err
	}
	cc, err := cons.Consume(func(msg jetstream.Msg) {
		meta, err := msg.Metadata()
		if err != nil {
			return
		}
		kind := byte('r')
		if strings.HasSuffix(msg.Subject(), ".in.start") {
			kind = 's'
		}
		select {
		case p.inbox <- input{seq: meta.Sequence.Stream, kind: kind, data: msg.Data()}:
		case <-pctx.Done():
		}
	})
	if err != nil {
		cancel()
		return err
	}
	c.mu.Lock()
	if old := c.parts[k]; old != nil {
		old.cancel()
	}
	c.parts[k] = p
	c.mu.Unlock()
	c.wg.Add(2)
	go func() {
		defer c.wg.Done()
		defer cc.Stop()
		defer close(p.done)
		p.loop(pctx)
	}()
	go func() {
		defer c.wg.Done()
		p.keepLease(pctx, kv, key)
	}()
	return nil
}

// resumeSeq 是 live 消费者的起点：重放读到的最后一条之后。
func (p *partition) resumeSeq() uint64 {
	hi := p.inHi
	if p.lastEv > hi {
		hi = p.lastEv
	}
	for _, in := range p.pending {
		if in.seq > hi {
			hi = in.seq
		}
	}
	return hi + 1
}

func (p *partition) keepLease(ctx context.Context, kv jetstream.KeyValue, key string) {
	t := time.NewTicker(p.c.opts.LeaseTTL / 3)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			rctx, cancel := context.WithTimeout(ctx, p.c.opts.LeaseTTL/3)
			rev, err := kv.Update(rctx, key, []byte(p.c.opts.Instance), p.leaseRev)
			cancel()
			if err != nil {
				if ctx.Err() != nil {
					return
				}
				p.fail(fmt.Errorf("%w: %v", ErrLeaseLost, err))
				return
			}
			p.leaseRev = rev
		}
	}
}

func (p *partition) fail(err error) {
	p.err.CompareAndSwap(nil, err)
	p.cancel()
}

type snapshot struct {
	V      int     `json:"v"`
	Part   int     `json:"part"`
	Seq    uint64  `json:"seq"` // 快照覆盖到的分区流序号（含）
	LastEv uint64  `json:"last_ev"`
	InHi   uint64  `json:"in_hi"`
	Writer string  `json:"writer"`
	At     int64   `json:"at"`
	Sagas  []*Saga `json:"sagas"`
}

// replay 从 KV 快照 + 之后的分区消息重建内存状态。事件按序覆盖 saga；输入先缓冲，结束后只保留 In 水位之后的。
// 读完再核对快照修订号没变（期间若有人写了新快照并清理，就重做），避免读到被清理掉一段的尾巴。
func (p *partition) replay(ctx context.Context) (int, error) {
	l := p.c.opts.Layout
	snapKV, err := p.c.js.KeyValue(ctx, l.SnapshotBucket())
	if err != nil {
		return 0, err
	}
	stream, err := p.c.js.Stream(ctx, l.StreamOf(p.k))
	if err != nil {
		return 0, err
	}
	for round := 0; ; round++ {
		p.sagas, p.lastEv, p.inHi, p.snapRev, p.pending = map[string]*Saga{}, 0, 0, 0, nil
		var from uint64 = 1
		entry, err := snapKV.Get(ctx, l.SnapshotKey(p.k))
		switch {
		case err == nil:
			var snap snapshot
			if err := json.Unmarshal(entry.Value(), &snap); err != nil {
				return 0, fmt.Errorf("decode snapshot: %w", err)
			}
			for _, s := range snap.Sagas {
				p.sagas[s.ID] = s
			}
			p.lastEv, p.inHi, p.snapRev, from = snap.LastEv, snap.InHi, entry.Revision(), snap.Seq+1
		case errors.Is(err, jetstream.ErrKeyNotFound):
		default:
			return 0, err
		}
		end := uint64(0)
		last, err := stream.GetLastMsgForSubject(ctx, l.PartFilter(p.k))
		switch {
		case err == nil:
			end = last.Sequence
		case errors.Is(err, jetstream.ErrMsgNotFound):
		default:
			return 0, err
		}
		n := 0
		if end >= from {
			cons, err := p.c.js.OrderedConsumer(ctx, l.StreamOf(p.k), jetstream.OrderedConsumerConfig{
				FilterSubjects: []string{l.PartFilter(p.k)}, DeliverPolicy: jetstream.DeliverByStartSequencePolicy, OptStartSeq: from,
			})
			if err != nil {
				return 0, err
			}
			it, err := cons.Messages(jetstream.PullMaxMessages(4096))
			if err != nil {
				return 0, err
			}
			for {
				msg, err := it.Next()
				if err != nil {
					it.Stop()
					return n, err
				}
				meta, err := msg.Metadata()
				if err != nil {
					it.Stop()
					return n, err
				}
				seq := meta.Sequence.Stream
				n++
				switch {
				case strings.Contains(msg.Subject(), ".ev."):
					var ev Event
					if err := json.Unmarshal(msg.Data(), &ev); err != nil || ev.V != WireVersion || ev.Saga == nil {
						it.Stop()
						return n, fmt.Errorf("bad event at %d: %v", seq, err)
					}
					// 快照只覆盖到输入水位 InHi，其后、LastEv 之前的事件已在快照里：按版本只前进，不回退。
					if old, ok := p.sagas[ev.Saga.ID]; !ok || ev.Saga.Version > old.Version {
						p.sagas[ev.Saga.ID] = ev.Saga
					}
					p.lastEv = seq
					if ev.In > p.inHi {
						p.inHi = ev.In
					}
				default:
					kind := byte('r')
					if strings.HasSuffix(msg.Subject(), ".in.start") {
						kind = 's'
					}
					p.pending = append(p.pending, input{seq: seq, kind: kind, data: msg.Data()})
				}
				if seq >= end {
					break
				}
			}
			it.Stop()
		}
		kept := p.pending[:0]
		for _, in := range p.pending {
			if in.seq > p.inHi {
				kept = append(kept, in)
			}
		}
		p.pending = kept
		// 快照在重放期间被别人换掉 → 重做。
		entry, err = snapKV.Get(ctx, l.SnapshotKey(p.k))
		if (err == nil && entry.Revision() == p.snapRev) || (errors.Is(err, jetstream.ErrKeyNotFound) && p.snapRev == 0) {
			now := p.c.opts.Now()
			p.wheel = newWheel(p.c.opts.TickMs, 1024, now)
			for _, s := range p.sagas {
				if at := s.dueAt(); at > 0 && !s.Status.Terminal() {
					p.wheel.schedule(s.ID, at)
				}
			}
			return n, nil
		}
		if round > 5 {
			return n, fmt.Errorf("snapshot keeps changing during replay")
		}
	}
}

type outMsg struct {
	subject string
	ev      Event
}

// loop 是分区的单写者循环：取一批输入 / 到期，算出新状态与命令，一次发布提交，PubAck 之后才改内存。
func (p *partition) loop(ctx context.Context) {
	c := p.c
	tick := time.NewTicker(time.Duration(c.opts.TickMs) * time.Millisecond)
	defer tick.Stop()
	var due []string
	for {
		batch := p.pending
		p.pending = nil
		timerFired := false
		if len(batch) == 0 {
			select {
			case <-ctx.Done():
				return
			case in := <-p.inbox:
				batch = append(batch, in)
			case <-tick.C:
				timerFired = true
			}
		}
	drain:
		for len(batch) < c.opts.MaxBatch {
			select {
			case in := <-p.inbox:
				batch = append(batch, in)
			default:
				break drain
			}
		}
		select {
		case <-tick.C:
			timerFired = true
		default:
		}
		now := c.opts.Now()
		overlay := map[string]*Saga{}
		get := func(id string) *Saga {
			if s, ok := overlay[id]; ok {
				return s
			}
			s, ok := p.sagas[id]
			if !ok {
				return nil
			}
			s = s.clone()
			overlay[id] = s
			return s
		}
		var out []outMsg
		emit := func(s *Saga, kind string, cmd *Command) {
			s.Version++
			s.UpdatedAt = now
			dest := "none"
			switch {
			case cmd != nil:
				dest = "acct"
			case s.Status.Terminal():
				dest = "done"
			}
			out = append(out, outMsg{subject: c.opts.Layout.EvSubject(p.k, dest),
				ev: Event{V: WireVersion, Part: p.k, Writer: p.writer, At: now, Kind: kind, Saga: s.clone(), Cmd: cmd}})
		}
		maxIn := p.inHi
		for _, in := range batch {
			if in.seq <= p.inHi {
				continue
			}
			if in.seq > maxIn {
				maxIn = in.seq
			}
			switch in.kind {
			case 's':
				var req StartRequest
				if err := json.Unmarshal(in.data, &req); err != nil || req.V != WireVersion || req.ID == "" {
					c.Stats.Ignored.Add(1)
					continue
				}
				if get(req.ID) != nil {
					c.Stats.DupStarts.Add(1)
					continue
				}
				s, cmd := c.m.start(req.ID, p.k, req.Data, now)
				overlay[s.ID] = s
				c.Stats.Starts.Add(1)
				emit(s, "start", cmd)
			case 'r':
				var res Completion
				if err := json.Unmarshal(in.data, &res); err != nil || res.V != WireVersion {
					c.Stats.Ignored.Add(1)
					continue
				}
				s := get(res.SagaID)
				if s == nil {
					c.Stats.Ignored.Add(1)
					continue
				}
				outcome, cmd := c.m.complete(s, res, now)
				switch outcome {
				case OutcomeAccepted:
					c.Stats.Accepted.Add(1)
					emit(s, "result", cmd)
				case OutcomeLateCompensate:
					c.Stats.LateCompensate.Add(1)
					c.opts.Log.Warn("jslogproto: step succeeded after the coordinator abandoned it; compensating it", "saga_id", s.ID, "cmd_id", res.CmdID, "status", s.Status)
					emit(s, "late_success", cmd)
				case OutcomeLateAlarm:
					c.Stats.LateAlarm.Add(1)
					c.opts.Log.Error("jslogproto: compensation succeeded after the coordinator abandoned it", "saga_id", s.ID, "cmd_id", res.CmdID)
					emit(s, "late_alarm", nil)
				case OutcomeDuplicate:
					c.Stats.Duplicate.Add(1)
				case OutcomeStaleAttempt:
					c.Stats.StaleAttempt.Add(1)
				default:
					c.Stats.Ignored.Add(1)
				}
			}
		}
		if timerFired {
			due = p.wheel.advance(now, due[:0])
			seen := map[string]bool{}
			for _, id := range due {
				if seen[id] {
					continue
				}
				seen[id] = true
				s := get(id)
				if s == nil || s.Status.Terminal() {
					continue
				}
				if changed, cmd := c.m.timer(s, now); changed {
					emit(s, "timer", cmd)
				}
			}
		}
		if len(out) == 0 {
			p.inHi = maxIn
			continue
		}
		for i := range out {
			out[i].ev.In = maxIn
		}
		if c.opts.BeforePublish != nil {
			c.opts.BeforePublish(p.k)
		}
		if ctx.Err() != nil {
			return
		}
		lastSeq, err := p.publish(ctx, out)
		if err != nil {
			if errors.Is(err, ErrFenced) {
				c.Stats.Fenced.Add(1)
			}
			c.opts.Log.Error("jslogproto: partition stops", "part", p.k, "err", err)
			p.fail(err)
			return
		}
		for _, o := range out {
			id := o.ev.Saga.ID
			s := overlay[id]
			p.sagas[id] = s
			if at := s.dueAt(); at > 0 && !s.Status.Terminal() {
				p.wheel.schedule(id, at)
			}
		}
		p.lastEv, p.inHi = lastSeq, maxIn
		c.Stats.EvMsgs.Add(uint64(len(out)))
		p.sinceSnp += len(out)
		if c.opts.SnapshotEvery > 0 && p.sinceSnp >= c.opts.SnapshotEvery {
			if err := p.snapshot(ctx); err != nil {
				c.opts.Log.Error("jslogproto: partition stops after snapshot failure", "part", p.k, "err", err)
				p.fail(err)
				return
			}
		}
	}
}

// publish 用一次发布提交一轮事件：单条用普通发布，多条用原子批量（首条带期望序号）。结果未知（超时、断连）时用同一
// MsgID、同一期望序号重发，按“重复 = 已落库 / 成功 = 这次落库 / 序号不符 = 看最后一条是不是自己的”判定。
func (p *partition) publish(ctx context.Context, out []outMsg) (uint64, error) {
	c := p.c
	p.seqGen++
	base := fmt.Sprintf("%s.%d.%d", p.writer, p.lastEv, p.seqGen)
	msgs := make([]*nats.Msg, len(out))
	for i, o := range out {
		data, err := json.Marshal(o.ev)
		if err != nil {
			return 0, err
		}
		m := nats.NewMsg(o.subject)
		m.Data = data
		m.Header.Set(jetstream.MsgIDHeader, fmt.Sprintf("%s.%d", base, i))
		msgs[i] = m
	}
	if !c.opts.DisableFence {
		msgs[0].Header.Set(jetstream.ExpectedLastSubjSeqSubjHeader, c.opts.Layout.EvFilter(p.k))
		msgs[0].Header.Set(jetstream.ExpectedLastSubjSeqHeader, fmt.Sprint(p.lastEv))
	}
	lastID := msgs[len(msgs)-1].Header.Get(jetstream.MsgIDHeader)
	deadline := time.Now().Add(30 * time.Second)
	for attempt := 0; ; attempt++ {
		// 每次发送换一个批号（服务端可能还留着上一次没发完的半批），MsgID 不变（去重靠它）。
		seq, err := p.send(ctx, msgs, fmt.Sprintf("b%x.%d.%d", p.wid, p.seqGen, attempt))
		if err == nil {
			return seq, nil
		}
		var conflict bool
		var dupBatch bool
		switch code := apiErrorCodeOf(err); code {
		case jetstream.JSErrCodeStreamWrongLastSequence, jetstream.JSErrCodeStreamWrongLastSequenceConstant:
			conflict = true
		case 10201: // atomic publish batch contains duplicate message id：批已落库
			dupBatch = true
		}
		if conflict || dupBatch {
			// 看分区事件过滤上的最后一条是不是这一轮的最后一条：是 → 之前那次已提交；否 → 别人写了，本实例被隔离。
			seq, mine, lerr := p.lastIsMine(ctx, lastID)
			if lerr != nil {
				return 0, lerr
			}
			if mine {
				c.Stats.ResolvedByLast.Add(1)
				return seq, nil
			}
			if dupBatch {
				return 0, fmt.Errorf("batch reported duplicate but last event is not ours")
			}
			return 0, fmt.Errorf("%w: %v", ErrFenced, err)
		}
		c.Stats.Unknown.Add(1)
		if time.Now().After(deadline) || ctx.Err() != nil {
			return 0, fmt.Errorf("publish outcome unknown after retries: %w", err)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (p *partition) send(ctx context.Context, msgs []*nats.Msg, batchID string) (uint64, error) {
	c := p.c
	c.Stats.PubCalls.Add(1)
	pctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if c.opts.DropAck != nil && c.opts.DropAck(p.k) {
		return 0, p.sendDroppingAck(msgs, batchID)
	}
	if len(msgs) == 1 {
		ack, err := c.js.PublishMsg(pctx, msgs[0], jetstream.WithRetryAttempts(0))
		if err != nil {
			return 0, err
		}
		if ack.Duplicate {
			c.Stats.DupAcks.Add(1)
		}
		return ack.Sequence, nil
	}
	c.Stats.BatchCalls.Add(1)
	id := batchID
	for i, m := range msgs {
		m.Header.Set("Nats-Batch-Id", id)
		m.Header.Set("Nats-Batch-Sequence", fmt.Sprint(i+1))
		if i < len(msgs)-1 {
			if err := c.nc.PublishMsg(m); err != nil {
				return 0, err
			}
			continue
		}
		m.Header.Set("Nats-Batch-Commit", "1")
		resp, err := c.nc.RequestMsgWithContext(pctx, m)
		if err != nil {
			return 0, err
		}
		var ack struct {
			Error *jetstream.APIError `json:"error"`
			Seq   uint64              `json:"seq"`
			Count int                 `json:"count"`
		}
		if err := json.Unmarshal(resp.Data, &ack); err != nil {
			return 0, err
		}
		if ack.Error != nil {
			return 0, ack.Error
		}
		if ack.Count != len(msgs) {
			return 0, fmt.Errorf("batch ack count %d, want %d", ack.Count, len(msgs))
		}
		return ack.Seq, nil
	}
	return 0, errors.New("empty batch")
}

// sendDroppingAck 把消息原样发出（批照常带批号与提交头）但不收回复，模拟 PubAck 丢失。
func (p *partition) sendDroppingAck(msgs []*nats.Msg, batchID string) error {
	for i, m := range msgs {
		if len(msgs) > 1 {
			m.Header.Set("Nats-Batch-Id", batchID)
			m.Header.Set("Nats-Batch-Sequence", fmt.Sprint(i+1))
			if i == len(msgs)-1 {
				m.Header.Set("Nats-Batch-Commit", "1")
			}
		}
		if err := p.c.nc.PublishMsg(m); err != nil {
			return err
		}
	}
	_ = p.c.nc.Flush()
	time.Sleep(20 * time.Millisecond)
	return context.DeadlineExceeded
}

func apiErrorCodeOf(err error) jetstream.ErrorCode {
	var apiErr *jetstream.APIError
	if errors.As(err, &apiErr) {
		return apiErr.ErrorCode
	}
	return 0
}

func (p *partition) lastIsMine(ctx context.Context, lastID string) (uint64, bool, error) {
	stream, err := p.c.js.Stream(ctx, p.c.opts.Layout.StreamOf(p.k))
	if err != nil {
		return 0, false, err
	}
	msg, err := stream.GetLastMsgForSubject(ctx, p.c.opts.Layout.EvFilter(p.k))
	if err != nil {
		return 0, false, err
	}
	return msg.Sequence, msg.Header.Get(jetstream.MsgIDHeader) == lastID, nil
}

// snapshot 把分区内存状态写进 KV（按修订号 CAS，只向前），再清理已被快照覆盖、参与方也已确认的旧消息。
// 保留最后一条事件：期望序号检查按“过滤上的最后一条”比较，删光了会变回 0。
func (p *partition) snapshot(ctx context.Context) error {
	c := p.c
	l := c.opts.Layout
	now := c.opts.Now()
	cutoff := now - c.opts.Retention.Milliseconds()
	snap := snapshot{V: WireVersion, Part: p.k, LastEv: p.lastEv, InHi: p.inHi, Writer: p.writer, At: now}
	// Seq 取输入水位：InHi 之后可能还有已送进 inbox、尚未处理的输入（序号小于 LastEv），重放必须从 InHi+1 读起。
	snap.Seq = p.inHi
	for id, s := range p.sagas {
		if s.Status.Terminal() && s.UpdatedAt < cutoff {
			delete(p.sagas, id)
			continue
		}
		snap.Sagas = append(snap.Sagas, s)
	}
	data, err := json.Marshal(snap)
	if err != nil {
		return err
	}
	kv, err := c.js.KeyValue(ctx, l.SnapshotBucket())
	if err != nil {
		return err
	}
	key := l.SnapshotKey(p.k)
	var rev uint64
	if p.snapRev == 0 {
		rev, err = kv.Create(ctx, key, data)
	} else {
		rev, err = kv.Update(ctx, key, data, p.snapRev)
	}
	if err != nil {
		// 修订号对不上：读回来比较 LastEv。别人的快照更新（> 自己）说明本实例已被接管；更旧的就在它之上覆盖。
		c.Stats.SnapshotConflicts.Add(1)
		entry, gerr := kv.Get(ctx, key)
		if gerr != nil {
			return fmt.Errorf("snapshot conflict and reread failed: %v / %v", err, gerr)
		}
		var other snapshot
		if jerr := json.Unmarshal(entry.Value(), &other); jerr != nil {
			return jerr
		}
		if other.LastEv > p.lastEv {
			return fmt.Errorf("%w: newer snapshot (last_ev %d > %d)", ErrFenced, other.LastEv, p.lastEv)
		}
		if rev, err = kv.Update(ctx, key, data, entry.Revision()); err != nil {
			return err
		}
	}
	p.snapRev = rev
	p.sinceSnp = 0
	c.Stats.Snapshots.Add(1)
	bound := snap.Seq + 1
	if p.lastEv < bound {
		bound = p.lastEv
	}
	for _, name := range c.opts.PurgeGuards {
		cons, err := c.js.Consumer(ctx, l.StreamOf(p.k), name)
		if err != nil {
			return nil // 参与方消费者还不在：不清理
		}
		info, err := cons.Info(ctx)
		if err != nil {
			return nil
		}
		if f := info.AckFloor.Stream + 1; f < bound {
			bound = f
		}
	}
	if bound <= 1 {
		return nil
	}
	stream, err := c.js.Stream(ctx, l.StreamOf(p.k))
	if err != nil {
		return err
	}
	if err := stream.Purge(ctx, jetstream.WithPurgeSubject(l.PartFilter(p.k)), jetstream.WithPurgeSequence(bound)); err != nil {
		return fmt.Errorf("purge: %w", err)
	}
	c.Stats.Purges.Add(1)
	return nil
}

// Partition 状态查询（测试 / 压测核对用，读的是循环外的快照，循环在跑时只做近似观察）。
func (c *Coordinator) PartitionErr(k int) error {
	c.mu.Lock()
	p := c.parts[k]
	c.mu.Unlock()
	if p == nil {
		return errors.New("not owned")
	}
	return p.stopErr()
}

// Stop 停止全部分区循环（不释放租约：模拟进程退出时由 TTL 过期）。
func (c *Coordinator) Stop() {
	c.mu.Lock()
	for _, p := range c.parts {
		p.cancel()
	}
	c.mu.Unlock()
	c.wg.Wait()
}

// SnapshotState 在分区停下之后读它的内存状态（测试核对用）。
func (c *Coordinator) SnapshotState(k int) map[string]*Saga {
	c.mu.Lock()
	p := c.parts[k]
	c.mu.Unlock()
	if p == nil {
		return nil
	}
	<-p.done
	out := make(map[string]*Saga, len(p.sagas))
	for id, s := range p.sagas {
		out[id] = s.clone()
	}
	return out
}
