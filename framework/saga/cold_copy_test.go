package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// 冷副本（设计 §8，第 6 批）的单元：投影器的攒批 / 版本只升 / 逐条结算 / 重投幂等，Engine 的回落读只在所有者内存里没有这条
// 记录时发生、不参与任何判定，以及冷副本 durable 是分区清理的守卫。Mongo 上的版本门（管道 upsert）与 kill -9 后的最终一致
// 在 cold_copy_real_integration_test.go。

// ---- 投影器 ----

// coldMemory 是按 coldNewer 门控的内存冷副本：单元里代替 Mongo 的管道 upsert（同一规则，Mongo 一侧由真实用例核对）。
type coldMemory struct {
	mu      sync.Mutex
	records map[string]Record
	batches [][]Record
	fail    int // 接下来这么多次写入失败
}

func (m *coldMemory) write(_ context.Context, records []Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.batches = append(m.batches, append([]Record(nil), records...))
	if m.fail > 0 {
		m.fail--
		return errors.New("injected write failure")
	}
	if m.records == nil {
		m.records = map[string]Record{}
	}
	for _, r := range records {
		if old, ok := m.records[r.ID]; !ok || coldNewer(r, old) {
			m.records[r.ID] = r
		}
	}
	return nil
}

func (m *coldMemory) snapshot() map[string]Record {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make(map[string]Record, len(m.records))
	for k, v := range m.records {
		out[k] = v
	}
	return out
}

// unitColdProjector 是只跑攒批的投影器（不订阅）：用例直接往 items 里放投递。
func unitColdProjector(t *testing.T, batchSize int, store *coldMemory) (*ColdCopyProjector, func()) {
	t.Helper()
	p := &ColdCopyProjector{layout: Layout{Prefix: "unit.saga", Stream: "UNIT_SAGA", Partitions: 1, Groups: 1},
		settings: ColdCopySettings{BatchSize: batchSize, MaxWait: 20 * time.Millisecond}, write: store.write, items: make(chan coldItem, batchSize)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.batch(ctx) }()
	return p, func() { cancel(); <-done }
}

// settlements 记每条投递被结算的次数与结果。
type settlements struct {
	mu      sync.Mutex
	results map[int][]error
}

func (s *settlements) settle(i int) func(error) {
	return func(err error) {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.results == nil {
			s.results = map[int][]error{}
		}
		s.results[i] = append(s.results[i], err)
	}
}

func (s *settlements) wait(t *testing.T, n int) map[int][]error {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		s.mu.Lock()
		got := len(s.results)
		out := make(map[int][]error, got)
		for k, v := range s.results {
			out[k] = append([]error(nil), v...)
		}
		s.mu.Unlock()
		if got >= n {
			return out
		}
		if time.Now().After(deadline) {
			t.Fatalf("only %d of %d deliveries settled", got, n)
		}
		time.Sleep(time.Millisecond)
	}
}

func coldRecord(id string, version uint64, created time.Time, status Status) Record {
	r := Record{Lifecycle: testLifecycle, ID: IDOf("rally", id), Type: "rally", DefinitionVersion: 1, BusinessKey: id, Status: status, Phase: PhaseForward,
		Version: version, CreatedAt: created, UpdatedAt: created.Add(time.Duration(version) * time.Second)}
	if !status.Terminal() {
		r.NextRunAt = r.UpdatedAt
	}
	return r
}

// 一批里同一 saga 只写最新的一条（版本只升；后来的一生盖住前一生），每条投递恰好结算一次、在写入成功之后。
func TestColdCopyProjectorWritesTheNewestRecordPerSagaAndSettlesEachDelivery(t *testing.T) {
	store := &coldMemory{}
	p, stop := unitColdProjector(t, 5, store)
	defer stop()
	t0 := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	later := t0.Add(2 * time.Hour) // 终态被剪掉之后同一业务键再开的一笔：版本从头来，但它更新
	deliveries := []Record{
		coldRecord("a", 3, t0, StatusCompleted),
		coldRecord("a", 5, t0, StatusCompleted),
		coldRecord("a", 4, t0, StatusCompleted),
		coldRecord("b", 9, t0, StatusFailed),
		coldRecord("b", 2, later, StatusCompleted),
	}
	var s settlements
	for i, r := range deliveries {
		p.items <- coldItem{record: r, settle: s.settle(i)}
	}
	got := s.wait(t, len(deliveries))
	for i := range deliveries {
		if len(got[i]) != 1 || got[i][0] != nil {
			t.Fatalf("delivery %d settled %v, want exactly one ack", i, got[i])
		}
	}
	if len(store.batches) != 1 || len(store.batches[0]) != 2 {
		t.Fatalf("batches %+v, want one write of two records (one per saga)", store.batches)
	}
	final := store.snapshot()
	if a := final[IDOf("rally", "a")]; a.Version != 5 {
		t.Fatalf("saga a has version %d, want the newest 5", a.Version)
	}
	if b := final[IDOf("rally", "b")]; !b.CreatedAt.Equal(later) || b.Version != 2 {
		t.Fatalf("saga b = v%d created %s, want the later life (v2)", b.Version, b.CreatedAt)
	}
}

