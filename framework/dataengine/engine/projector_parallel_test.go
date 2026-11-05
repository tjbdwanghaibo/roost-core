package engine

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// entityParallelStore 是声明可并行投影的替身：Project / ProjectBatch 先过钩子，再按调用内顺序登记应用次序。
// 钩子可以阻塞、观察别的调用或返回错误，用来构造“后面的记录先完成”“前面的失败”等受控交错。
type entityParallelStore struct {
	mu      sync.Mutex
	applied []coredata.TransactionID
	calls   []string
	hook    func(ctx context.Context, records []coredata.CommitRecord) error
}

func (*entityParallelStore) SupportsParallelProjection() bool { return true }

func (s *entityParallelStore) Project(ctx context.Context, record coredata.CommitRecord) error {
	return s.apply(ctx, "project", []coredata.CommitRecord{record})
}

func (s *entityParallelStore) ProjectBatch(ctx context.Context, records []coredata.CommitRecord) error {
	return s.apply(ctx, "batch", records)
}

func (s *entityParallelStore) apply(ctx context.Context, kind string, records []coredata.CommitRecord) error {
	if s.hook != nil {
		if err := s.hook(ctx, records); err != nil {
			return err
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, fmt.Sprintf("%s:%d", kind, len(records)))
	for i := range records {
		s.applied = append(s.applied, records[i].ID)
	}
	return nil
}

func (s *entityParallelStore) appliedIDs() []coredata.TransactionID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return slices.Clone(s.applied)
}

func (s *entityParallelStore) isApplied(id coredata.TransactionID) bool {
	return slices.Contains(s.appliedIDs(), id)
}

// entityRecord 是写实体 entities 的一条普通记录（每个实体一个 heroes 文档）；sequence 决定事务 ID。
func entityRecord(sequence byte, entities ...int64) coredata.CommitRecord {
	record := projectorRecord(sequence, false)
	template := record.Mutations[0]
	record.Mutations = record.Mutations[:0]
	for _, id := range entities {
		mutation := template
		mutation.Key.ID = id
		record.Mutations = append(record.Mutations, mutation)
	}
	return record
}

func recordTouches(record coredata.CommitRecord, entity int64) bool {
	for _, mutation := range record.Mutations {
		if mutation.Key.ID == entity {
			return true
		}
	}
	return false
}

// waitForLaterConflictingRecord 在一次调用投影 records 之前，最多等 window 时长，看 WAL 里排在它们后面、
// 与它们写同一实体（或都带 effect）的记录有没有先被应用。正确调度下这种记录不可能先于前驱开始，等待总是超时、
// 顺序不受影响；去掉冲突检查后它们落在别的通道里并发执行，会在这段等待里先完成，顺序断言因此变红。
func waitForLaterConflictingRecord(s *entityParallelStore, all, records []coredata.CommitRecord, conflict func(a, b coredata.CommitRecord) bool, window time.Duration) {
	position := func(id coredata.TransactionID) int {
		return slices.IndexFunc(all, func(r coredata.CommitRecord) bool { return r.ID == id })
	}
	deadline := time.Now().Add(window)
	for time.Now().Before(deadline) {
		for _, record := range records {
			for _, applied := range s.appliedIDs() {
				later := position(applied)
				if later > position(record.ID) && conflict(record, all[later]) {
					return
				}
			}
		}
		time.Sleep(2 * time.Millisecond)
	}
}

func sharesEntity(a, b coredata.CommitRecord) bool {
	for _, mutation := range a.Mutations {
		if recordTouches(b, mutation.Key.ID) {
			return true
		}
	}
	return false
}

// assertConflictOrder 断言：任意两条冲突记录的应用次序与 WAL 次序相同，且每条记录恰好应用一次。
func assertConflictOrder(t *testing.T, all []coredata.CommitRecord, applied []coredata.TransactionID, conflict func(a, b coredata.CommitRecord) bool) {
	t.Helper()
	if len(applied) != len(all) {
		t.Fatalf("applied %d records, want %d: %v", len(applied), len(all), applied)
	}
	at := make(map[coredata.TransactionID]int, len(applied))
	for i, id := range applied {
		if _, dup := at[id]; dup {
			t.Fatalf("transaction %s applied twice", id)
		}
		at[id] = i
	}
	for i := range all {
		for j := i + 1; j < len(all); j++ {
			if conflict(all[i], all[j]) && at[all[i].ID] > at[all[j].ID] {
				t.Fatalf("WAL record %d (%s) applied after later conflicting record %d (%s); applied order=%v", i, all[i].ID, j, all[j].ID, applied)
			}
		}
	}
}

