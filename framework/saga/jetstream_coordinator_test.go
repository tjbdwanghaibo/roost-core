package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// JetStream 协调器（第 2 批，未接线）的单元：分区判定（decide / install）不碰 NATS，用桩接口驱动。真实 JetStream 的端到端在
// jetstream_coordinator_real_integration_test.go。

// stubLog / stubRaw 只实现单元用到的方法；其余方法调用即 panic（嵌入的 nil 接口）。
type stubLog struct{ fnats.JetStreamLog }

type stubRaw struct {
	fnats.RawClient
	mu      sync.Mutex
	replies map[string][][]byte
	answer  func(subject string, data []byte) ([]byte, error)
}

func (r *stubRaw) PublishOnce(subject string, data []byte) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.replies == nil {
		r.replies = map[string][][]byte{}
	}
	r.replies[subject] = append(r.replies[subject], append([]byte(nil), data...))
	return nil
}

func (r *stubRaw) RequestContext(ctx context.Context, subject string, data []byte) ([]byte, error) {
	if r.answer == nil {
		return nil, errors.New("no responders")
	}
	return r.answer(subject, data)
}

func (r *stubRaw) verdicts(subject string) []verdictReply {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []verdictReply
	for _, raw := range r.replies[subject] {
		var v verdictReply
		_ = json.Unmarshal(raw, &v)
		out = append(out, v)
	}
	return out
}

// testClock 是可控时钟。
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *testClock) Set(t time.Time) {
	c.mu.Lock()
	c.now = t
	c.mu.Unlock()
}

func (c *testClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	return c.now
}

// unitPartition 是一个不发布的分区：decide 之后直接 install（事件当作已持久，序号递增）。
type unitPartition struct {
	t     *testing.T
	e     *Engine
	p     *partition
	raw   *stubRaw
	clock *testClock
	seq   uint64
	// events 是 install 过的全部事件（按提交顺序）。
	events []logEvent
}