// 写失败时这一批的每条投递都 nak（逐条，带错误），不能有一条被确认：确认了没写进去的事件，冷副本就永远缺它（原型 AckAll
// 的教训）。重投同样的事件（乱序、带旧版本）之后冷副本与一次写成的结果相同。
func TestColdCopyProjectorNaksEveryDeliveryOfAFailedBatchAndRedeliveryIsIdempotent(t *testing.T) {
	store := &coldMemory{fail: 1}
	p, stop := unitColdProjector(t, 3, store)
	defer stop()
	t0 := time.Date(2026, 10, 29, 9, 0, 0, 0, time.UTC)
	deliveries := []Record{coldRecord("a", 2, t0, StatusCompleted), coldRecord("b", 7, t0, StatusCompensated), coldRecord("a", 6, t0, StatusCompleted)}
	var first settlements
	for i, r := range deliveries {
		p.items <- coldItem{record: r, settle: first.settle(i)}
	}
	for i, results := range first.wait(t, len(deliveries)) {
		if len(results) != 1 || results[0] == nil {
			t.Fatalf("delivery %d of a failed write settled %v, want exactly one nak", i, results)
		}
	}
	if len(store.snapshot()) != 0 {
		t.Fatalf("a failed write left records: %+v", store.snapshot())
	}
	// JetStream 重投（顺序不保证；第二次连同一条更旧的重复）：写成功、全部确认、结果只由最新版本决定。
	redelivered := []Record{deliveries[2], deliveries[0], deliveries[1], deliveries[0]}
	var second settlements
	for i, r := range redelivered {
		p.items <- coldItem{record: r, settle: second.settle(i)}
	}
	for i, results := range second.wait(t, len(redelivered)) {
		if len(results) != 1 || results[0] != nil {
			t.Fatalf("redelivery %d settled %v, want one ack", i, results)
		}
	}
	// 再来一遍全部旧版本：冷副本不变。
	var third settlements
	for i, r := range deliveries {
		p.items <- coldItem{record: r, settle: third.settle(i)}
	}
	third.wait(t, len(deliveries))
	final := store.snapshot()
	if final[IDOf("rally", "a")].Version != 6 || final[IDOf("rally", "b")].Version != 7 || len(final) != 2 {
		t.Fatalf("cold copy after redeliveries: %+v", final)
	}
}

// 停止时手里还没写的投递全部 nak（交给重投），不能悄悄丢掉、也不能确认。
func TestColdCopyProjectorNaksPendingDeliveriesOnStop(t *testing.T) {
	store := &coldMemory{}
	p := &ColdCopyProjector{settings: ColdCopySettings{BatchSize: 100, MaxWait: time.Hour}, write: store.write, items: make(chan coldItem, 100)}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); p.batch(ctx) }()
	var s settlements
	for i := range 3 {
		p.items <- coldItem{record: coldRecord(strconv.Itoa(i), 1, time.Now(), StatusCompleted), settle: s.settle(i)}
	}
	time.Sleep(20 * time.Millisecond)
	cancel()
	<-done
	for i, results := range s.wait(t, 3) {
		if len(results) != 1 || !errors.Is(results[0], ErrClosed) {
			t.Fatalf("delivery %d settled %v on stop, want one ErrClosed nak", i, results)
		}
	}
	if len(store.batches) != 0 {
		t.Fatalf("wrote on stop: %+v", store.batches)
	}
}