// 互不相干的实体并行投影：第一条记录的 Mongo 调用卡住时，另一个实体的记录照样完成并唤醒它的等待方，
// 而 ReplayPass 等全部通道返回后才结束、只确认连续前缀。现行串行投影器在这里超时（两条进同一个批、一起卡住）。
func TestEntityParallelIndependentEntityCompletesWhileFirstIsBlocked(t *testing.T) {
	records := []coredata.CommitRecord{entityRecord(1, 101), entityRecord(2, 102)}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	store := &entityParallelStore{}
	store.hook = func(ctx context.Context, batch []coredata.CommitRecord) error {
		if slices.ContainsFunc(batch, func(r coredata.CommitRecord) bool { return r.ID == records[0].ID }) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	ticket := &projectionTicket{done: make(chan struct{})}
	p.tickets[records[1].ID] = ticket
	done := make(chan struct{})
	var n int
	var err error
	go func() { defer close(done); n, err = p.ReplayPass(context.Background()) }()
	awaitChan(t, entered, "first entity projection")
	awaitChan(t, ticket.Done(), "independent entity projected while the first is blocked")
	select {
	case <-done:
		t.Fatal("replay returned before every lane finished")
	default:
	}
	assertWALReplayCount(t, w, 2)
	once.Do(func() { close(release) })
	awaitChan(t, done, "parallel replay completion")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 0)
}

// 同一实体的记录必须沿 WAL 顺序投影，不同实体的记录可以并行。每次调用先等一会儿，看后面同实体的记录会不会
// 抢先完成：去掉冲突检查（同实体记录落进不同通道）时这里变红。
func TestEntityParallelSameEntityRecordsKeepWALOrder(t *testing.T) {
	records := []coredata.CommitRecord{
		entityRecord(1, 201), entityRecord(2, 202), entityRecord(3, 201), entityRecord(4, 203),
		entityRecord(5, 201), entityRecord(6, 202), entityRecord(7, 204), entityRecord(8, 203),
	}
	store := &entityParallelStore{}
	store.hook = func(_ context.Context, batch []coredata.CommitRecord) error {
		waitForLaterConflictingRecord(store, records, batch, sharesEntity, 150*time.Millisecond)
		return nil
	}
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	if n, err := p.ReplayPass(context.Background()); n != len(records) || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertConflictOrder(t, records, store.appliedIDs(), sharesEntity)
	assertWALReplayCount(t, w, 0)
}

// 多实体事务把它写到的全部实体串起来：与它共享任一实体的单实体记录都按 WAL 顺序排在它前后。
// 只按第一个 mutation 取键（漏掉 Multi 的其余实体）时，C、D 上的记录会越过多实体事务，这里变红。
func TestEntityParallelMultiEntityRecordOrdersOverlappingRecords(t *testing.T) {
	const a, b, c, d = 301, 302, 303, 304
	records := []coredata.CommitRecord{
		entityRecord(1, a), entityRecord(2, b), entityRecord(3, a, c), entityRecord(4, c),
		entityRecord(5, d), entityRecord(6, b), entityRecord(7, c, d), entityRecord(8, d), entityRecord(9, a),
	}
	store := &entityParallelStore{}
	store.hook = func(_ context.Context, batch []coredata.CommitRecord) error {
		waitForLaterConflictingRecord(store, records, batch, sharesEntity, 150*time.Millisecond)
		return nil
	}
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	if n, err := p.ReplayPass(context.Background()); n != len(records) || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertConflictOrder(t, records, store.appliedIDs(), sharesEntity)
	assertWALReplayCount(t, w, 0)
}

func hasEffect(a, b coredata.CommitRecord) bool {
	return len(a.Effects) > 0 && len(b.Effects) > 0
}

