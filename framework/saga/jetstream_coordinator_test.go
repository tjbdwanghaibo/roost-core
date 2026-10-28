package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	e     *JetStreamEngine
	p     *partition
	raw   *stubRaw
	clock *testClock
	seq   uint64
	// events 是 install 过的全部事件（按提交顺序）。
	events []logEvent
}

func newUnitPartition(t *testing.T, configure func(*JetStreamOptions)) *unitPartition {
	t.Helper()
	clock := &testClock{now: time.Date(2026, 10, 28, 12, 0, 0, 0, time.UTC)}
	options := JetStreamOptions{Layout: Layout{Prefix: "unit.saga", Stream: "UNIT_SAGA", Partitions: 1, Groups: 1}, Owned: []int{0}, Writer: "unit", Clock: clock.Now}
	options.Admission.Enabled = false
	if configure != nil {
		configure(&options)
	}
	raw := &stubRaw{}
	e, err := NewJetStreamEngine(stubLog{}, raw, options)
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
	return r
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
	raw, _ := json.Marshal(startInput{V: LogWireVersion, Request: request, Attempt: attempt, Digest: digest})
	msg := fnats.LogMsg{Subject: u.e.layout.StartSubject(0), Data: raw, MsgID: "start." + attempt}
	if reply != "" {
		msg.Headers = map[string]string{replyHeader: reply}
	}
	return msg
}

func (u *unitPartition) resultInput(c Completion) fnats.LogMsg {
	raw, _ := json.Marshal(resultInput{V: LogWireVersion, Completion: c})
	return fnats.LogMsg{Subject: u.e.layout.ResultSubject(0), Data: raw, MsgID: "res." + c.CommandID}
}