// 投影器只收本布局的事件：坏信封（解不开、版本不对、subject 不是事件）永久拒绝（Term），好的推迟结算交给批写。
func TestColdCopyProjectorTermsUndecodableEventsAndDefersTheRest(t *testing.T) {
	u := newUnitPartition(t, nil)
	r := u.round(u.clock.Now(), u.startInput("x", nil, "a1", ""))
	if len(r.events) != 1 {
		t.Fatalf("setup: %d events", len(r.events))
	}
	ev := r.events[0]
	ev.In = r.maxIn
	raw, err := encodeEvent(ev)
	if err != nil {
		t.Fatal(err)
	}
	p := &ColdCopyProjector{layout: u.e.layout, items: make(chan coldItem, 1)}
	bad := []*fnats.JetStreamMsg{
		{Subject: u.e.layout.DoneEventSubject(0), Data: []byte("{")},
		{Subject: u.e.layout.DoneEventSubject(0), Data: []byte(`{"v":1}`)},
		{Subject: u.e.layout.StartSubject(0), Data: raw},
		{Subject: "other.0.ev.done", Data: raw},
	}
	for i, msg := range bad {
		if err := p.handle(context.Background(), msg); err == nil || !fnats.IsPermanent(err) {
			t.Fatalf("bad event %d: %v, want a permanent rejection", i, err)
		}
	}
	var settled []error
	good := &fnats.JetStreamMsg{Subject: u.e.layout.CommandEventSubject(0, "reserve"), Data: raw,
		DeferSettle: func() func(error) { return func(err error) { settled = append(settled, err) } }}
	if err := p.handle(context.Background(), good); err != nil {
		t.Fatal(err)
	}
	item := <-p.items
	if item.record.ID != IDOf("rally", "x") || item.record.Version != ev.Saga.Record.Version || len(settled) != 0 {
		t.Fatalf("deferred item %+v, settled %v", item.record, settled)
	}
}

// ---- 回落读 ----

// queryLog 让单元分区回答散播查询（Engine.askAll 走 JetStreamLog.RequestMany）。
type queryLog struct {
	stubLog
	u *unitPartition
}

func (l queryLog) RequestMany(_ context.Context, _ string, data []byte, more func([]byte) bool) error {
	reply, err := l.u.ask(data)
	if err != nil {
		return err
	}
	more(reply)
	return nil
}

var unitReplySeq struct {
	sync.Mutex
	n int
}

// ask 让分区回答一条查询（与 serveQueries 交给循环的同一路径），返回回复。
func (u *unitPartition) ask(data []byte) ([]byte, error) {
	var request queryRequest
	if err := json.Unmarshal(data, &request); err != nil {
		return nil, err
	}
	unitReplySeq.Lock()
	unitReplySeq.n++
	subject := fmt.Sprintf("unit.reply.%d", unitReplySeq.n)
	unitReplySeq.Unlock()
	u.p.answer(pendingQuery{request: request, reply: subject})
	u.raw.mu.Lock()
	defer u.raw.mu.Unlock()
	replies := u.raw.replies[subject]
	if len(replies) != 1 {
		return nil, fmt.Errorf("partition answered %d times", len(replies))
	}
	return replies[0], nil
}

// coldUnit 是一个回答查询的单元分区 + mongotest 上的冷副本（只用 Find / FindOne；版本门的管道 upsert 在真实 Mongo 用例）。
func coldUnit(t *testing.T) (*unitPartition, *ColdCopy, *mongotest.Collection) {
	t.Helper()
	u := newUnitPartition(t, func(o *Options) { o.TerminalRetention = time.Minute })
	client := mongotest.NewClient()
	cold, err := NewColdCopy(client, "saga", "")
	if err != nil {
		t.Fatal(err)
	}
	u.e.opts.ColdCopy = cold
	u.e.log = queryLog{u: u}
	u.raw.answer = func(_ string, data []byte) ([]byte, error) { return u.ask(data) }
	return u, cold, client.Collection("saga", DefaultColdCopyCollection)
}

func seedCold(t *testing.T, coll *mongotest.Collection, records ...Record) {
	t.Helper()
	for _, r := range records {
		if err := r.Validate(); err != nil {
			t.Fatalf("seed %s: %v", r.ID, err)
		}
		if _, err := coll.InsertOne(context.Background(), toColdRecordDoc(r)); err != nil {
			t.Fatal(err)
		}
	}
}

// completeAll 把分区里的 saga 正向两步都做成功（Completed）。
func (u *unitPartition) completeAll(at time.Time, ids ...string) time.Time {
	for step := 0; step < 2; step++ {
		var inputs []fnats.LogMsg
		for _, id := range ids {
			w := u.record(id)
			inputs = append(inputs, u.resultInput(Completion{CommandID: w.CommandID, IdempotencyKey: w.OperationKey, SagaID: id, Success: true}))
		}
		at = at.Add(time.Millisecond)
		u.round(at, inputs...)
	}
	return at
}