func newUnitPartition(t *testing.T, configure func(*Options)) *unitPartition {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	options := Options{Layout: Layout{Prefix: "unit.saga", Stream: "UNIT_SAGA", Partitions: 1, Groups: 1}, Owned: []int{0}, Writer: "unit", Clock: clock.Now}
	options.Admission.Enabled = false
	if configure != nil {
		configure(&options)
	}
	raw := &stubRaw{}
	e, err := NewEngine(stubLog{}, raw, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := e.Register(testDefinition()); err != nil {
		t.Fatal(err)
	}
	p := &partition{e: e, k: 0, stream: e.layout.StreamOf(0), writer: e.writer, sagas: map[string]*sagaState{},
		wheel: newWheel(e.opts.Tick, e.opts.WheelSlots, clock.Now()), snapshotSlot: make(chan struct{}, 1), purgeStalls: map[string]purgeStall{}}
	return &unitPartition{t: t, e: e, p: p, raw: raw, clock: clock}
}

// round 判定一批输入（at 时刻）并提交，返回这一轮。
func (u *unitPartition) round(at time.Time, inputs ...fnats.LogMsg) *round {
	u.t.Helper()
	u.clock.Set(at)
	for i := range inputs {
		u.seq++
		inputs[i].Seq = u.seq
	}
	r := u.p.decide(inputs, at)
	if len(r.events) > 0 {
		u.seq++
	}
	// 事件按发布时的样子编码一遍再解码，确认线格式能往返（与 publish 同一编码）。
	for i, ev := range r.events {
		ev.In = r.maxIn
		raw, err := encodeEvent(ev)
		if err != nil {
			u.t.Fatal(err)
		}
		decoded, err := decodeEvent(raw, u.e.layout, 0)
		if err != nil {
			u.t.Fatalf("event %d does not decode: %v", i, err)
		}
		u.events = append(u.events, decoded)
	}
	u.p.install(r, u.seq)
	if err := u.p.checkTimers(); err != nil {
		u.t.Fatalf("after the round at input seq %d: %v", u.seq, err)
	}
	return r
}

// checkTimers 是分区的定时不变量（第 3 批评审建议，只在测试里查，不加热路径开销）：每条非终态 saga 在时间轮里都有一个
// 恰好落在它当前 NextRunAt 的条目。缺了它，这条 saga 再也不会被看一眼（没有输入就停在 waiting / pending）——2026-10-28
// 墙钟 / 单调钟比较导致的卡住就是这一类。陈旧条目（早于或晚于 NextRunAt）不算数：decide 按记录当前的 NextRunAt 判定。
func (p *partition) checkTimers() error {
	entries := make(map[string]map[int64]struct{}, p.wheel.pending())
	for _, slot := range p.wheel.slots {
		for _, entry := range slot {
			if entries[entry.id] == nil {
				entries[entry.id] = map[int64]struct{}{}
			}
			entries[entry.id][entry.at] = struct{}{}
		}
	}
	for id, st := range p.sagas {
		if st.Record.Status.Terminal() {
			continue
		}
		if _, ok := entries[id][st.Record.NextRunAt.UnixNano()]; !ok {
			return fmt.Errorf("partition %d: non-terminal saga %s (%s, next run %s) has no timer entry at its NextRunAt", p.k, id, st.Record.Status, st.Record.NextRunAt.Format(time.RFC3339Nano))
		}
	}
	return nil
}

func (u *unitPartition) startInput(bk string, data []byte, attempt, reply string) fnats.LogMsg {
	u.t.Helper()
	request, err := prepareStart(StartRequest{Type: "rally", BusinessKey: bk, DefinitionVersion: 1, Data: data}, u.e.opts.MaxPayloadBytes)
	if err != nil {
		u.t.Fatal(err)
	}
	digest, err := startIntentDigest(request)
	if err != nil {
		u.t.Fatal(err)
	}
	raw, _ := json.Marshal(startInput{V: WireVersion, Request: request, Attempt: attempt, Digest: digest})
	msg := fnats.LogMsg{Subject: u.e.layout.StartSubject(0), Data: raw, MsgID: "start." + attempt}
	if reply != "" {
		msg.Headers = map[string]string{replyHeader: reply}
	}
	return msg
}

func (u *unitPartition) resultInput(c Completion) fnats.LogMsg {
	raw, _ := json.Marshal(resultInput{V: WireVersion, Completion: c})
	return fnats.LogMsg{Subject: u.e.layout.ResultSubject(0), Data: raw, MsgID: "res." + c.CommandID}
}

func (u *unitPartition) opInput(in opInput, reply string) fnats.LogMsg {
	in.V = WireVersion
	if in.Attempt == "" {
		in.Attempt = fmt.Sprintf("op-%d", u.seq+1)
	}
	raw, _ := json.Marshal(in)
	return fnats.LogMsg{Subject: u.e.layout.OpSubject(0), Data: raw, MsgID: "op." + in.Attempt, Headers: map[string]string{replyHeader: reply}}
}

func (u *unitPartition) record(id string) Record {
	u.t.Helper()
	st := u.p.sagas[id]
	if st == nil {
		u.t.Fatalf("saga %s is not in the partition", id)
	}
	return st.Record
}

// restart 模拟协调器进程重启：换一个新分区，状态只从已提交的事件（u.events，编码后再解码的那份）按序覆盖出来，定时器与
// 剪枝队列按 replayPartition 的规则重排。重启前内存里的任何东西都不带过去。
func (u *unitPartition) restart() {
	u.t.Helper()
	e := u.e
	p := &partition{e: e, k: 0, stream: e.layout.StreamOf(0), writer: e.writer, sagas: map[string]*sagaState{},
		wheel: newWheel(e.opts.Tick, e.opts.WheelSlots, u.clock.Now()), snapshotSlot: make(chan struct{}, 1), purgeStalls: map[string]purgeStall{}}
	for _, ev := range u.events {
		st := ev.Saga.clone()
		p.sagas[st.Record.ID] = st
		p.inHi = max(p.inHi, ev.In)
	}
	p.lastEv = u.seq
	for id, st := range p.sagas {
		switch {
		case !st.Record.Status.Terminal():
			p.wheel.schedule(id, st.Record.NextRunAt)
		case prunable(st.Record.Status):
			p.terminals = append(p.terminals, pruneEntry{id: id, updatedAt: st.Record.UpdatedAt})
		}
	}
	slices.SortFunc(p.terminals, func(a, b pruneEntry) int { return a.updatedAt.Compare(b.updatedAt) })
	u.p = p
	if err := p.checkTimers(); err != nil {
		u.t.Fatalf("after restart: %v", err)
	}
}

func TestPartitionStartIsDispatchedInTheSameRoundAsOneEvent(t *testing.T) {
	u := newUnitPartition(t, nil)
	at := u.clock.Now()
	r := u.round(at, u.startInput("bk-1", []byte("d"), "a1", "reply.a1"))
	id := IDOf("rally", "bk-1")
	if len(r.events) != 1 || r.events[0].Command == nil || r.events[0].Kind != "dispatch" {
		t.Fatalf("events = %+v, want one dispatch event carrying the command (start collapsed into it)", r.events)
	}
	cmd := r.events[0].Command
	if cmd.SagaID != id || cmd.Partition != 0 || cmd.Topic != "reserve" || cmd.Attempt != 1 || cmd.ID != commandID(operationKey(id, u.record(id).Lifecycle, PhaseForward, 0), 0, 1) {
		t.Fatalf("command = %+v", cmd)
	}
	got := u.record(id)
	if got.Status != StatusWaiting || got.Version != 2 || got.StartAttempt != "a1" || got.StartDigest == "" {
		t.Fatalf("record = %+v", got)
	}
	verdicts := u.raw.verdicts("reply.a1")
	if len(verdicts) != 1 || verdicts[0].Outcome != verdictAccepted || verdicts[0].Record == nil || verdicts[0].Record.ID != id || verdicts[0].Attempt != "a1" {
		t.Fatalf("verdicts = %+v", verdicts)
	}
}

func TestPartitionStartIdentityIsDecidedByTheStartDigest(t *testing.T) {
	u := newUnitPartition(t, nil)
	at := u.clock.Now()
	u.round(at, u.startInput("bk-1", []byte("d"), "a1", "reply.a1"))
	r := u.round(at.Add(time.Millisecond), u.startInput("bk-1", []byte("d"), "a2", "reply.a2"), u.startInput("bk-1", []byte("other"), "a3", "reply.a3"))
	if len(r.events) != 0 {
		t.Fatalf("a repeated or conflicting start must not write an event: %+v", r.events)
	}
	if v := u.raw.verdicts("reply.a2"); len(v) != 1 || v[0].Outcome != verdictExisting || v[0].Record == nil || v[0].Record.StartAttempt != "a1" {
		t.Fatalf("same intent verdicts = %+v", v)
	}
	if v := u.raw.verdicts("reply.a3"); len(v) != 1 || v[0].Outcome != verdictIdentityConflict {
		t.Fatalf("other intent verdicts = %+v", v)
	}
	if _, err := verdictResult(u.raw.verdicts("reply.a3")[0]); !errors.Is(err, ErrIdentityConflict) {
		t.Fatalf("verdictResult = %v", err)
	}
}

// 准入关闭（初始化 fail-closed）：拒绝只回复触发它的那次尝试，不写事件、不建记录（选项 R-A），所以同 ID 的等待者不会被它唤醒。
func TestPartitionAdmissionRejectionAnswersOnlyTheTriggeringAttempt(t *testing.T) {
	u := newUnitPartition(t, func(o *Options) { o.Admission.Enabled = true })
	id := IDOf("rally", "bk-1")
	waiter, cancel := u.e.watch.register(id)
	defer cancel()
	at := u.clock.Now()
	r := u.round(at, u.startInput("bk-1", nil, "a1", "reply.a1"))
	if len(r.events) != 0 || u.p.sagas[id] != nil {
		t.Fatalf("a rejected start must not write: events %+v", r.events)
	}
	v := u.raw.verdicts("reply.a1")
	if len(v) != 1 || v[0].Outcome != verdictRejected || v[0].Reason != AdmissionReasonNotReady {
		t.Fatalf("verdicts = %+v", v)
	}
	if _, err := verdictResult(v[0]); !errors.Is(err, ErrBackpressure) || !strings.Contains(err.Error(), AdmissionReasonNotReady) {
		t.Fatalf("verdictResult = %v", err)
	}
	if got := u.raw.verdicts("reply.other"); len(got) != 0 {
		t.Fatalf("another attempt received %+v", got)
	}
	select {
	case <-waiter.signal:
		t.Fatal("a rejection woke a WaitTerminal waiter")
	default:
	}
	// 初始快照读完、没有信号 → 打开，同一业务键的下一次尝试被接受。
	u.e.gate.markReady()
	u.round(at.Add(time.Millisecond), u.startInput("bk-1", nil, "a2", "reply.a2"))
	if v := u.raw.verdicts("reply.a2"); len(v) != 1 || v[0].Outcome != verdictAccepted {
		t.Fatalf("after ready: %+v", v)
	}
	if u.e.Stats().AdmissionRejected != 1 {
		t.Fatalf("stats = %+v", u.e.Stats())
	}
}

func TestPartitionResultDuplicateAndIdentityConflict(t *testing.T) {
	u := newUnitPartition(t, nil)
	at := u.clock.Now()
	u.round(at, u.startInput("bk-1", []byte("d"), "a1", ""))
	id := IDOf("rally", "bk-1")
	waiting := u.record(id)
	ok := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: id, Success: true, Data: []byte("r")}
	r := u.round(at.Add(time.Millisecond), u.resultInput(ok))
	if len(r.events) != 1 || r.events[0].Command == nil || r.events[0].Command.Topic != "march" {
		t.Fatalf("result + next dispatch = %+v", r.events)
	}
	if tomb := u.p.sagas[id].Ops[waiting.OperationKey]; tomb.Closure != OperationClosedWithResult {
		t.Fatalf("tombstone = %+v", tomb)
	}
	r = u.round(at.Add(2*time.Millisecond), u.resultInput(ok))
	if len(r.events) != 0 || u.e.Stats().Duplicates != 1 {
		t.Fatalf("redelivered result: events %+v stats %+v", r.events, u.e.Stats())
	}
	conflicting := ok
	conflicting.Data = []byte("different")
	r = u.round(at.Add(3*time.Millisecond), u.resultInput(conflicting))
	if len(r.events) != 0 || u.e.Stats().RejectedInputs != 1 {
		t.Fatalf("conflicting result: events %+v stats %+v", r.events, u.e.Stats())
	}
	// 接收为当前结果的路径上也查回执（存储 Apply 的回执检查）：第 1 步一次尝试的可重试失败被接收（记录停在这一步退避），
	// 同一 CommandID 再送来一个成功——判定会接收它，回执检查把它判为身份冲突，不推进。
	march := u.record(id)
	retry := Completion{CommandID: march.CommandID, IdempotencyKey: march.OperationKey, SagaID: id, Retryable: true, Error: "busy"}
	u.round(at.Add(4*time.Millisecond), u.resultInput(retry))
	if got := u.record(id); got.Status != StatusPending || got.Attempt != 1 {
		t.Fatalf("after the retryable failure: %+v", got)
	}
	sameAttemptSuccess := Completion{CommandID: march.CommandID, IdempotencyKey: march.OperationKey, SagaID: id, Success: true}
	u.round(at.Add(5*time.Millisecond), u.resultInput(sameAttemptSuccess))
	got := u.record(id)
	if _, closed := u.p.sagas[id].Ops[march.OperationKey]; closed || got.CompletedSteps != 1 || got.Status == StatusCompleted || u.e.Stats().RejectedInputs != 2 {
		t.Fatalf("success for an attempt that already failed was accepted: record %+v stats %+v", got, u.e.Stats())
	}
}