// 打开全局 effect 顺序（effect_order=wal，2026-11-05 前的唯一行为）时：带 effect 的记录之间（即使写的是不同实体）仍按
// WAL 顺序投影，不带 effect 的记录与它们并行。去掉 effect 顺序类（变异 M5）时这里变红。
func TestEntityParallelEffectRecordsKeepWALOrderAcrossEntities(t *testing.T) {
	records := []coredata.CommitRecord{entityRecord(1, 401), entityRecord(2, 402), entityRecord(3, 403), entityRecord(4, 404), entityRecord(5, 405)}
	for _, i := range []int{0, 2, 3} {
		records[i].Effects = []coredata.Effect{{ID: fmt.Sprintf("effect-%d", i), Topic: "hero.changed", Payload: []byte{byte(i)}}}
	}
	store := &entityParallelStore{}
	store.hook = func(_ context.Context, batch []coredata.CommitRecord) error {
		waitForLaterConflictingRecord(store, records, batch, hasEffect, 150*time.Millisecond)
		return nil
	}
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	p.opts.EffectOrder = EffectOrderWAL
	if n, err := p.ReplayPass(context.Background()); n != len(records) || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertConflictOrder(t, records, store.appliedIDs(), hasEffect)
	assertWALReplayCount(t, w, 0)
}

