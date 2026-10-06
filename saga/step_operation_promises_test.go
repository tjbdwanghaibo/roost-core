package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	deengine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/metrics"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	corenest "github.com/tjbdwanghaibo/roost-core/nest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// U-0280：原生步骤的“最多生效一次”以操作实例（saga + 步骤 + 方向）为单位，不以尝试（CommandID）为单位。
//
// 这里把演练 drill6 的三种交错做成确定性用例：协调器用内存 Store、手动推进时钟（processClaimed），
// 原生收件箱是 mongotest 上真实的 DataEngineStepInbox，“进程崩溃后 WAL 重放投影”用真实的
// dataengine MongoStore.Project 投影同一条 CommitRecord（含 lease fence、回执、completion effect
// 和一笔业务写），所以 fence 的接受 / 跳过就是生产的判定。业务写是往 debits 集合插入一份以尝试号
// 区分的文档：debits 里的文档数就是这一步真实生效的次数。
//
//   - (a) 尝试 k 已提交、协调器已发出 k+1；k 的结果在 k+1 等待期间到达并被接收，随后 k+1 投递：
//     k+1 不能再执行，只回放 k 的结果。
//   - (b) k 的结果在重试退避期间到达，被协调器以 ErrNotWaiting 丢弃；k+1 投递时必须回放 k 的结果，
//     协调器照常接收并前进，而不是让 k+1 再扣一次。
//   - (c) 协调器放弃（重试用尽）之后，k 才由 WAL 重放投影：命令截止已过，租约封顶在截止时间，
//     记录必须被跳过、不留回执、不改业务；而“截止前已投影、结果在放弃后才送达”的残余窗口
//     只告警（计数 + ERROR），不重开终态。
//   - (d) 投影积压：k 已准入 WAL 但未投影，k+1 接替它（supersede），k 之后的投影被跳过，k+1 执行一次。
func TestNativeStepTakesEffectAtMostOncePerOperation(t *testing.T) {
	t.Run("a: late result accepted while k+1 waits, k+1 replays it", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k) // 进程 A 把尝试 k 写进 WAL 后被 kill -9
		w.tick(5 * time.Second)                 // k 超时 → 退避
		w.tick(10 * time.Second)                // 发出 k+1
		k1 := w.pendingCommand()
		if !w.project(recordK) { // 进程 B 启动重放 WAL：k 的租约仍在截止前 → 投影生效
			t.Fatal("attempt k committed within its deadline was skipped on replay")
		}
		w.deliverEffect(recordK) // k 的 completion 在协调器等 k+1 时到达 → 接收，saga 前进
		if got := w.record().Step; got != 1 {
			t.Fatalf("late result of attempt %d was not accepted while attempt %d waited: step=%d", k.Attempt, k1.Attempt, got)
		}
		w.deliverCommand(w.inboxB, k1)
		w.assertEffective(k.IdempotencyKey, 1)
	})

	t.Run("b: late result dropped during backoff, k+1 replays it", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		w.tick(5 * time.Second) // k 超时 → 退避（pending）
		if !w.project(recordK) {
			t.Fatal("attempt k committed within its deadline was skipped on replay")
		}
		if err := w.complete(w.effectCompletion(recordK)); !errors.Is(err, ErrNotWaiting) {
			t.Fatalf("completion of attempt %d during backoff = %v, want ErrNotWaiting (the Nest completion consumer drops it as permanent)", k.Attempt, err)
		}
		w.tick(10 * time.Second)
		k1 := w.pendingCommand()
		w.deliverCommand(w.inboxB, k1)
		w.deliverReplays()
		if record := w.record(); record.Step != 1 || record.CompletedSteps != 1 {
			t.Fatalf("attempt %d did not carry the dropped result of attempt %d to the coordinator: %+v", k1.Attempt, k.Attempt, record)
		}
		w.assertEffective(k.IdempotencyKey, 1)
	})

	t.Run("c: replay after the deadline is skipped, nothing takes effect", func(t *testing.T) {
		// 进程 A 在“现在 - 60s”准入尝试 k（截止 = 准入 + 5s），崩溃；协调器 5s 后放弃；进程 B 现在才重放
		// WAL。claim 的 LeaseDuration 是 2 分钟，若不封顶到截止时间，这次重放仍在租约内、会生效。
		w := newNativeWorld(t, 1, time.Now().UTC().Add(-time.Minute))
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		w.tick(5 * time.Second) // 唯一一次尝试超时 → 没有已完成步骤 → Failed
		if record := w.record(); record.Status != StatusFailed {
			t.Fatalf("coordinator did not give up: %+v", record)
		}
		if w.project(recordK) {
			t.Errorf("attempt %s was replayed %s after its deadline and still took effect: the claim lease outlived the command deadline, so the saga stays failed with an uncompensated debit",
				k.ID, time.Since(k.DeadlineAt).Round(time.Second))
			w.deliverEffect(recordK)
		}
		w.assertEffective(k.IdempotencyKey, 0)
	})

	// saga 方向 ④（2026-10-07）：之前是“只告警、不重开”，现在重开去补偿这一步；告警计数不变。
	t.Run("c': success projected before the deadline but delivered after abandonment is alarmed and compensated", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		if !w.project(recordK) {
			t.Fatal("attempt projected within its deadline was skipped")
		}
		w.tick(5 * time.Second) // completion 还在路上，协调器按超时放弃 → Failed
		before := counterValue("saga.completion.late_after_abandon_total")
		duplicatesBefore := w.engine.Stats().Duplicates
		w.deliverEffect(recordK)
		record := w.record()
		if record.Status != StatusCompensating || record.Phase != PhaseCompensate || record.Step != 0 || record.CompletedSteps != 0 {
			t.Fatalf("a late success did not bring the abandoned saga back to compensate only that step: %+v", record)
		}
		if grown := counterValue("saga.completion.late_after_abandon_total") - before; grown != 1 {
			t.Errorf("success of %s arrived after the coordinator abandoned the step; saga.completion.late_after_abandon_total grew by %d, want 1 (duplicates grew by %d): an effective but uncompensated step must be visible",
				k.ID, grown, w.engine.Stats().Duplicates-duplicatesBefore)
		}
		w.assertEffective(k.IdempotencyKey, 1)
	})

	t.Run("d: projection backlog, k+1 supersedes the unprojected k", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k) // 已准入 WAL，投影积压
		w.tick(5 * time.Second)
		w.tick(10 * time.Second)
		k1 := w.pendingCommand()
		w.deliverCommand(w.inboxA, k1) // 同一进程：k+1 接替 k 后执行
		if w.project(recordK) {
			t.Errorf("attempt %s was projected after attempt %s superseded it: the step took effect twice", k.ID, k1.ID)
		}
		w.deliverEffects()
		if record := w.record(); record.Step != 1 {
			t.Fatalf("saga did not advance on attempt %s: %+v", k1.ID, record)
		}
		w.assertEffective(k.IdempotencyKey, 1)
	})
}