func TestPartitionSkipsUndecodableInputsAndAdvancesTheWatermark(t *testing.T) {
	u := newUnitPartition(t, nil)
	at := u.clock.Now()
	bad := []fnats.LogMsg{
		{Subject: u.e.layout.StartSubject(0), Data: []byte("{"), Headers: map[string]string{replyHeader: "reply.bad"}},
		{Subject: u.e.layout.ResultSubject(0), Data: []byte(`{"v":1,"completion":{}}`)},
		{Subject: u.e.layout.OpSubject(0), Data: []byte(`{"v":3,"kind":"resume"}`)},
	}
	r := u.round(at, bad...)
	if len(r.events) != 0 || u.p.inHi != u.seq || u.e.Stats().RejectedInputs != 3 {
		t.Fatalf("events %+v inHi %d seq %d stats %+v", r.events, u.p.inHi, u.seq, u.e.Stats())
	}
	if v := u.raw.verdicts("reply.bad"); len(v) != 1 || v[0].Outcome != verdictInvalid {
		t.Fatalf("verdicts = %+v", v)
	}
}

func TestPartitionPrunesTerminalsAfterRetentionButKeepsManualRequired(t *testing.T) {
	u := newUnitPartition(t, func(o *Options) { o.TerminalRetention = time.Minute })
	at := u.clock.Now()
	u.round(at, u.startInput("done", nil, "a1", ""), u.startInput("stuck", nil, "a2", ""))
	done, stuck := IDOf("rally", "done"), IDOf("rally", "stuck")
	// done 正向两步都成功 → Completed；stuck 的第 0 步被拒 → 没有已完成步骤 → Failed。再把 stuck 改成 ManualRequired 不容易，
	// 所以第三笔用定义缺失：注册表里没有的版本到期时 fence 到 ManualRequired。
	for step := 0; step < 2; step++ {
		w := u.record(done)
		u.round(at.Add(time.Duration(step+1)*time.Millisecond), u.resultInput(Completion{CommandID: w.CommandID, IdempotencyKey: w.OperationKey, SagaID: done, Success: true}))
	}
	w := u.record(stuck)
	u.round(at.Add(5*time.Millisecond), u.resultInput(Completion{CommandID: w.CommandID, IdempotencyKey: w.OperationKey, SagaID: stuck, Error: "no"}))
	manual := &sagaState{Record: Record{Lifecycle: testLifecycle, ID: IDOf("rally", "manual"), Type: "rally", DefinitionVersion: 9, BusinessKey: "manual", Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: at, CreatedAt: at, UpdatedAt: at}}
	u.p.sagas[manual.Record.ID] = manual
	u.p.wheel.schedule(manual.Record.ID, at)
	u.round(at.Add(6 * time.Millisecond))
	if u.record(done).Status != StatusCompleted || u.record(stuck).Status != StatusFailed || u.record(manual.Record.ID).Status != StatusManualRequired {
		t.Fatalf("setup: %v %v %v", u.record(done).Status, u.record(stuck).Status, u.record(manual.Record.ID).Status)
	}
	u.p.prune(at.Add(30 * time.Second))
	if len(u.p.sagas) != 3 {
		t.Fatalf("pruned inside the retention window: %d left", len(u.p.sagas))
	}
	u.p.prune(at.Add(2 * time.Minute))
	if _, ok := u.p.sagas[done]; ok {
		t.Fatal("Completed past retention was kept")
	}
	if _, ok := u.p.sagas[stuck]; ok {
		t.Fatal("Failed past retention was kept")
	}
	if _, ok := u.p.sagas[manual.Record.ID]; !ok {
		t.Fatal("ManualRequired must stay resident")
	}
	// 剪掉之后同一业务键可以再开一笔（唯一性窗口 = 终态保留期）。
	u.round(at.Add(3*time.Minute), u.startInput("done", nil, "a9", "reply.a9"))
	if v := u.raw.verdicts("reply.a9"); len(v) != 1 || v[0].Outcome != verdictAccepted || v[0].Record.Version != 2 {
		t.Fatalf("restart after prune: %+v", v)
	}
	// 迟到的结果找不到记录：只计数。
	u.round(at.Add(4*time.Minute), u.resultInput(Completion{CommandID: w.CommandID, IdempotencyKey: w.OperationKey, SagaID: stuck, Success: true}))
	if u.e.Stats().UnknownSaga != 1 {
		t.Fatalf("stats = %+v", u.e.Stats())
	}
}