// 缺省 effect_order=entity（维护者 2026-11-05）：带 effect、写不相干实体的两条记录分进不同通道，第一条的 Mongo 调用卡住时
// 第二条照样完成并唤醒它的等待方。全局 effect 顺序下两条共享 effect 顺序类、排在同一通道里，第二条等第一条，这里超时变红。
func TestEntityParallelEntityEffectOrderProjectsUnrelatedEffectRecordsConcurrently(t *testing.T) {
	records := []coredata.CommitRecord{entityRecord(1, 801), entityRecord(2, 802)}
	for i := range records {
		records[i].Effects = []coredata.Effect{{ID: fmt.Sprintf("effect-80%d", i+1), Topic: "alliance.member_changed", Payload: []byte{byte(i)}}}
	}
	entered, release := make(chan struct{}), make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	store := &entityParallelStore{}
	store.hook = func(ctx context.Context, batch []coredata.CommitRecord) error {
		if slices.ContainsFunc(batch, func(r coredata.CommitRecord) bool { return r.ID == records[0].ID }) {
			close(entered)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	ticket := &projectionTicket{done: make(chan struct{})}
	p.tickets[records[1].ID] = ticket
	done := make(chan struct{})
	var n int
	var err error
	go func() { defer close(done); n, err = p.ReplayPass(context.Background()) }()
	awaitChan(t, entered, "first effect record projection")
	awaitChan(t, ticket.Done(), "unrelated entity's effect record projected while the first is blocked")
	assertWALReplayCount(t, w, 2)
	once.Do(func() { close(release) })
	awaitChan(t, done, "parallel replay completion")
	if err != nil || n != 2 {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 0)
}

// 缺省 effect_order=entity 只放宽不相干实体之间的顺序：同一实体上的带 effect 记录仍按 WAL 顺序投影（同实体的 effect 暂存顺序不变）。
// 每次调用先等一会儿，看后面同实体的记录会不会抢先完成；放宽时连实体键一起丢掉（只留事务键）时这里变红。
func TestEntityParallelEntityEffectOrderKeepsSameEntityEffectsInWALOrder(t *testing.T) {
	records := []coredata.CommitRecord{
		entityRecord(1, 901), entityRecord(2, 902), entityRecord(3, 901), entityRecord(4, 903),
		entityRecord(5, 901), entityRecord(6, 902), entityRecord(7, 903),
	}
	for i := range records {
		records[i].Effects = []coredata.Effect{{ID: fmt.Sprintf("effect-9%02d", i), Topic: "alliance.member_changed", Payload: []byte{byte(i)}}}
	}
	store := &entityParallelStore{}
	store.hook = func(_ context.Context, batch []coredata.CommitRecord) error {
		waitForLaterConflictingRecord(store, records, batch, sharesEntity, 150*time.Millisecond)
		return nil
	}
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	if n, err := p.ReplayPass(context.Background()); n != len(records) || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	assertConflictOrder(t, records, store.appliedIDs(), sharesEntity)
	if p.Stats().ParallelWindows == 0 {
		t.Fatal("effect records on three entities were projected serially")
	}
	assertWALReplayCount(t, w, 0)
}

// failingOnceStore 包住真实 MongoStore（mongotest）：第一轮里含 failID 的调用先等 waitID 落库（证明后面的记录先投影完），
// 再返回一个可重试错误；之后照常委托。
type failingOnceStore struct {
	*MongoStore
	failID, waitID coredata.TransactionID
	armed          atomic.Bool
	waitApplied    chan struct{}
	waitOnce       sync.Once
	sawWait        atomic.Bool
}

var errInjectedProjection = errors.New("injected transient projection failure")

func (s *failingOnceStore) intercept(ctx context.Context, records []coredata.CommitRecord) error {
	if !s.armed.Load() || !slices.ContainsFunc(records, func(r coredata.CommitRecord) bool { return r.ID == s.failID }) {
		return nil
	}
	select {
	case <-s.waitApplied:
		s.sawWait.Store(true)
	case <-time.After(3 * time.Second):
	case <-ctx.Done():
		return ctx.Err()
	}
	return errInjectedProjection
}

func (s *failingOnceStore) observe(records []coredata.CommitRecord, err error) {
	if err == nil && slices.ContainsFunc(records, func(r coredata.CommitRecord) bool { return r.ID == s.waitID }) {
		s.waitOnce.Do(func() { close(s.waitApplied) })
	}
}

func (s *failingOnceStore) ProjectBatch(ctx context.Context, records []coredata.CommitRecord) error {
	if err := s.intercept(ctx, records); err != nil {
		return err
	}
	err := s.MongoStore.ProjectBatch(ctx, records)
	s.observe(records, err)
	return err
}

func (s *failingOnceStore) Project(ctx context.Context, record coredata.CommitRecord) error {
	if err := s.intercept(ctx, []coredata.CommitRecord{record}); err != nil {
		return err
	}
	err := s.MongoStore.Project(ctx, record)
	s.observe([]coredata.CommitRecord{record}, err)
	return err
}

// 后面的记录先投影完、前面的记录失败：ack 不越过失败点（WAL 一条都没确认）；之后用同一个 WAL 目录重启
// 一个新的投影器重放全部记录，已成功的后缀靠持久事务标记幂等，不误报冲突，最终版本与 outbox 恰好一次。
// 实体 8 上三条记录：两条普通 Patch 之间夹一条带 effect 的记录，前一条普通记录独占一个批；
// 并行通道里它若走单 mutation 快路（不写标记），重放时文档已是更高版本 → ErrProjectionConflict（变异核对 M4）。
// 去掉前缀确认（整窗 ack）时第一轮之后 WAL 已空、失败的记录丢失（变异核对 M3）。
func TestEntityParallelFailedPrefixStopsAckAndRestartReplaysSuffixIdempotently(t *testing.T) {
	mongoStore, client, heroes := newMongoStoreTest(t)
	for _, seed := range []bson.M{{"_id": int64(7), "_version": int64(1)}, {"_id": int64(8), "_version": int64(1)}} {
		if err := heroes.Seed(seed); err != nil {
			t.Fatal(err)
		}
	}
	failed := batchRecord(t, 1, 7, 1, 2)
	first := batchRecord(t, 2, 8, 1, 2)
	special := batchRecord(t, 3, 8, 2, 3)
	special.Effects = []coredata.Effect{{ID: "effect-entity-8", Topic: "hero.changed", Payload: []byte{8}}}
	last := batchRecord(t, 4, 8, 3, 4)
	records := []coredata.CommitRecord{failed, first, special, last}
	store := &failingOnceStore{MongoStore: mongoStore, failID: failed.ID, waitID: last.ID, waitApplied: make(chan struct{})}
	store.armed.Store(true)

	dir := t.TempDir()
	wal, err := nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	for i := range records {
		if _, err := wal.Append(context.Background(), records[i]); err != nil {
			t.Fatal(err)
		}
	}
	projector, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	n, err := projector.ReplayPass(context.Background())
	if n != 0 || !errors.Is(err, errInjectedProjection) || !strings.Contains(err.Error(), failed.ID.String()) {
		t.Fatalf("first pass n=%d err=%v", n, err)
	}
	if !store.sawWait.Load() {
		t.Fatal("the failing record did not observe the later records projected first (projection is not parallel)")
	}
	if fatal := projector.fatal(); fatal != nil {
		t.Fatalf("transient failure fenced the projector: %v", fatal)
	}
	if err := projector.Close(context.Background()); err != nil {
		t.Fatal(err)
	}

	// 重启：新进程从同一 WAL 目录读 checkpoint，按前缀之后的全部记录重放。
	wal, err = nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	assertWALReplayIDs(t, wal, []coredata.TransactionID{failed.ID, first.ID, special.ID, last.ID})
	store.armed.Store(false)
	restarted, err := NewProjector(wal, store, ProjectorOptions{CloseWAL: true, ManualReplay: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = restarted.Close(context.Background()) })
	if err := restarted.Flush(context.Background()); err != nil {
		t.Fatalf("replay after restart: %v", err)
	}
	if fatal := restarted.fatal(); fatal != nil {
		t.Fatalf("replay of the successful suffix reported a conflict: %v", fatal)
	}
	assertWALReplayCount(t, wal, 0)
	seven, _ := heroes.Lookup(int64(7))
	eight, _ := heroes.Lookup(int64(8))
	if seven["_version"] != int64(2) || eight["_version"] != int64(4) || eight["_last_tx"] != last.ID.String() {
		t.Fatalf("documents after replay: 7=%v 8=%v", seven, eight)
	}
	if outbox := client.Collection(testDatabase, OutboxCollection).Len(); outbox != 1 {
		t.Fatalf("outbox entries=%d, want exactly one", outbox)
	}
}

// 投影冲突在并行通道里仍 fail-stop：冲突的通道停下，别的通道不再开始新单元，fatal / OnFatal 只一次，
// 确认停在连续成功前缀，冲突之后的准入被拒绝。逐通道判断 fatal，不依赖错误合并后的形状。
func TestEntityParallelFatalConflictFailStopsEveryLane(t *testing.T) {
	const a, b = 501, 502
	ok := entityRecord(1, a)
	conflict := entityRecord(2, b)
	afterOK := entityRecord(3, a)
	afterOK.Effects = []coredata.Effect{{ID: "effect-after", Topic: "hero.changed", Payload: []byte{3}}}
	afterConflict := entityRecord(4, b)
	records := []coredata.CommitRecord{ok, conflict, afterOK, afterConflict}
	conflictFailed := make(chan struct{})
	var stopOnce sync.Once
	var sawConflict atomic.Bool
	store := &entityParallelStore{}
	store.hook = func(ctx context.Context, batch []coredata.CommitRecord) error {
		switch {
		case slices.ContainsFunc(batch, func(r coredata.CommitRecord) bool { return r.ID == conflict.ID }):
			return fmt.Errorf("stored version is newer: %w", ErrProjectionConflict)
		case slices.ContainsFunc(batch, func(r coredata.CommitRecord) bool { return r.ID == ok.ID }):
			select {
			case <-conflictFailed:
				sawConflict.Store(true)
			case <-time.After(3 * time.Second):
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}
	fenced := make(chan error, 2)
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	p.opts.OnFatal = func(err error) { fenced <- err }
	// 第一条通道在投影器观察到冲突（停止补位已置位）之后才返回，它的下一个单元必须不再发出。
	p.laneFailed = func() { stopOnce.Do(func() { close(conflictFailed) }) }
	n, err := p.ReplayPass(context.Background())
	if !errors.Is(err, ErrProjectionConflict) || !strings.Contains(err.Error(), conflict.ID.String()) {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !sawConflict.Load() {
		t.Fatal("the first lane did not run concurrently with the conflicting lane")
	}
	if n != 1 {
		t.Fatalf("processed=%d, want the successful prefix of 1", n)
	}
	if store.isApplied(afterOK.ID) || store.isApplied(afterConflict.ID) {
		t.Fatalf("a lane started a new unit after the conflict: %v", store.appliedIDs())
	}
	if !errors.Is(awaitChan(t, fenced, "OnFatal"), ErrProjectionConflict) {
		t.Fatal("OnFatal got a different error")
	}
	if stats := p.Stats(); stats.FatalProjectionConflicts != 1 {
		t.Fatalf("stats=%+v", stats)
	}
	assertWALReplayIDs(t, w, []coredata.TransactionID{conflict.ID, afterOK.ID, afterConflict.ID})
	if _, err := p.Enqueue(context.Background(), entityRecord(9, 509)); !errors.Is(err, ErrProjectionConflict) {
		t.Fatalf("admission after fatal err=%v", err)
	}
	select {
	case extra := <-fenced:
		t.Fatalf("OnFatal called twice: %v", extra)
	case <-time.After(50 * time.Millisecond):
	}
}

// lanesOf 把规划结果写成“每条通道的事务序号”，便于逐条对照（序号取事务 ID 末字节）。
func lanesOf(records []coredata.CommitRecord, window replayWindow) [][]byte {
	lanes := make([][]byte, 0, len(window.lanes))
	for _, lane := range window.lanes {
		var ids []byte
		for _, index := range lane {
			ids = append(ids, records[window.start+index].ID[15])
		}
		lanes = append(lanes, ids)
	}
	return lanes
}

// 规划器：冲突键把记录连成分量，整分量进同一通道并保持 WAL 顺序；屏障记录切开窗口；workers=1 或整段互相冲突时串行。
func TestPlanReplayWindowsGroupsRecordsByConflictKeys(t *testing.T) {
	receipt := func(record coredata.CommitRecord, id string) coredata.CommitRecord {
		record.Receipts = append(record.Receipts, coredata.Receipt{Namespace: "saga", ID: id})
		return record
	}
	effect := func(record coredata.CommitRecord, id string) coredata.CommitRecord {
		record.Effects = append(record.Effects, coredata.Effect{ID: id, Topic: "t"})
		return record
	}
	barrier := entityRecord(3, 0)
	type window struct {
		start, end int
		lanes      [][]byte
	}
	for _, tc := range []struct {
		name              string
		records           []coredata.CommitRecord
		workers           int
		remoteParallel    bool
		effectsInWALOrder bool
		want              []window
	}{
		{name: "independent", records: []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 12), entityRecord(3, 13)}, workers: 8,
			want: []window{{0, 3, [][]byte{{1}, {2}, {3}}}}},
		{name: "same entity chained", records: []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 12), entityRecord(3, 11)}, workers: 8,
			want: []window{{0, 3, [][]byte{{1, 3}, {2}}}}},
		{name: "multi entity joins components", records: []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 12), entityRecord(3, 12, 13), entityRecord(4, 13), entityRecord(5, 14)}, workers: 8,
			want: []window{{0, 5, [][]byte{{1}, {2, 3, 4}, {5}}}}},
		{name: "same receipt identity", records: []coredata.CommitRecord{receipt(entityRecord(1, 11), "op"), entityRecord(2, 12), receipt(entityRecord(3, 13), "op")}, workers: 8,
			want: []window{{0, 3, [][]byte{{1, 3}, {2}}}}},
		{name: "distinct receipts", records: []coredata.CommitRecord{receipt(entityRecord(1, 11), "a"), receipt(entityRecord(2, 12), "b")}, workers: 8,
			want: []window{{0, 2, [][]byte{{1}, {2}}}}},
		{name: "effects in WAL order when enabled", records: []coredata.CommitRecord{effect(entityRecord(1, 11), "e1"), entityRecord(2, 12), effect(entityRecord(3, 13), "e3")}, workers: 8, effectsInWALOrder: true,
			want: []window{{0, 3, [][]byte{{1, 3}, {2}}}}},
		{name: "effects by entity are per entity", records: []coredata.CommitRecord{effect(entityRecord(1, 11), "e1"), entityRecord(2, 12), effect(entityRecord(3, 13), "e3")}, workers: 8,
			want: []window{{0, 3, [][]byte{{1}, {2}, {3}}}}},
		{name: "effects by entity keep same entity order", records: []coredata.CommitRecord{effect(entityRecord(1, 11), "e1"), effect(entityRecord(2, 12), "e2"), effect(entityRecord(3, 11), "e3")}, workers: 8,
			want: []window{{0, 3, [][]byte{{1, 3}, {2}}}}},
		{name: "effects by entity keep same effect id order", records: []coredata.CommitRecord{effect(entityRecord(1, 11), "e1"), effect(entityRecord(2, 12), "e1")}, workers: 8,
			want: []window{{0, 2, nil}}},
		{name: "duplicate transaction id", records: func() []coredata.CommitRecord {
			r := []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 12), entityRecord(1, 13)}
			return r
		}(), workers: 8, want: []window{{0, 3, [][]byte{{1, 1}, {2}}}}},
		{name: "workers bound lanes", records: []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 12), entityRecord(3, 13), entityRecord(4, 14), entityRecord(5, 11)}, workers: 2,
			want: []window{{0, 5, [][]byte{{1, 4, 5}, {2, 3}}}}},
		{name: "all conflicting is serial", records: []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 11, 12), entityRecord(3, 12)}, workers: 8,
			want: []window{{0, 3, nil}}},
		{name: "one worker is serial", records: []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 12)}, workers: 1,
			want: []window{{0, 2, nil}}},
		{name: "barrier splits windows", records: []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 12), barrier, entityRecord(4, 13), entityRecord(5, 14)}, workers: 8,
			want: []window{{0, 2, [][]byte{{1}, {2}}}, {2, 3, nil}, {3, 5, [][]byte{{4}, {5}}}}},
		{name: "lone records around barrier merge into one serial run", records: []coredata.CommitRecord{entityRecord(1, 11), barrier, entityRecord(4, 13)}, workers: 8,
			want: []window{{0, 3, nil}}},
		{name: "remote without capability is one class", records: []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2), entityRecord(3, 13)}, workers: 8,
			want: []window{{0, 3, [][]byte{{1, 2}, {3}}}}},
		{name: "remote with capability is per entity", records: []coredata.CommitRecord{remoteProjectionRecord(t, 1), remoteProjectionRecord(t, 2), entityRecord(3, 13)}, workers: 8, remoteParallel: true,
			want: []window{{0, 3, [][]byte{{1}, {2}, {3}}}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var got []window
			for _, w := range planReplayWindows(tc.records, tc.workers, conflictScope{remoteParallel: tc.remoteParallel, effectsInWALOrder: tc.effectsInWALOrder}) {
				var lanes [][]byte
				if w.lanes != nil {
					lanes = lanesOf(tc.records, w)
				}
				got = append(got, window{w.start, w.end, lanes})
			}
			if fmt.Sprint(got) != fmt.Sprint(tc.want) {
				t.Fatalf("windows=%v want=%v", got, tc.want)
			}
		})
	}
}