// S5 与 U-0280 重叠的交错：deadline / compensation / Resume 与晚到回执、同一操作多个结果或旧尝试、
// receipt / tombstone TTL 之后的重投。
func TestNativeStepOperationInterleavingsWithCoordinatorDecisions(t *testing.T) {
	// saga 方向 ④（2026-10-07）：之前迟到成功只告警、saga 仍 Failed，运维 Resume 让新一生回放它、继续向前。现在协调器
	// 自己把 saga 带回补偿、只补偿这一步，不需要 Resume（Compensating 也不能 Resume）；这一步仍然只执行过一次。
	t.Run("a late effective step on a failed saga is compensated, not run again", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		if !w.project(recordK) {
			t.Fatal("attempt projected within its deadline was skipped")
		}
		w.tick(5 * time.Second)
		w.deliverEffect(recordK) // 放弃之后才到：补偿这一步
		w.tick(6 * time.Second)
		compensation := w.pendingCommand()
		if compensation.Phase != PhaseCompensate || compensation.Step != 0 {
			t.Fatalf("the late effective step was not compensated: %+v", compensation)
		}
		w.completeCompensation(compensation, "")
		if record := w.record(); record.Status != StatusCompensated {
			t.Fatalf("want Compensated after compensating the late step: %+v", record)
		}
		w.assertEffective(k.IdempotencyKey, 1)
		if got := w.executions(k.IdempotencyKey); got != 1 {
			t.Fatalf("debit handler ran %d times, want 1", got)
		}
	})

	// saga 方向 ④（2026-10-07）：之前是“告警、不重开”，现在告警并重开去补偿这一步。
	t.Run("saga deadline abandons a step whose success arrives later: alarm and compensate it", func(t *testing.T) {
		w := newNativeWorldWithDeadline(t, 15, time.Now().UTC(), 7*time.Second)
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		if !w.project(recordK) {
			t.Fatal("attempt projected within its deadline was skipped")
		}
		w.tick(5 * time.Second) // 超时 → 退避
		w.tick(8 * time.Second) // saga 截止 → 没有已完成步骤 → Failed，操作被放弃
		before := counterValue("saga.completion.late_after_abandon_total")
		w.deliverEffect(recordK)
		if record := w.record(); record.Status != StatusCompensating || record.Step != 0 || record.LateStep != 1 {
			t.Fatalf("late success did not bring a saga closed by its deadline back to compensate the step: %+v", record)
		}
		if grown := counterValue("saga.completion.late_after_abandon_total") - before; grown != 1 {
			t.Fatalf("late success after the saga deadline grew the alarm by %d, want 1", grown)
		}
	})

	t.Run("manual Compensate during a step's backoff abandons it: a late success is alarmed", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		w.deliverCommand(w.inboxA, w.pendingCommand()) // debit 正常完成
		w.deliverEffects()
		w.tick(time.Second)
		deliver := w.pendingCommand() // deliver 的第一次尝试超时，进入退避
		w.tick(7 * time.Second)
		if record := w.record(); record.Status != StatusPending || record.Step != 1 || record.Attempt != 1 {
			t.Fatalf("deliver is not in backoff: %+v", record)
		}
		if _, err := w.engine.Compensate(w.ctx, w.sagaID, "operator", w.at(7*time.Second)); err != nil {
			t.Fatal(err)
		}
		before := counterValue("saga.completion.late_after_abandon_total")
		if err := w.complete(Completion{CommandID: deliver.ID, IdempotencyKey: deliver.IdempotencyKey, SagaID: deliver.SagaID, Success: true}); err != nil {
			t.Fatalf("late success after manual compensation = %v, want acknowledged", err)
		}
		if grown := counterValue("saga.completion.late_after_abandon_total") - before; grown != 1 {
			t.Fatalf("late success of a step abandoned by Compensate grew the alarm by %d, want 1", grown)
		}
		if record := w.record(); record.Status != StatusCompensating || record.CompletedSteps != 1 {
			t.Fatalf("late success changed the compensating saga: %+v", record)
		}
	})

	t.Run("a retryable failure does not stop a later attempt from running", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.outcome = func(command Command) Completion {
			if command.Attempt == 1 {
				return Completion{Success: false, Retryable: true, Error: "busy"}
			}
			return Completion{Success: true}
		}
		w.tick(0)
		k := w.pendingCommand()
		w.deliverCommand(w.inboxA, k)
		w.deliverEffects()
		w.tick(time.Second)
		k1 := w.pendingCommand()
		if k1.Attempt != 2 {
			t.Fatalf("retryable failure did not schedule attempt 2: %+v", k1)
		}
		w.deliverCommand(w.inboxB, k1)
		w.deliverEffects()
		if record := w.record(); record.Step != 1 {
			t.Fatalf("attempt after a retryable failure did not run: %+v", record)
		}
		if got := w.executions(k.IdempotencyKey); got != 2 {
			t.Fatalf("handler ran %d times, want 2 (the retryable failure and the retry)", got)
		}
		// 旧尝试的可重试失败晚到：操作已带结果关闭，按重复确认，不告警。
		before := counterValue("saga.completion.late_after_abandon_total")
		if err := w.complete(Completion{CommandID: k.ID, IdempotencyKey: k.IdempotencyKey, SagaID: k.SagaID, Success: false, Retryable: true, Error: "busy"}); err != nil {
			t.Fatalf("late result of an older attempt = %v, want acknowledged", err)
		}
		if counterValue("saga.completion.late_after_abandon_total") != before {
			t.Fatal("a late result of an operation closed with a result was alarmed as abandoned")
		}
	})

	t.Run("a refusal dropped during backoff is replayed in the same life, not in a resumed one", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.outcome = func(command Command) Completion {
			if strings.Contains(command.ID, ":r1:") {
				return Completion{Success: true}
			}
			return Completion{Success: false, Error: "sender no longer has the items"}
		}
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		w.tick(5 * time.Second)
		if !w.project(recordK) {
			t.Fatal("refusal projected within its deadline was skipped")
		}
		if err := w.complete(w.effectCompletion(recordK)); !errors.Is(err, ErrNotWaiting) {
			t.Fatalf("refusal during backoff = %v, want ErrNotWaiting", err)
		}
		w.tick(10 * time.Second)
		w.deliverCommand(w.inboxB, w.pendingCommand())
		w.deliverReplays()
		record := w.record()
		if record.Status != StatusFailed {
			t.Fatalf("the replayed refusal did not fail the saga: %+v", record)
		}
		if got := w.executions(k.IdempotencyKey); got != 1 {
			t.Fatalf("a refused step ran %d times in one life, want 1", got)
		}
		// 运维修复原因后 Resume：新的一生必须真的重新执行，旧的一生的拒绝不再回放。
		if _, err := w.engine.Resume(w.ctx, ResumeRequest{ID: w.sagaID, Now: w.at(11 * time.Second)}); err != nil {
			t.Fatal(err)
		}
		w.tick(11 * time.Second)
		w.deliverCommand(w.inboxB, w.pendingCommand())
		w.deliverEffects()
		if record := w.record(); record.Step != 1 {
			t.Fatalf("Resume replayed the refusal of the previous life instead of running the step: %+v", record)
		}
		if got := w.executions(k.IdempotencyKey); got != 2 {
			t.Fatalf("handler ran %d times, want 2 (one per life)", got)
		}
	})

	t.Run("after receipt and tombstone TTL a late success is not recorded, not alarmed, not applied", func(t *testing.T) {
		w := newNativeWorld(t, 1, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		recordK := w.commitAttempt(w.inboxA, k)
		if !w.project(recordK) {
			t.Fatal("attempt projected within its deadline was skipped")
		}
		w.tick(5 * time.Second)
		w.store.expireReceiptsAndTombstones()
		before := counterValue("saga.completion.late_after_abandon_total")
		if err := w.complete(w.effectCompletion(recordK)); !errors.Is(err, ErrNotWaiting) {
			t.Fatalf("late success after tombstone TTL = %v, want ErrNotWaiting (dropped; TTL must outlive stream retention)", err)
		}
		if counterValue("saga.completion.late_after_abandon_total") != before {
			t.Fatal("a completion with no tombstone left was alarmed")
		}
		if record := w.record(); record.Status != StatusFailed {
			t.Fatalf("late success after TTL changed the saga: %+v", record)
		}
	})
}