func TestPartitionResumeIsRefusedWhenReceiptsMayHaveExpired(t *testing.T) {
	u := newUnitPartition(t, func(o *Options) {
		o.TerminalRetention = time.Hour
		o.ReceiptRetention = 2 * time.Hour
	})
	at := u.clock.Now()
	u.round(at, u.startInput("bk", nil, "a1", ""))
	id := IDOf("rally", "bk")
	w := u.record(id)
	u.round(at.Add(time.Millisecond), u.resultInput(Completion{CommandID: w.CommandID, IdempotencyKey: w.OperationKey, SagaID: id, Error: "no"}))
	if u.record(id).Status != StatusFailed {
		t.Fatalf("setup status %v", u.record(id).Status)
	}
	late := at.Add(3 * time.Hour)
	u.round(late, u.opInput(opInput{Kind: opResume, ID: id, Resume: &ResumeRequest{ID: id, Now: late}, Now: late, Attempt: "r1"}, "reply.r1"))
	v := u.raw.verdicts("reply.r1")
	if len(v) != 1 || v[0].Outcome != verdictReceiptsExpired {
		t.Fatalf("verdicts = %+v", v)
	}
	if _, err := verdictResult(v[0]); !errors.Is(err, ErrReceiptsExpired) {
		t.Fatalf("verdictResult = %v", err)
	}
	soon := at.Add(time.Minute)
	u.round(soon, u.opInput(opInput{Kind: opResume, ID: id, Resume: &ResumeRequest{ID: id, Now: soon}, Now: soon, Attempt: "r2"}, "reply.r2"))
	if v := u.raw.verdicts("reply.r2"); len(v) != 1 || v[0].Outcome != verdictAccepted || v[0].Record.Incarnation != 1 {
		t.Fatalf("in-window resume: %+v", v)
	}
}