// 并行窗口里只有“本窗口里同键最后一条”的单 mutation 记录可以走快路；多 mutation 记录本来就不走快路。
func TestPlanReplayWindowsAllowsFastPathOnlyForTheLastRecordOfItsKeys(t *testing.T) {
	records := []coredata.CommitRecord{entityRecord(1, 11), entityRecord(2, 12), entityRecord(3, 11), entityRecord(4, 13, 14), entityRecord(5, 12)}
	windows := planReplayWindows(records, 8, conflictScope{})
	if len(windows) != 1 || windows[0].lanes == nil {
		t.Fatalf("windows=%+v", windows)
	}
	if want := []bool{false, false, true, false, true}; !slices.Equal(windows[0].fastPath, want) {
		t.Fatalf("fast path=%v want=%v", windows[0].fastPath, want)
	}
}

// 批量写退回逐条、而逐条路径让一条“窗口里后面还有同键记录”的快路记录成功时，该通道就此停下：
// 本轮只确认到它，后面的同键记录留给下一轮（下一轮从确认前缀重读）；别的通道照常。
func TestEntityParallelPerRecordFastPathStopsItsLaneUntilNextPass(t *testing.T) {
	first := entityRecord(1, 601)
	sameEntity := entityRecord(2, 601)
	sameEntity.Effects = []coredata.Effect{{ID: "effect-601", Topic: "hero.changed", Payload: []byte{1}}}
	other := entityRecord(3, 602)
	records := []coredata.CommitRecord{first, sameEntity, other}
	store := &entityParallelStore{}
	var deferred atomic.Bool
	store.hook = func(_ context.Context, batch []coredata.CommitRecord) error {
		// 只有批量调用（ProjectBatch）里出现 first 时退回逐条；逐条路径（Project）照常。
		if len(batch) == 1 && batch[0].ID == first.ID && deferred.CompareAndSwap(false, true) {
			return fmt.Errorf("%w: replayed put", ErrProjectionBatchNeedsPerRecord)
		}
		return nil
	}
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	n, err := p.ReplayPass(context.Background())
	if n != 1 || err != nil {
		t.Fatalf("first pass n=%d err=%v", n, err)
	}
	if store.isApplied(sameEntity.ID) || !store.isApplied(other.ID) {
		t.Fatalf("applied=%v", store.appliedIDs())
	}
	assertWALReplayIDs(t, w, []coredata.TransactionID{sameEntity.ID, other.ID})
	if n, err := p.ReplayPass(context.Background()); n != 2 || err != nil {
		t.Fatalf("second pass n=%d err=%v", n, err)
	}
	assertWALReplayCount(t, w, 0)
}