// nativeWorld 是一个协调器 + 两个进程（A、B，各自一个原生收件箱）+ 一个投影器的确定性模型。
type nativeWorld struct {
	t       *testing.T
	ctx     context.Context
	engine  *Engine
	store   *memoryStore
	mongo   *mongotest.Client
	project func(coredata.CommitRecord) bool
	inboxA  *DataEngineStepInbox
	inboxB  *DataEngineStepInbox
	t0      time.Time
	clock   time.Time
	sagaID  string
	outcome func(Command) Completion

	mu          sync.Mutex
	effects     []coredata.CommitRecord
	replays     [][]byte
	execByOp    map[string]int
	debitSerial int64
}

func newNativeWorld(t *testing.T, attempts uint32, t0 time.Time) *nativeWorld {
	return newNativeWorldWithDeadline(t, attempts, t0, 0)
}

func newNativeWorldWithDeadline(t *testing.T, attempts uint32, t0 time.Time, sagaDeadline time.Duration) *nativeWorld {
	t.Helper()
	w := &nativeWorld{t: t, ctx: context.Background(), store: newMemoryStore(), mongo: mongotest.NewClient(), t0: t0, clock: t0, execByOp: map[string]int{}}
	w.outcome = func(Command) Completion { return Completion{Success: true} }
	options := DefaultOptions()
	options.LeaseDuration, options.StoreTimeout, options.PublishTimeout = time.Second, 100*time.Millisecond, 100*time.Millisecond
	engine, err := NewEngine(w.store, PublishFunc(func(context.Context, Command) error { return nil }), options)
	if err != nil {
		t.Fatal(err)
	}
	w.engine = engine
	if err := engine.Register(Definition{Type: "gift", Version: 1, Steps: []Step{
		{Name: "debit", ForwardTopic: "gift.debit", Timeout: 5 * time.Second, MaxAttempts: attempts, BackoffMin: 100 * time.Millisecond, BackoffMax: time.Second},
		{Name: "deliver", ForwardTopic: "gift.deliver", Timeout: 5 * time.Second, MaxAttempts: 5, BackoffMin: 100 * time.Millisecond, BackoffMax: time.Second},
	}}); err != nil {
		t.Fatal(err)
	}
	w.sagaID = fmt.Sprintf("gift-%d", nativeWorldSeq.Add(1))
	start := StartRequest{ID: w.sagaID, Type: "gift", DefinitionVersion: 1, BusinessKey: w.sagaID, Data: []byte("x"), Now: t0}
	if sagaDeadline > 0 {
		start.DeadlineAt = t0.Add(sagaDeadline)
	}
	if _, err := engine.StartSaga(w.ctx, start); err != nil {
		t.Fatal(err)
	}
	projector, err := deengine.NewMongoStore(w.mongo, deengine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 3, TransactionReceiptTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	// project 返回这条记录是否生效（业务写、回执、effect 落库），false 表示被 fence 跳过。
	w.project = func(record coredata.CommitRecord) bool {
		t.Helper()
		if err := projector.Project(w.ctx, record); err != nil {
			t.Fatalf("project %s: %v", record.ID, err)
		}
		var marker bson.M
		if err := w.mongo.Collection("game", deengine.TransactionCollection).FindOne(w.ctx, bson.M{"_id": record.ID.String()}, &marker); err != nil {
			t.Fatalf("transaction marker %s: %v", record.ID, err)
		}
		return marker["skipped"] != true
	}
	newInbox := func(owner string) *DataEngineStepInbox {
		inbox, err := NewDataEngineStepInbox(w.mongo, "game", DataEngineStepInboxOptions{Owner: owner, LeaseDuration: 2 * time.Minute})
		if err != nil {
			t.Fatal(err)
		}
		inbox.now = func() time.Time { return w.clock }
		return inbox
	}
	w.inboxA, w.inboxB = newInbox("game-1300-pid-a"), newInbox("game-1300-pid-b")
	return w
}

var nativeWorldSeq atomic.Int64

func (w *nativeWorld) at(offset time.Duration) time.Time { return w.t0.Add(offset) }

// tick 把协调器与收件箱的时钟推到 t0+offset，并处理到期的 saga。
func (w *nativeWorld) tick(offset time.Duration) {
	w.t.Helper()
	w.clock = w.at(offset)
	records, err := w.store.ClaimDue(w.ctx, ClaimRequest{Owner: "coordinator", Now: w.clock, LeaseDuration: time.Second, Limit: 10})
	if err != nil {
		w.t.Fatal(err)
	}
	for _, record := range records {
		if err := w.engine.processClaimed(w.ctx, record, w.clock); err != nil {
			w.t.Fatal(err)
		}
	}
}

func (w *nativeWorld) record() Record {
	w.t.Helper()
	record, err := w.store.Get(w.ctx, w.sagaID)
	if err != nil {
		w.t.Fatal(err)
	}
	return record
}

// pendingCommand 返回协调器 outbox 里这个 saga 当前排队的命令（每个操作最多一条）。
func (w *nativeWorld) pendingCommand() Command {
	w.t.Helper()
	w.store.mu.Lock()
	defer w.store.mu.Unlock()
	for _, item := range w.store.outbox {
		if item.Command.SagaID == w.sagaID {
			delete(w.store.outbox, item.Command.ID)
			return item.Command
		}
	}
	w.t.Fatal("no command queued")
	return Command{}
}

// commitAttempt 是进程在 handler 里做的事：Reserve、Nest 事务（Bind + 业务写 + EmitCompletion），
// 只写进 WAL（返回 CommitRecord），不投影。
func (w *nativeWorld) commitAttempt(inbox *DataEngineStepInbox, command Command) coredata.CommitRecord {
	w.t.Helper()
	reservation, err := inbox.Reserve(w.ctx, command)
	if err != nil || reservation.Duplicate {
		w.t.Fatalf("reserve %s: %+v %v", command.ID, reservation, err)
	}
	return w.runHandler(inbox, command, reservation)
}

func (w *nativeWorld) runHandler(inbox *DataEngineStepInbox, command Command, reservation Reservation) coredata.CommitRecord {
	w.t.Helper()
	w.mu.Lock()
	w.execByOp[command.IdempotencyKey]++
	w.debitSerial++
	serial := w.debitSerial
	w.mu.Unlock()
	completion := w.outcome(command)
	completion.CommandID, completion.IdempotencyKey, completion.SagaID = command.ID, command.IdempotencyKey, command.SagaID
	committer := &stepFenceCommitter{}
	if _, err := corenest.RunIsolatedTransaction(w.ctx, committer, "gift-debit", func() (any, error) {
		if err := inbox.Bind(command, reservation); err != nil {
			return nil, err
		}
		return nil, EmitCompletion(completion)
	}); err != nil {
		w.t.Fatalf("native transaction %s: %v", command.ID, err)
	}
	record := committer.record
	if completion.Success {
		data, _ := bson.Marshal(bson.M{"_id": serial, "operation": command.IdempotencyKey, "command": command.ID})
		record.Mutations = append(record.Mutations, coredata.Mutation{
			Key:  coredata.DocumentKey{Database: "game", Resource: "debits", ID: serial},
			Kind: coredata.MutationPut, NextVersion: 1, Mask: coredata.AllFields, Schema: 1, Codec: "bson-v2", Data: data,
		})
	}
	return record
}

// deliverCommand 把命令交给一个进程的原生步骤消费者（真实的 SubscribeDataEngineStep）。handler 提交后
// 立即投影，模拟正常的投影延迟；消费者向 saga 结果流重发的 completion 记在 replays 里。
func (w *nativeWorld) deliverCommand(inbox *DataEngineStepInbox, command Command) {
	w.t.Helper()
	client := &replayJetStream{world: w}
	transport, _ := NewJetStreamPublisher(client, "roost.saga")
	handler := func(ctx context.Context, command Command) (Completion, error) {
		reservation, ok := ReservationFromContext(ctx)
		if !ok {
			return Completion{}, errors.New("no reservation")
		}
		record := w.runHandler(inbox, command, reservation)
		if w.project(record) {
			w.mu.Lock()
			w.effects = append(w.effects, record)
			w.mu.Unlock()
		}
		// 返回值与事务里 emit 的 completion 一致（原生 handler 的权威结果是事务里那份）。
		return w.effectCompletion(record), nil
	}
	config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift.debit", AckWait: 30 * time.Second}
	if _, err := SubscribeDataEngineStep(w.ctx, client, transport, inbox, config, handler); err != nil {
		w.t.Fatal(err)
	}
	raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: command})
	if err != nil {
		w.t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(w.ctx, 2*time.Second)
	defer cancel()
	if err := client.handler(ctx, &fnats.JetStreamMsg{Data: raw}); err != nil {
		w.t.Fatalf("deliver %s: %v", command.ID, err)
	}
}