// 回执保留期必须盖住每一步的重试窗口（RV-20261030-02）：参与方 Complete 时剪掉早于 now − 保留期的回执，回执被剪之后同一操作
// 的下一次尝试会再执行一次业务。Register 按补齐后的预算（定义里写的、Options.StepBudgets 的缺省与覆盖）检查，保留期至少是
// 窗口（超时 × 次数 + 退避上限 × (次数 − 1)）的 ReceiptRetentionFactor 倍，不够就拒绝登记。
func TestRegisterRefusesStepsWhoseRetryWindowOutlivesTheReceipts(t *testing.T) {
	u := newUnitPartition(t, func(o *Options) {
		o.TerminalRetention, o.ReceiptRetention = time.Minute, time.Minute
		o.StepBudgets.Defaults = StepBudget{Timeout: 10 * time.Second, MaxAttempts: 10, BackoffMin: 10 * time.Second, BackoffMax: 10 * time.Second}
	})
	step := func(timeout time.Duration, attempts uint32, backoff time.Duration) Definition {
		return Definition{Type: "window", Version: uint32(attempts), Steps: []Step{{Name: "debit", ForwardTopic: "window.debit", Timeout: timeout, MaxAttempts: attempts, BackoffMin: backoff, BackoffMax: backoff}}}
	}
	// 评审给的配置：10s × 10 次 + 10s × 9 次退避 = 190s，回执只留 60s。预算来自 StepBudgets 的缺省（定义里没写）。
	fromDefaults := Definition{Type: "window", Version: 100, Steps: []Step{{Name: "debit", ForwardTopic: "window.debit"}}}
	if err := u.e.Register(fromDefaults); !errors.Is(err, ErrInvalidDefinition) || !strings.Contains(err.Error(), `step "debit"`) || !strings.Contains(err.Error(), "3m10s") {
		t.Fatalf("Register accepted a step whose resolved retry window (190s) outlives the 60s receipt retention: err = %v", err)
	}
	// 定义里写死的预算同样检查：窗口 31s，2 倍 62s > 60s。
	if err := u.e.Register(step(10*time.Second, 2, 11*time.Second)); !errors.Is(err, ErrInvalidDefinition) {
		t.Fatalf("Register accepted a 31s window under a 60s receipt retention (needs 62s): err = %v", err)
	}
	// 恰好 2 倍（窗口 30s）可以登记。
	if err := u.e.Register(step(10*time.Second, 2, 10*time.Second)); err != nil {
		t.Fatalf("a 30s window under a 60s receipt retention was refused: %v", err)
	}
}