func (u *unitPartition) opInput(in opInput, reply string) fnats.LogMsg {
	in.V = LogWireVersion
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

func TestPartitionStartIsDispatchedInTheSameRoundAsOneEvent(t *testing.T) {
	u := newUnitPartition(t, nil)
	at := u.clock.Now()
	r := u.round(at, u.startInput("bk-1", []byte("d"), "a1", "reply.a1"))
	id := IDOf("rally", "bk-1")
	if len(r.events) != 1 || r.events[0].Command == nil || r.events[0].Kind != "dispatch" {
		t.Fatalf("events = %+v, want one dispatch event carrying the command (start collapsed into it)", r.events)
	}
	cmd := r.events[0].Command
	if cmd.SagaID != id || cmd.Partition != 0 || cmd.Topic != "reserve" || cmd.Attempt != 1 || cmd.ID != commandID(operationKey(id, PhaseForward, 0), 0, 1) {
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
	u := newUnitPartition(t, func(o *JetStreamOptions) { o.Admission.Enabled = true })
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
		{Subject: u.e.layout.OpSubject(0), Data: []byte(`{"v":2,"kind":"resume"}`)},
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
	u := newUnitPartition(t, func(o *JetStreamOptions) { o.TerminalRetention = time.Minute })
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
	manual := &sagaState{Record: Record{ID: IDOf("rally", "manual"), Type: "rally", DefinitionVersion: 9, BusinessKey: "manual", Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: at, CreatedAt: at, UpdatedAt: at}}
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
	u := newUnitPartition(t, func(o *JetStreamOptions) {
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

func TestJetStreamOptionsValidation(t *testing.T) {
	base := func() JetStreamOptions {
		return JetStreamOptions{Layout: Layout{Prefix: "a.b", Stream: "S", Partitions: 16, Groups: 4}}
	}
	cases := map[string]struct {
		mutate func(*JetStreamOptions)
		want   string
	}{
		"defaults":                    {func(*JetStreamOptions) {}, ""},
		"50 partitions on one stream": {func(o *JetStreamOptions) { o.Layout.Partitions, o.Layout.Groups = 50, 1 }, ""},
		"51 partitions on one stream": {func(o *JetStreamOptions) { o.Layout.Partitions, o.Layout.Groups = 51, 1 }, "at most 50 atomic batches"},
		"100 over 2 streams":          {func(o *JetStreamOptions) { o.Layout.Partitions, o.Layout.Groups = 100, 2 }, ""},
		"101 over 2 streams":          {func(o *JetStreamOptions) { o.Layout.Partitions, o.Layout.Groups = 101, 2 }, "ceil(101/2)"},
		"groups over partitions":      {func(o *JetStreamOptions) { o.Layout.Partitions, o.Layout.Groups = 2, 4 }, "stream groups"},
		"bad stream name":             {func(o *JetStreamOptions) { o.Layout.Stream = "A.B" }, "stream"},
		"query prefix collision":      {func(o *JetStreamOptions) { o.Layout.Prefix = "a.q" }, "prefix"},
		"owned out of range":          {func(o *JetStreamOptions) { o.Owned = []int{16} }, "owned partition"},
		"owned twice":                 {func(o *JetStreamOptions) { o.Owned = []int{1, 1} }, "owned partition"},
		"duplicate window too short":  {func(o *JetStreamOptions) { o.DuplicateWindow = 59 * time.Second }, "duplicate window"},
		"publish timeout over budget": {func(o *JetStreamOptions) { o.PublishTimeout = 30 * time.Second }, "publish timeout"},
		"publish timeout over batch":  {func(o *JetStreamOptions) { o.PublishTimeout, o.PublishUnknownBudget = 10*time.Second, time.Minute }, "< 10s"},
		"retention over receipts":     {func(o *JetStreamOptions) { o.TerminalRetention, o.ReceiptRetention = 2*time.Hour, time.Hour }, "terminal retention"},
		"admission low over high":     {func(o *JetStreamOptions) { o.Admission.HighRecords, o.Admission.LowRecords = 10, 10 }, "low_records"},
		"admission stale over ttl":    {func(o *JetStreamOptions) { o.Admission.StaleAfter, o.Admission.SignalTTL = time.Minute, time.Minute }, "signal TTL"},
		"max batch":                   {func(o *JetStreamOptions) { o.MaxBatch = 1001 }, "max batch"},
		"tick":                        {func(o *JetStreamOptions) { o.Tick = time.Second }, "tick"},
		"writer must be a token":      {func(o *JetStreamOptions) { o.Writer = "a.b" }, "writer"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			options := base()
			options.Admission.Enabled = true
			tc.mutate(&options)
			_, err := NewJetStreamEngine(stubLog{}, &stubRaw{}, options)
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
	w.observe(Record{ID: "s1", Status: StatusWaiting, Version: 5}) // 非终态不交付
	w.observe(Record{ID: "s1", Status: StatusFailed, Version: 3})
	w.observe(Record{ID: "s1", Status: StatusCompensated, Version: 7})
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
	e, err := NewJetStreamEngine(stubLog{}, raw, JetStreamOptions{Layout: Layout{Prefix: "w.saga", Stream: "W", Partitions: 1, Groups: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.WaitTerminal(context.Background(), "s1"); !errors.Is(err, ErrClosed) {
		t.Fatalf("WaitTerminal before Start: %v", err)
	}
	e.ready.Store(true) // 单元里没有 Start：终态由用例直接交给观察者
	current := Record{ID: "s1", Status: StatusCompensating, Version: 9}
	raw.answer = func(subject string, data []byte) ([]byte, error) {
		return json.Marshal(queryReply{V: LogWireVersion, Part: 0, Found: true, Record: &current})
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
	e.watch.observe(Record{ID: "s1", Status: StatusFailed, Version: 8})
	select {
	case r := <-results:
		t.Fatalf("an older terminal (v8, before the reopen at v9) woke a waiter: %+v", r)
	case <-time.After(50 * time.Millisecond):
	}
	e.watch.observe(Record{ID: "s1", Status: StatusCompensated, Version: 11})
	for range 2 {
		if r := <-results; r.Version != 11 {
			t.Fatalf("woke with %+v", r)
		}
	}
	current = Record{ID: "s2", Status: StatusCompleted, Version: 4}
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
	raw, _ := json.Marshal(verdictReply{V: LogWireVersion, Attempt: "a", Outcome: verdictRejected, Reason: AdmissionReasonBacklog})
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