type replayJetStream struct {
	world   *nativeWorld
	handler fnats.JetStreamHandler
}

func (*replayJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (s *replayJetStream) Publish(_ context.Context, subject string, data []byte, _ fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	if strings.HasPrefix(subject, "roost.saga.result.") {
		s.world.mu.Lock()
		s.world.replays = append(s.world.replays, append([]byte(nil), data...))
		s.world.mu.Unlock()
	}
	return fnats.JetStreamPublishAck{}, nil
}
func (s *replayJetStream) Subscribe(_ context.Context, _ fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	s.handler = handler
	return closedSubscription{}, nil
}

func (w *nativeWorld) effectCompletion(record coredata.CommitRecord) Completion {
	w.t.Helper()
	for _, effect := range record.Effects {
		if strings.HasPrefix(effect.Topic, CompletionEffectTopicPrefix) {
			completion, err := DecodeCompletionEffect(effect.Payload)
			if err != nil {
				w.t.Fatal(err)
			}
			return completion
		}
	}
	w.t.Fatal("record has no completion effect")
	return Completion{}
}

func (w *nativeWorld) complete(completion Completion) error {
	_, err := w.engine.Complete(w.ctx, completion)
	return err
}

// deliverEffect 模拟 Nest 完成消费者：投影生效的记录的 completion effect 交给协调器；
// 协调器的 ErrNotWaiting 等终态错误按永久错误丢弃（handleNestCompletion 的行为）。
func (w *nativeWorld) deliverEffect(record coredata.CommitRecord) {
	w.t.Helper()
	if err := w.complete(w.effectCompletion(record)); err != nil && !isTerminalCompletionError(err) {
		w.t.Fatalf("complete: %v", err)
	}
}

func (w *nativeWorld) deliverEffects() {
	w.t.Helper()
	w.mu.Lock()
	records := w.effects
	w.effects = nil
	w.mu.Unlock()
	for _, record := range records {
		w.deliverEffect(record)
	}
}

// deliverReplays 模拟 saga 结果流的完成消费者（SubscribeCompletions）。
func (w *nativeWorld) deliverReplays() {
	w.t.Helper()
	w.mu.Lock()
	replays := w.replays
	w.replays = nil
	w.mu.Unlock()
	for _, raw := range replays {
		var envelope completionEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			w.t.Fatal(err)
		}
		if err := w.complete(envelope.Completion); err != nil {
			w.t.Fatalf("replayed completion: %v", err)
		}
	}
	w.deliverEffects()
}