func TestJetStreamOptionsValidation(t *testing.T) {
	base := func() Options {
		return Options{Layout: Layout{Prefix: "a.b", Stream: "S", Partitions: 16, Groups: 4}}
	}
	cases := map[string]struct {
		mutate func(*Options)
		want   string
	}{
		"defaults":                    {func(*Options) {}, ""},
		"50 partitions on one stream": {func(o *Options) { o.Layout.Partitions, o.Layout.Groups = 50, 1 }, ""},
		"51 partitions on one stream": {func(o *Options) { o.Layout.Partitions, o.Layout.Groups = 51, 1 }, "at most 50 atomic batches"},
		"100 over 2 streams":          {func(o *Options) { o.Layout.Partitions, o.Layout.Groups = 100, 2 }, ""},
		"101 over 2 streams":          {func(o *Options) { o.Layout.Partitions, o.Layout.Groups = 101, 2 }, "ceil(101/2)"},
		"groups over partitions":      {func(o *Options) { o.Layout.Partitions, o.Layout.Groups = 2, 4 }, "stream groups"},
		"bad stream name":             {func(o *Options) { o.Layout.Stream = "A.B" }, "stream"},
		"query prefix collision":      {func(o *Options) { o.Layout.Prefix = "a.q" }, "prefix"},
		"owned out of range":          {func(o *Options) { o.Owned = []int{16} }, "owned partition"},
		"owned twice":                 {func(o *Options) { o.Owned = []int{1, 1} }, "owned partition"},
		"duplicate window too short":  {func(o *Options) { o.DuplicateWindow = 59 * time.Second }, "duplicate window"},
		"publish timeout over budget": {func(o *Options) { o.PublishTimeout = 30 * time.Second }, "publish timeout"},
		"publish timeout over batch":  {func(o *Options) { o.PublishTimeout, o.PublishUnknownBudget = 10*time.Second, time.Minute }, "< 10s"},
		"retention over receipts":     {func(o *Options) { o.TerminalRetention, o.ReceiptRetention = 2*time.Hour, time.Hour }, "terminal retention"},
		"admission low over high":     {func(o *Options) { o.Admission.HighRecords, o.Admission.LowRecords = 10, 10 }, "low_records"},
		"admission stale over ttl":    {func(o *Options) { o.Admission.StaleAfter, o.Admission.SignalTTL = time.Minute, time.Minute }, "signal TTL"},
		"max batch":                   {func(o *Options) { o.MaxBatch = 1001 }, "max batch"},
		"tick":                        {func(o *Options) { o.Tick = time.Second }, "tick"},
		"writer must be a token":      {func(o *Options) { o.Writer = "a.b" }, "writer"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			options := base()
			options.Admission.Enabled = true
			tc.mutate(&options)
			_, err := NewEngine(stubLog{}, &stubRaw{}, options)
			switch {
			case tc.want == "" && err != nil:
				t.Fatalf("unexpected error %v", err)
			case tc.want != "" && (err == nil || !strings.Contains(err.Error(), tc.want)):
				t.Fatalf("error %v, want it to mention %q", err, tc.want)
			}
		})
	}
}

func TestTerminalWatchWakesEveryWaiterOfTheIDWithNewerTerminals(t *testing.T) {
	w := newTerminalWatch()
	first, cancelFirst := w.register("s1")
	defer cancelFirst()
	second, cancelSecond := w.register("s1")
	defer cancelSecond()
	other, cancelOther := w.register("s2")
	defer cancelOther()
	w.observe(Record{Lifecycle: testLifecycle, ID: "s1", Status: StatusWaiting, Version: 5}) // 非终态不交付
	w.observe(Record{Lifecycle: testLifecycle, ID: "s1", Status: StatusFailed, Version: 3})
	w.observe(Record{Lifecycle: testLifecycle, ID: "s1", Status: StatusCompensated, Version: 7})
	for name, waiter := range map[string]*terminalWaiter{"first": first, "second": second} {
		if got := waiter.newerThan(4); got == nil || got.Version != 7 || got.Status != StatusCompensated {
			t.Fatalf("%s waiter got %+v", name, got)
		}
		if got := waiter.newerThan(7); got != nil {
			t.Fatalf("%s waiter accepted a terminal not newer than the known version: %+v", name, got)
		}
	}
	if got := other.newerThan(0); got != nil {
		t.Fatalf("waiter of another ID woke: %+v", got)
	}
	cancelFirst()
	w.mu.Lock()
	n := len(w.waiters["s1"])
	w.mu.Unlock()
	if n != 1 {
		t.Fatalf("cancel left %d waiters", n)
	}
}