// Get / GetByBusinessKey：所有者内存里有就用内存（冷副本那条可能落后）；内存里没有才读冷副本；所有者不可用时不拿冷副本顶替。
func TestGetFallsBackToTheColdCopyOnlyWhenTheOwnerLacksTheRecord(t *testing.T) {
	u, _, coll := coldUnit(t)
	ctx := context.Background()
	at := u.clock.Now()
	u.round(at, u.startInput("live", nil, "a1", ""))
	live := IDOf("rally", "live")
	stale := u.record(live)
	stale.Status, stale.Version, stale.OperationKey, stale.CommandID, stale.Attempt = StatusFailed, 99, "", "", 0
	gone := coldRecord("gone", 4, at.Add(-3*time.Hour), StatusCompleted)
	seedCold(t, coll, stale, gone)

	got, err := u.e.Get(ctx, live)
	if err != nil || got.Status != StatusWaiting || got.Version == 99 {
		t.Fatalf("Get(live) = %v %+v, want the owner's in-memory record", err, got)
	}
	got, err = u.e.GetByBusinessKey(ctx, "rally", "gone")
	if err != nil || got.ID != gone.ID || got.Version != 4 || got.Status != StatusCompleted {
		t.Fatalf("GetByBusinessKey(gone) = %v %+v, want the cold copy record", err, got)
	}
	if _, err := u.e.Get(ctx, IDOf("rally", "never")); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get(never) = %v, want ErrNotFound", err)
	}
	u.raw.answer = func(string, []byte) ([]byte, error) { return nil, errors.New("no responders") }
	if _, err := u.e.Get(ctx, gone.ID); !errors.Is(err, ErrPartitionUnavailable) {
		t.Fatalf("Get with the owner away = %v, want ErrPartitionUnavailable (the cold copy may be older than memory)", err)
	}
}