func (w *nativeWorld) executions(operation string) int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.execByOp[operation]
}

// assertEffective 断言这个操作实例真实生效的次数：投影落库的业务写与成功回执各 want 个。
func (w *nativeWorld) assertEffective(operation string, want int) {
	w.t.Helper()
	var debits []bson.M
	if err := w.mongo.Collection("game", "debits").Find(w.ctx, bson.M{"operation": operation}, &debits); err != nil {
		w.t.Fatal(err)
	}
	var receipts []dataEngineReceipt
	if err := w.mongo.Collection("game", dataEngineReceiptCollection).Find(w.ctx, bson.M{}, &receipts); err != nil {
		w.t.Fatal(err)
	}
	successes := 0
	for _, receipt := range receipts {
		completion, err := DecodeCompletionEffect(receipt.Payload)
		if err == nil && completion.IdempotencyKey == operation && completion.Success {
			successes++
		}
	}
	if len(debits) != want || successes != want {
		commands := make([]string, 0, len(debits))
		for _, debit := range debits {
			commands = append(commands, fmt.Sprint(debit["command"]))
		}
		w.t.Errorf("operation %s took effect %d time(s) with %d success receipt(s), want %d: debits by %v (handler ran %d times)",
			operation, len(debits), successes, want, commands, w.executions(operation))
	}
}