// workers=1 关闭并行：独立实体的普通记录与改动前一样合成一个批、串行投影。
func TestEntityParallelOneWorkerKeepsSerialProjection(t *testing.T) {
	records := []coredata.CommitRecord{entityRecord(1, 701), entityRecord(2, 702), entityRecord(3, 703)}
	store := &entityParallelStore{}
	p, w := stoppedProjectorWithRecords(t, store, records, 4<<20)
	p.opts.ProjectionWorkers = 1
	if n, err := p.ReplayPass(context.Background()); n != 3 || err != nil {
		t.Fatalf("n=%d err=%v", n, err)
	}
	if !slices.Equal(store.calls, []string{"batch:3"}) || p.Stats().ParallelWindows != 0 {
		t.Fatalf("calls=%v stats=%+v", store.calls, p.Stats())
	}
	assertWALReplayCount(t, w, 0)
}

func TestNewProjectorRejectsOutOfRangeProjectionWorkers(t *testing.T) {
	for _, workers := range []int{-1, 65} {
		wal, err := nestwal.Open(nestwal.DefaultOptions(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewProjector(wal, &entityParallelStore{}, ProjectorOptions{ProjectionWorkers: workers, ManualReplay: true}); err == nil {
			t.Fatalf("workers=%d accepted", workers)
		}
		_ = wal.Close(context.Background())
	}
}

// effect 顺序范围只认 entity / wal；空取 entity，拼错的取值在构造时拒绝，不静默退回缺省。
func TestNewProjectorEffectOrder(t *testing.T) {
	for _, tc := range []struct {
		order EffectOrder
		want  EffectOrder
		ok    bool
	}{{"", EffectOrderEntity, true}, {EffectOrderWAL, EffectOrderWAL, true}, {EffectOrderEntity, EffectOrderEntity, true}, {"per_entity", "", false}} {
		wal, err := nestwal.Open(nestwal.DefaultOptions(t.TempDir()))
		if err != nil {
			t.Fatal(err)
		}
		p, err := NewProjector(wal, &entityParallelStore{}, ProjectorOptions{EffectOrder: tc.order, ManualReplay: true})
		if (err == nil) != tc.ok {
			t.Fatalf("effect order %q: err=%v", tc.order, err)
		}
		if p != nil {
			if p.opts.EffectOrder != tc.want {
				t.Fatalf("effect order %q became %q, want %q", tc.order, p.opts.EffectOrder, tc.want)
			}
			_ = p.Close(context.Background())
		}
		_ = wal.Close(context.Background())
	}
}

func TestMongoStoreDeclaresParallelProjection(t *testing.T) {
	store, _, _ := newMongoStoreTest(t)
	if !store.SupportsParallelProjection() {
		t.Fatal("MongoStore must declare parallel projection")
	}
	var _ ParallelProjectionStore = store
}