// List：冷副本只补内存里没有的 ID。内存持有的 ID（落后的版本、或同业务键重开之后的旧一生）即使匹配条件也不用冷副本那条。
func TestListAddsOnlyColdRecordsTheOwnersDoNotHold(t *testing.T) {
	u, _, coll := coldUnit(t)
	ctx := context.Background()
	at := u.clock.Now()
	u.round(at, u.startInput("reopened", nil, "a1", ""), u.startInput("recent", nil, "a2", ""))
	reopened, recent := IDOf("rally", "reopened"), IDOf("rally", "recent")
	at = u.completeAll(at, recent)
	oldLife := coldRecord("reopened", 7, at.Add(-5*time.Hour), StatusCompleted) // 内存里是新一生（Waiting）
	history := coldRecord("history", 5, at.Add(-4*time.Hour), StatusCompensated)
	recentCold := u.record(recent) // 冷副本里同一条（版本相同）
	seedCold(t, coll, oldLife, history, recentCold)

	all, err := u.e.List(ctx, Query{Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]Status{}
	for _, r := range all {
		if _, dup := ids[r.ID]; dup {
			t.Fatalf("List returned %s twice: %+v", r.ID, all)
		}
		ids[r.ID] = r.Status
	}
	if len(all) != 3 || ids[reopened] != StatusWaiting || ids[recent] != StatusCompleted || ids[history.ID] != StatusCompensated {
		t.Fatalf("List = %+v, want memory's reopened (waiting) and recent plus the cold-only history", ids)
	}
	if all[0].ID != history.ID {
		t.Fatalf("List order: first %s, want the oldest (history)", all[0].ID)
	}
	completed, err := u.e.List(ctx, Query{Statuses: []Status{StatusCompleted}, Limit: 10})
	if err != nil {
		t.Fatal(err)
	}
	if len(completed) != 1 || completed[0].ID != recent {
		t.Fatalf("List(completed) = %+v, want only recent (the old life of reopened is held in memory)", completed)
	}
}

// 冷副本的一页被内存持有的 ID 占满时再翻一页，结果仍是按（更新时间，ID）排序的精确前缀。
func TestListPagesThroughColdRecordsHeldInMemory(t *testing.T) {
	u, _, coll := coldUnit(t)
	ctx := context.Background()
	at := u.clock.Now()
	var inputs []fnats.LogMsg
	for i := range 3 {
		inputs = append(inputs, u.startInput(fmt.Sprintf("held%d", i), nil, fmt.Sprintf("a%d", i), ""))
	}
	u.round(at, inputs...)
	for i := range 3 {
		seedCold(t, coll, coldRecord(fmt.Sprintf("held%d", i), 9, at.Add(-6*time.Hour+time.Duration(i)*time.Minute), StatusCompleted))
	}
	for i := range 3 {
		seedCold(t, coll, coldRecord(fmt.Sprintf("old%d", i), 3, at.Add(-5*time.Hour+time.Duration(i)*time.Minute), StatusCompleted))
	}
	got, err := u.e.List(ctx, Query{Statuses: []Status{StatusCompleted}, Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != IDOf("rally", "old0") || got[1].ID != IDOf("rally", "old1") {
		t.Fatalf("List(limit 2) = %+v, want old0, old1 (the first cold page is all held in memory)", got)
	}
}

// 启动判定的补查只看内存：冷副本里旧一生的记录（启动摘要不同）不能被当成“这次启动撞上了别的意图”。
func TestStartVerdictPollIgnoresTheColdCopy(t *testing.T) {
	u, _, coll := coldUnit(t)
	u.e.opts.StartVerdictTimeout = 10 * time.Millisecond
	u.e.log = verdictlessLog{queryLog{u: u}}
	u.e.ready.Store(true)
	old := coldRecord("again", 6, u.clock.Now().Add(-3*time.Hour), StatusCompleted)
	old.StartDigest = "an-older-life"
	seedCold(t, coll, old)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_, err := u.e.StartSaga(ctx, StartRequest{Type: "rally", BusinessKey: "again", DefinitionVersion: 1})
	if !errors.Is(err, ErrStartUnconfirmed) {
		t.Fatalf("StartSaga with no verdict and only a cold record = %v, want ErrStartUnconfirmed (the cold copy takes no part in decisions)", err)
	}
}

// WaitTerminal 等的是“下一个终态”：所有者内存里没有这笔时，冷副本里的旧终态不算。
func TestWaitTerminalIgnoresTheColdCopy(t *testing.T) {
	u, _, coll := coldUnit(t)
	u.e.ready.Store(true)
	old := coldRecord("waited", 6, u.clock.Now().Add(-3*time.Hour), StatusCompleted)
	seedCold(t, coll, old)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if got, err := u.e.WaitTerminal(ctx, old.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("WaitTerminal = %+v %v, want to keep waiting past the cold copy's old terminal", got, err)
	}
}

// verdictlessLog 收下意图（PubAck 成功）却永远不回判定：发起方走补查路径。
type verdictlessLog struct{ queryLog }

func (verdictlessLog) PublishFenced(context.Context, []fnats.LogMsg, fnats.LogFence) (fnats.LogPublishAck, error) {
	return fnats.LogPublishAck{Seq: 1}, nil
}

// ---- 清理守卫 ----

// purgeLog 记录清理调用，DurableAckFloors 返回给定的 floor。
type purgeLog struct {
	stubLog
	floors map[string]uint64
	purged []uint64
}

func (l *purgeLog) DurableAckFloors(context.Context, string) (map[string]uint64, error) {
	out := make(map[string]uint64, len(l.floors))
	for k, v := range l.floors {
		out[k] = v
	}
	return out, nil
}

func (l *purgeLog) Purge(_ context.Context, _, _ string, below uint64) error {
	l.purged = append(l.purged, below)
	return nil
}

// 冷副本开启时它的 durable 是清理守卫：落后时清理不越过它的 ack floor；找不到它（R1 内存消费者随服务端重启消失、还没重建）
// 时不清理；跟得上时照常清理。
func TestColdCopyDurableGuardsThePartitionPurge(t *testing.T) {
	u, _, _ := coldUnit(t)
	view := snapshotView{lastEv: 100, inHi: 120}
	durable := ColdCopyDurable(u.p.stream)
	cases := []struct {
		name   string
		floors map[string]uint64
		want   []uint64
	}{
		{"cold copy keeps up", map[string]uint64{"step": 500, durable: 500}, []uint64{100}},
		{"cold copy lags", map[string]uint64{"step": 500, durable: 40}, []uint64{41}},
		{"cold copy durable missing", map[string]uint64{"step": 500}, nil},
	}
	for _, tc := range cases {
		log := &purgeLog{floors: tc.floors}
		u.e.log = log
		u.p.purge(context.Background(), view, u.clock.Now())
		if fmt.Sprint(log.purged) != fmt.Sprint(tc.want) {
			t.Errorf("%s: purged below %v, want %v", tc.name, log.purged, tc.want)
		}
	}
	// 冷副本关闭时它不是守卫：没有它照常清理。
	u.e.opts.ColdCopy = nil
	log := &purgeLog{floors: map[string]uint64{"step": 500}}
	u.e.log = log
	u.p.purge(context.Background(), view, u.clock.Now())
	if fmt.Sprint(log.purged) != "[100]" {
		t.Fatalf("cold copy off: purged below %v, want [100]", log.purged)
	}
}