func counterValue(name string) int64 {
	var total int64
	for _, metric := range metrics.Snapshot() {
		if metric.Name == name {
			total += metric.Value
		}
	}
	return total
}

// expireReceiptsAndTombstones 模拟协调器侧 completion receipt 与 operation tombstone 过了 TTL。
func (s *memoryStore) expireReceiptsAndTombstones() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.receipts = map[string]Completion{}
	s.closed = map[string]string{}
	s.closures = map[string]OperationClosure{}
	s.lateAlarms = map[string]bool{}
}

// B：claim 租约封顶到命令截止时间，新建与接管都一样；已过截止的命令不再分配租约。
func TestNativeStepLeaseNeverOutlivesTheCommandDeadline(t *testing.T) {
	client := mongotest.NewClient()
	inbox, err := NewDataEngineStepInbox(client, "game", DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: 2 * time.Minute})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	inbox.now = func() time.Time { return now }
	command := dataEngineCommand("gift-l:1:0:1", "gift-l:1:0", "x")
	command.DeadlineAt = now.Add(5 * time.Second)
	if _, err := inbox.Reserve(context.Background(), command); err != nil {
		t.Fatal(err)
	}
	claim := func() stepOperation { return inboxOperation(t, client, command.IdempotencyKey) }
	if got := claim(); !got.LeaseUntil.Equal(command.DeadlineAt) || got.CommandID != command.ID {
		t.Fatalf("operation lease_until=%v current=%q, want the command deadline %v (LeaseDuration 2m is longer)", got.LeaseUntil, got.CommandID, command.DeadlineAt)
	}
	// 同一命令在截止前的接管（例如租约被交还）同样封顶。
	if err := inbox.releaseLease(context.Background(), inbox.activeReservation(command.IdempotencyKey, command.ID, mustCommandDigest(t, command), 1)); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Second)
	if reservation, err := inbox.Reserve(context.Background(), command); err != nil || reservation.Duplicate || reservation.Token != 2 {
		t.Fatalf("takeover=%+v err=%v", reservation, err)
	}
	if got := claim(); !got.LeaseUntil.Equal(command.DeadlineAt) {
		t.Fatalf("taken-over claim lease_until=%v, want the command deadline %v", got.LeaseUntil, command.DeadlineAt)
	}
	now = command.DeadlineAt
	expired := command
	expired.ID, expired.Attempt = "gift-l:1:0:2", 2
	expired.DeadlineAt = now
	if _, err := inbox.Reserve(context.Background(), expired); !errors.Is(err, ErrCommandExpired) {
		t.Fatalf("reserve of a command at its deadline err=%v, want ErrCommandExpired", err)
	}
}