// WaitTerminal：登记后先 Get；已是终态立即返回；否则只接受版本大于已知版本的终态（重开之前的旧终态不提前唤醒），同一 ID 的
// 多个等待者都被唤醒。
func TestWaitTerminalIgnoresTerminalsOlderThanTheKnownVersion(t *testing.T) {
	raw := &stubRaw{}
	e, err := NewEngine(stubLog{}, raw, Options{Layout: Layout{Prefix: "w.saga", Stream: "W", Partitions: 1, Groups: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WaitTerminal(context.Background(), "s1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitTerminal before Start: %v", err)
	}
	e.ready.Store(true) // 单元里没有 Start：终态由用例直接交给观察者
	current := Record{Lifecycle: testLifecycle, ID: "s1", Status: StatusCompensating, Version: 9}
	raw.answer = func(subject string, data []byte) ([]byte, error) {
		return json.Marshal(queryReply{V: WireVersion, Part: 0, Found: true, Record: &current})
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	results := make(chan Record, 2)
	for range 2 {
		go func() {
			r, err := e.WaitTerminal(ctx, "s1")
			if err != nil {
				t.Error(err)
			}
			results <- r
		}()
	}
	for {
		e.watch.mu.Lock()
		n := len(e.watch.waiters["s1"])
		e.watch.mu.Unlock()
		if n == 2 {
			break
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(20 * time.Millisecond) // 两个等待者都已 Get 完（已知版本 9）
	e.watch.observe(Record{Lifecycle: testLifecycle, ID: "s1", Status: StatusFailed, Version: 8})
	select {
	case r := <-results:
		t.Fatalf("an older terminal (v8, before the reopen at v9) woke a waiter: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	e.watch.observe(Record{Lifecycle: testLifecycle, ID: "s1", Status: StatusCompensated, Version: 11})
	for range 2 {
		if r := <-results; r.Version != 11 {
			t.Fatalf("woke with %+v", r)
		}
	}
	current = Record{Lifecycle: testLifecycle, ID: "s2", Status: StatusCompleted, Version: 4}
	if r, err := e.WaitTerminal(ctx, "s2"); err != nil || r.Version != 4 {
		t.Fatalf("already terminal: %+v %v", r, err)
	}
}

func TestReplyRouterDeliversOnlyToTheMatchingAttempt(t *testing.T) {
	router := replyRouter{waiting: map[string]chan verdictReply{}}
	a, doneA := router.await("a")
	defer doneA()
	b, doneB := router.await("b")
	defer doneB()
	raw, _ := json.Marshal(verdictReply{V: WireVersion, Attempt: "a", Outcome: verdictRejected, Reason: AdmissionReasonBacklog})
	router.deliver(raw)
	router.deliver(raw) // 重复判定：第一条为准，不阻塞
	select {
	case v := <-a:
		if v.Outcome != verdictRejected {
			t.Fatalf("a got %+v", v)
		}
	default:
		t.Fatal("attempt a got nothing")
	}
	select {
	case v := <-b:
		t.Fatalf("attempt b got another attempt's verdict %+v", v)
	default:
	}
}

func TestPurgeBoundKeepsTheLastEventTheReplayStartAndUnackedCommands(t *testing.T) {
	cases := []struct {
		name   string
		view   snapshotView
		floors map[string]uint64
		want   uint64
	}{
		{"input watermark below last event", snapshotView{lastEv: 100, inHi: 90}, nil, 91},
		{"last event below input watermark", snapshotView{lastEv: 100, inHi: 120}, nil, 100},
		{"durable behind", snapshotView{lastEv: 100, inHi: 120}, map[string]uint64{"step-a": 40, "step-b": 99}, 41},
		{"durable caught up", snapshotView{lastEv: 100, inHi: 120}, map[string]uint64{"step-a": 500}, 100},
	}
	for _, tc := range cases {
		if got := purgeBound(tc.view, tc.floors); got != tc.want {
			t.Errorf("%s: purgeBound = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// ---- simEngine：把单元分区包成原协调器的同步 API（StartSaga / Complete / Resume / Compensate / Get / 到期），让原来驱动
// Redis 引擎（内存 Store + 后台循环）的状态机用例原样保留断言、换驱动（设计 §11.1：“保留断言，改写驱动”）。

// simEngine 每个操作就是一轮判定 + 提交（事件当作已持久）；派发的命令进 commands。时间由 clock 决定，不跑后台循环：
// 到期由用例显式 due / advance 触发（原用例等后台循环领取的地方）。
type simEngine struct {
	*unitPartition
	commands chan Command
	attempts int
	// rejected 是本轮分区跳过的结果输入的原因（engineHooks.rejectedInput），Complete 据它还原原协调器的错误返回。
	rejected error
}

func newSimEngine(t *testing.T, configure func(*Options)) *simEngine {
	t.Helper()
	s := &simEngine{unitPartition: newUnitPartition(t, configure), commands: make(chan Command, 1024)}
	s.e.hooks.rejectedInput = func(kind, reason string, err error) {
		if kind == "result" {
			s.rejected = err
		}
	}
	// 所有者查询（Engine.Get / GetByBusinessKey 走核心 NATS 的请求）由这个分区同步回答。
	s.raw.answer = func(_ string, data []byte) ([]byte, error) {
		var request queryRequest
		if err := json.Unmarshal(data, &request); err != nil {
			return nil, err
		}
		s.p.answer(pendingQuery{request: request, reply: "sim.answer"})
		s.raw.mu.Lock()
		defer s.raw.mu.Unlock()
		replies := s.raw.replies["sim.answer"]
		return replies[len(replies)-1], nil
	}
	return s
}

// step 判定并提交一轮，把事件里的命令放进 commands。
func (s *simEngine) step(inputs ...fnats.LogMsg) *round {
	s.t.Helper()
	s.rejected = nil
	r := s.round(s.clock.Now(), inputs...)
	for _, ev := range r.events {
		if ev.Command != nil {
			select {
			case s.commands <- ev.Command.Clone():
			default:
				s.t.Fatal("simEngine: command buffer full")
			}
		}
	}
	return r
}

// advance 把时钟推进 d 并处理到期（原用例里后台循环的一次领取）。
func (s *simEngine) advance(d time.Duration) {
	s.t.Helper()
	s.clock.Advance(d)
	s.step()
}

func (s *simEngine) nextAttempt(prefix string) string {
	s.attempts++
	return fmt.Sprintf("%s-%d", prefix, s.attempts)
}

// StartSaga 与 Engine.StartSaga 同一判定：定义与请求在发起方先查，意图交给分区，返回判定。
func (s *simEngine) StartSaga(_ context.Context, request StartRequest) (Record, error) {
	s.t.Helper()
	if _, ok := s.e.definition(request.Type, request.DefinitionVersion); !ok {
		return Record{}, fmt.Errorf("%w: %s/%d", ErrDefinitionMissing, request.Type, request.DefinitionVersion)
	}
	if !request.Now.IsZero() {
		s.clock.Set(request.Now)
	}
	prepared, err := prepareStart(request, s.e.opts.MaxPayloadBytes)
	if err != nil {
		return Record{}, err
	}
	digest, err := startIntentDigest(prepared)
	if err != nil {
		return Record{}, err
	}
	attempt := s.nextAttempt("start")
	prepared.Now = time.Time{}
	raw, _ := json.Marshal(startInput{V: WireVersion, Request: prepared, Attempt: attempt, Digest: digest})
	k := s.e.layout.PartitionOf(IDOf(prepared.Type, prepared.BusinessKey))
	reply := "reply." + attempt
	s.step(fnats.LogMsg{Subject: s.e.layout.StartSubject(k), Data: raw, MsgID: "start." + attempt, Headers: map[string]string{replyHeader: reply}})
	return s.verdict(reply)
}

func (s *simEngine) verdict(reply string) (Record, error) {
	s.t.Helper()
	verdicts := s.raw.verdicts(reply)
	if len(verdicts) != 1 {
		s.t.Fatalf("simEngine: %d verdicts for %s", len(verdicts), reply)
	}
	return verdictResult(verdicts[0])
}

// Complete 把结果交给分区。原协调器的 Complete 同步返回接收后的记录或拒收原因；分区不回复结果输入，这里按跳过原因还原：
// not_waiting → ErrNotWaiting，identity → ErrIdentityConflict，definition_missing → ErrDefinitionMissing，记录不在 → ErrNotFound；
// 重复返回当前记录、nil（与原协调器相同）。
func (s *simEngine) Complete(_ context.Context, completion Completion) (Record, error) {
	s.t.Helper()
	if completion.Validate() != nil || len(completion.Data) > s.e.opts.MaxPayloadBytes {
		return Record{}, ErrInvalidRecord
	}
	if s.p.sagas[completion.SagaID] == nil {
		s.step(s.resultInput(completion))
		return Record{}, ErrNotFound
	}
	s.step(s.resultInput(completion))
	if s.rejected != nil {
		return Record{}, s.rejected
	}
	return s.record(completion.SagaID), nil
}

func (s *simEngine) Resume(_ context.Context, request ResumeRequest) (Record, error) {
	s.t.Helper()
	request, _, err := prepareResume(request, s.clock.Now())
	if err != nil {
		return Record{}, err
	}
	attempt := s.nextAttempt("op")
	reply := "reply." + attempt
	s.step(s.opInput(opInput{Kind: opResume, ID: request.ID, Resume: &request, Now: request.Now, Attempt: attempt}, reply))
	return s.verdict(reply)
}

func (s *simEngine) Compensate(_ context.Context, id, reason string, now time.Time) (Record, error) {
	s.t.Helper()
	id = strings.TrimSpace(id)
	if !validSubjectToken(id, 128) || len(reason) > 4096 {
		return Record{}, ErrInvalidRecord
	}
	if now.IsZero() {
		now = s.clock.Now()
	}
	attempt := s.nextAttempt("op")
	reply := "reply." + attempt
	s.step(s.opInput(opInput{Kind: opCompensate, ID: id, Reason: reason, Now: now.UTC(), Attempt: attempt}, reply))
	return s.verdict(reply)
}

// Get 读分区内存（所有者的回答）。
func (s *simEngine) Get(_ context.Context, id string) (Record, error) {
	st := s.p.sagas[id]
	if st == nil {
		return Record{}, ErrNotFound
	}
	return st.Record.Clone(), nil
}

// seed 把一条记录直接放进分区（原用例里 store.Create 种的记录），排上它的到期。
func (s *simEngine) seed(record Record) {
	s.t.Helper()
	if err := record.Validate(); err != nil {
		s.t.Fatalf("seed %s: %v", record.ID, err)
	}
	s.p.sagas[record.ID] = &sagaState{Record: record}
	if !record.Status.Terminal() {
		s.p.wheel.schedule(record.ID, record.NextRunAt)
	}
}

// waitCommand 取下一条派发的命令（同步派发：没有就是失败，不等）。
func (s *simEngine) waitCommand() Command {
	s.t.Helper()
	select {
	case command := <-s.commands:
		return command
	default:
		s.t.Fatal("simEngine: no command was dispatched")
		return Command{}
	}
}

// compensatingStep：记录在补偿第 step 步——还没派发（Compensating），或已派发、在等这一步的补偿（Waiting）。分区在同一轮里派发
// 因输入（结果、运维意图）而到期的步骤，所以原协调器断言 Compensating 的地方，分区上常常已经在等补偿命令的结果。
func compensatingStep(record Record, step int) bool {
	return record.Phase == PhaseCompensate && record.Step == step && (record.Status == StatusCompensating || record.Status == StatusWaiting)
}