// 消费者对 Reserve 结论的处理：接替后的旧尝试 ack 且不执行；另一尝试租约仍有效时返回可重试错误
// （nak 后重投）；原生 handler 返回零值 Completion 不再导致多一次 nak（结果在事务里 emit）。
func TestNativeStepConsumerHandlesOperationOutcomes(t *testing.T) {
	t.Run("superseded attempt is acknowledged without running", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		_ = w.commitAttempt(w.inboxA, k)
		w.tick(5 * time.Second)
		w.tick(10 * time.Second)
		w.deliverCommand(w.inboxB, w.pendingCommand()) // k+1 接替 k 并执行
		w.clock = w.at(4 * time.Second)                // k 的一条迟到重投（k 自己的截止之前，消费者仍会处理它）
		w.deliverCommand(w.inboxA, k)
		if got := w.executions(k.IdempotencyKey); got != 2 {
			t.Fatalf("handler ran %d times, want 2 (k admitted before the crash, k+1); the superseded k must not run again", got)
		}
	})

	t.Run("live lease of another attempt is retried later", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		_ = w.commitAttempt(w.inboxA, k)
		k1 := k
		k1.ID, k1.Attempt = k.IdempotencyKey+":2", 2
		k1.DeadlineAt = k.DeadlineAt.Add(10 * time.Second)
		client := &replayJetStream{world: w}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		handler := func(context.Context, Command) (Completion, error) {
			t.Fatal("ran a second attempt while the first one's lease was live")
			return Completion{}, nil
		}
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift.debit", AckWait: 30 * time.Second}
		if _, err := SubscribeDataEngineStep(w.ctx, client, transport, w.inboxB, config, handler); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(commandEnvelope{Version: WireVersion, Command: k1})
		if err := client.handler(w.ctx, &fnats.JetStreamMsg{Data: raw}); !errors.Is(err, errOperationAttemptInFlight) {
			t.Fatalf("delivery while another attempt holds a live lease = %v, want errOperationAttemptInFlight (nak, retried)", err)
		}
	})

	t.Run("zero completion from a native handler is not a delivery failure", func(t *testing.T) {
		w := newNativeWorld(t, 15, time.Now().UTC())
		w.tick(0)
		k := w.pendingCommand()
		client := &replayJetStream{world: w}
		transport, _ := NewJetStreamPublisher(client, "roost.saga")
		handler := func(ctx context.Context, command Command) (Completion, error) {
			reservation, _ := ReservationFromContext(ctx)
			w.project(w.runHandler(w.inboxA, command, reservation))
			return Completion{}, nil // 生成模板的写法：结果已在事务里 emit
		}
		config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-gift-debit", Topic: "gift.debit", AckWait: 30 * time.Second}
		if _, err := SubscribeDataEngineStep(w.ctx, client, transport, w.inboxA, config, handler); err != nil {
			t.Fatal(err)
		}
		raw, _ := json.Marshal(commandEnvelope{Version: WireVersion, Command: k})
		if err := client.handler(w.ctx, &fnats.JetStreamMsg{Data: raw}); err != nil {
			t.Fatalf("successful native step with a zero returned Completion = %v, want nil (ack): the authoritative completion is the one emitted in the transaction", err)
		}
	})
}

// C' 在 MongoStore 上：tombstone 记录关闭方式；放弃关闭后到达的成功告警，带结果关闭后的旧结果按重复处理，
// U-0280 之前写的 tombstone（没有 closure 字段）不告警；Resume 后的新一生带结果关闭时改记为“带结果”。
func TestMongoStoreTombstoneTellsAbandonedFromResolved(t *testing.T) {
	ctx := context.Background()
	t.Run("abandoned by timeout, then a late success", func(t *testing.T) {
		engine, store, waiting := waitingOnMongo(t)
		// rally 的 march 步骤 MaxAttempts 2：库里的记录已是第 2 次尝试，超时即用尽 → 补偿 reserve。
		// 领取走真实的 ClaimDue（带租约 fence）；之前 mongotest 不支持 ClaimDue 的 $in []Status，
		// 这里绕开领取直接交给 processClaimed（RR-20261006-08，O-S5-6）。
		record := waiting.Clone()
		record.Attempt, record.Version = 2, waiting.Version+1
		record.CommandID = commandID(record.OperationKey, 0, 2)
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: waiting.Version, After: record}); err != nil {
			t.Fatal(err)
		}
		now := record.NextRunAt.Add(time.Second)
		claimed, err := store.ClaimDue(ctx, ClaimRequest{Owner: engine.opts.Owner, Now: now, LeaseDuration: time.Minute, Limit: 8})
		if err != nil || len(claimed) != 1 || claimed[0].Attempt != 2 {
			t.Fatalf("ClaimDue = %+v err=%v, want the waiting record at attempt 2", claimed, err)
		}
		if err := engine.processClaimed(ctx, claimed[0], now); err != nil {
			t.Fatal(err)
		}
		late := Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
		history, err := store.CompletionHistory(ctx, late)
		if err != nil || !history.Recorded || history.Receipt || history.Closure != OperationAbandoned {
			t.Fatalf("history after timeout exhaustion=%+v err=%v, want an abandoned tombstone", history, err)
		}
		before := engine.Stats().LateAfterAbandon
		if _, err := engine.Complete(ctx, late); err != nil {
			t.Fatal(err)
		}
		if engine.Stats().LateAfterAbandon != before+1 {
			t.Fatal("late success after abandonment was not counted")
		}
	})
	t.Run("closed with a result, then an older attempt's result", func(t *testing.T) {
		engine, store, record := waitingOnMongo(t)
		if _, err := engine.Complete(ctx, Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}); err != nil {
			t.Fatal(err)
		}
		older := Completion{CommandID: record.OperationKey + ":0", IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true}
		history, err := store.CompletionHistory(ctx, older)
		if err != nil || !history.Recorded || history.Closure != OperationClosedWithResult {
			t.Fatalf("history=%+v err=%v, want a result tombstone", history, err)
		}
		before := engine.Stats()
		if _, err := engine.Complete(ctx, older); err != nil {
			t.Fatal(err)
		}
		if after := engine.Stats(); after.LateAfterAbandon != before.LateAfterAbandon || after.Duplicates != before.Duplicates+1 {
			t.Fatalf("older result after a resolved operation: stats %+v -> %+v, want a duplicate", before, after)
		}
	})
	t.Run("tombstone written before U-0280 has no closure", func(t *testing.T) {
		engine, store, record := waitingOnMongo(t)
		if err := store.client.(*mongotest.Client).Collection("saga", defaultOperationCollection).Seed(bson.M{"_id": "gift-1:1:0", "saga_id": record.ID, "created_at": record.CreatedAt}); err != nil {
			t.Fatal(err)
		}
		late := Completion{CommandID: "gift-1:1:0:1", IdempotencyKey: "gift-1:1:0", SagaID: record.ID, Success: true}
		before := engine.Stats()
		if _, err := engine.Complete(ctx, late); err != nil {
			t.Fatal(err)
		}
		if after := engine.Stats(); after.LateAfterAbandon != before.LateAfterAbandon || after.Duplicates != before.Duplicates+1 {
			t.Fatalf("legacy tombstone: stats %+v -> %+v, want a duplicate and no alarm", before, after)
		}
	})
	t.Run("a resumed life resolving an abandoned operation upgrades the tombstone", func(t *testing.T) {
		_, store, record := waitingOnMongo(t)
		after := record.Clone()
		after.Version++
		after.Status, after.NextRunAt, after.OperationKey, after.CommandID = StatusPending, record.NextRunAt, "", ""
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: after, CloseOperation: record.OperationKey}); err != nil {
			t.Fatal(err)
		}
		resolved := after.Clone()
		resolved.Version++
		receipt := Completion{CommandID: record.OperationKey + ":r1:1", IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true, CompletedAt: time.Now().UTC()}
		if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: after.Version, After: resolved, Receipt: &receipt, CloseOperation: record.OperationKey}); err != nil {
			t.Fatal(err)
		}
		history, err := store.CompletionHistory(ctx, Completion{CommandID: record.CommandID, IdempotencyKey: record.OperationKey, SagaID: record.ID, Success: true})
		if err != nil || history.Closure != OperationClosedWithResult {
			t.Fatalf("history=%+v err=%v, want the tombstone upgraded to a result closure", history, err)
		}
	})
}
