package persistflow

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/saga"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// fencedStep 通过正式 DataEngineStepInbox 取得租约，然后让 trade_fenced 在 Nest 里
// 修改 Trader 并 Bind 该租约——与生产的原生 saga 步骤同一条链路。
type fencedStep struct {
	inbox       *saga.DataEngineStepInbox
	command     saga.Command
	reservation saga.Reservation
}

func reserveFencedStep(t *testing.T, ctx context.Context, h *tradeFixture, name string) fencedStep {
	t.Helper()
	inbox, err := saga.NewDataEngineStepInbox(h.client, h.database, saga.DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	command := saga.Command{
		ID: fmt.Sprintf("%s-%d", name, now.UnixNano()), IdempotencyKey: name, SagaID: "saga-" + name, SagaType: "gift",
		DefinitionVersion: 1, BusinessKey: name, StepName: "debit", Phase: saga.PhaseForward, Attempt: 1,
		Topic: "gift.debit", CreatedAt: now, DeadlineAt: now.Add(time.Hour),
	}
	reservation, err := inbox.Reserve(ctx, command)
	if err != nil || reservation.Duplicate {
		t.Fatalf("reserve=%v err=%v", reservation, err)
	}
	return fencedStep{inbox: inbox, command: command, reservation: reservation}
}

// expire 把这个操作状态文档（saga 原生收件箱，每个操作一份，_id = IdempotencyKey）的租约改到过去：
// 投影时 fence 不再成立，整笔记录被跳过。
func (step fencedStep) expire(t *testing.T, ctx context.Context, h *tradeFixture) {
	t.Helper()
	operations := h.client.Database(h.database).Collection("_dataengine_step_operations")
	if _, err := operations.UpdateOne(ctx, bson.M{"_id": step.command.IdempotencyKey}, bson.M{"$set": bson.M{"lease_until": time.Now().UTC().Add(-time.Second)}}); err != nil {
		t.Fatal(err)
	}
}

func (step fencedStep) params() nest.Params {
	return nest.Params{step.inbox, step.command, step.reservation}
}

func (h *tradeFixture) mongoState(t *testing.T, id int64) tradeState {
	t.Helper()
	var wallet struct {
		Coins     int64  `bson:"coins"`
		Transfers int64  `bson:"transfers"`
		Version   uint64 `bson:"_version"`
	}
	var inventory struct {
		Items int64 `bson:"items"`
	}
	if err := h.client.Database(h.database).Collection("trade_wallets").FindOne(h.ctx, bson.M{"_id": id}, &wallet); err != nil {
		t.Fatal(err)
	}
	if err := h.client.Database(h.database).Collection("trade_inventories").FindOne(h.ctx, bson.M{"_id": id}, &inventory); err != nil {
		t.Fatal(err)
	}
	return tradeState{Coins: wallet.Coins, Items: inventory.Items, Transfers: wallet.Transfers, Version: wallet.Version}
}

func (h *tradeFixture) memoryState(t *testing.T, id int64) tradeState {
	t.Helper()
	e, ok := h.access.Manager().Get(id).(*Trader)
	if !ok {
		t.Fatalf("trader %d is not resident", id)
	}
	return tradeState{Coins: e.wallet.GetCoins(), Items: e.inventory.GetItems(), Transfers: e.wallet.GetTransfers(), Version: e.wallet.DirtyTracker().Version()}
}

func (h *tradeFixture) noFatal(t *testing.T, flushErr error, what string) {
	t.Helper()
	select {
	case fatal := <-h.fatal:
		t.Fatalf("%s: projector fenced: %v (flush=%v)", what, fatal, flushErr)
	default:
	}
	if flushErr != nil {
		t.Fatalf("%s: flush=%v", what, flushErr)
	}
}

// syncFrames 记录 entitysync 推给会话 1 的帧，解出 Trader 的 coins 与是否全量。
type syncFrames struct {
	mu     sync.Mutex
	frames [][]byte
}

func (recorder *syncFrames) push(_ context.Context, _ entitysync.SessionID, data []byte) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.frames = append(recorder.frames, append([]byte(nil), data...))
	return nil
}

type traderFrame struct {
	operation frame.ObjectOperation
	full      bool
	coins     int64
}

func (recorder *syncFrames) take(t *testing.T, id int64) []traderFrame {
	t.Helper()
	recorder.mu.Lock()
	frames := recorder.frames
	recorder.frames = nil
	recorder.mu.Unlock()
	var out []traderFrame
	for _, data := range frames {
		decoded, err := entitysync.DecodeFrame(data, frame.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		for _, object := range decoded.Objects {
			for _, component := range object.Components {
				update, err := entitysync.DecodeSubjectUpdate(component.Data, 0)
				if err != nil {
					t.Fatal(err)
				}
				if update.SubjectID != id {
					continue
				}
				var snapshot traderSnapshot
				if err := bson.Unmarshal(update.Payload.BytesCopy(), &snapshot); err != nil {
					t.Fatal(err)
				}
				out = append(out, traderFrame{operation: object.Operation, full: update.Full, coins: snapshot.Coins})
			}
		}
	}
	return out
}

// RR-20260926-30：原生 saga 步骤（本地 Trader 扣款 + lease fence）已在 Nest 内存提交，投影时租约已过期，
// 记录被跳过。旧实现仍接受同一 Trader 的下一笔事务（按含扣款的内存版本准入 WAL），它投影时判 fatal
// 冲突，Projector fence；重启回放同一冲突起不来。走正式生成 Entity、Nest、文件 WAL 与真实 Mongo。
// 承诺：步骤未投影期间同实体事务以可重试的 ErrFencedEntityPending 拒绝；跳过后实体被驱逐（不持久化），
// 重载后内存 == Mongo，Sync 订阅者收到全量；下一笔事务不 fatal；重启恢复通过。
func TestGeneratedDataEngineFencedStepSkipDoesNotFenceTheEntity(t *testing.T) {
	ctx, client := reloadTestClient(t)
	const base = 33000
	clearTraderDocuments(t, ctx, client, base, 1)
	root := t.TempDir()
	// Sync：Nest 按正式装配接入 entitysync（setter 标脏、提交确认后可交付）；订阅者先拿到快照；
	// Runtime 重载实体后把 subject 接到新对象上（kit nest Mod 的正式装配同此）。
	recorder := &syncFrames{}
	syncManager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: entitysync.TransportFunc(recorder.push)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = syncManager.Close(context.Background()) })
	h := newTradeFixtureWith(t, ctx, client, root, "fenced", nest.DurabilityStrict, false, []nest.NestOption{nest.NestOptionWithEntitySync(syncManager)})
	id := h.seed(t, base, 1)[0]
	seeded := h.mongoState(t, id)
	unhook := h.runtime.Repository.OnEntityLoaded(func(loaded entity.IThreadSafeEntity) {
		if state := loaded.Base().Sync(); state != nil {
			_ = syncManager.Rebind(state)
		}
	})
	defer unhook()
	if err := syncManager.Register(h.access.Manager().Get(id).Base().Sync()); err != nil {
		t.Fatal(err)
	}
	if err := syncManager.OpenSession(1); err != nil {
		t.Fatal(err)
	}
	if err := syncManager.Subscribe(1, id, entity.SyncProfile{}); err != nil {
		t.Fatal(err)
	}
	if err := syncManager.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recorder.take(t, id); len(got) != 1 || got[0].operation != frame.ObjectCreate || got[0].coins != seeded.Coins {
		t.Fatalf("first sync frame=%+v, want a create with coins=%d", got, seeded.Coins)
	}

	step := reserveFencedStep(t, ctx, h, "rr30")
	pause := h.gate.pause()
	defer pause.resume()
	if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_fenced"), id, step.params()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-pause.entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := syncManager.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	// 被跳过的扣款已经作为一次对象更新发给了订阅者（Trader 的打包器每次都给整份内容）。
	if got := recorder.take(t, id); len(got) != 1 || got[0].operation != frame.ObjectUpdate || got[0].coins != seeded.Coins-100 {
		t.Fatalf("sync frame for the debit=%+v, want an update with coins=%d", got, seeded.Coins-100)
	}
	step.expire(t, ctx, h)
	_, buyErr := h.scheduler.Request(ctx, nest.NewHandlerName("trade_buy"), id, nil)
	pause.resume()
	flushErr := h.runtime.Flush(ctx)
	if errors.Is(flushErr, engine.ErrProjectionConflict) {
		t.Fatalf("the next Trader transaction after a skipped fenced step fenced the projector: buy=%v flush=%v", buyErr, flushErr)
	}
	h.noFatal(t, flushErr, "after the skipped step")
	if !errors.Is(buyErr, coredata.ErrFencedEntityPending) || !errors.Is(buyErr, nest.ErrCommitRejected) {
		t.Fatalf("trade_buy behind the unprojected fenced step=%v, want a retryable ErrFencedEntityPending rejection", buyErr)
	}
	if got := h.mongoState(t, id); got != seeded {
		t.Fatalf("skipped step changed Mongo: %+v want %+v", got, seeded)
	}

	// 跳过后驱逐：WaitEntityProjection 覆盖驱逐，返回时实体已不在内存。
	if err := h.runtime.WaitEntityProjection(ctx, id); err != nil {
		t.Fatal(err)
	}
	if h.access.Manager().Get(id) != nil {
		t.Fatal("the Trader carrying the skipped debit is still resident")
	}
	if stats := h.runtime.Projector.Stats(); stats.StaleEvictions != 1 || stats.FencedEntities != 0 {
		t.Fatalf("projector stats=%+v", stats)
	}
	if err := syncManager.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recorder.take(t, id); len(got) != 0 {
		t.Fatalf("an evicted subject still synced: %+v", got)
	}

	// 下次访问从 Mongo 重载；Sync 对订阅者强制全量（整份替换，不是接着旧版本的增量）。
	if _, err := h.runtime.Repository.LoadEntity(ctx, id, EntityKindTrader); err != nil {
		t.Fatal(err)
	}
	if memory := h.memoryState(t, id); memory != seeded {
		t.Fatalf("reloaded memory=%+v Mongo=%+v", memory, seeded)
	}
	if err := syncManager.Flush(ctx); err != nil {
		t.Fatal(err)
	}
	if got := recorder.take(t, id); len(got) != 1 || got[0].operation != frame.ObjectUpdate || !got[0].full || got[0].coins != seeded.Coins {
		t.Fatalf("sync after the reload=%+v, want a full ObjectUpdate with coins=%d", got, seeded.Coins)
	}

	// 屏障已解除：同一笔 trade_buy 重试成功，投影不 fatal，内存 == Mongo。
	if _, err := h.scheduler.Request(ctx, nest.NewHandlerName("trade_buy"), id, nil); err != nil {
		t.Fatalf("trade_buy after the eviction: %v", err)
	}
	h.noFatal(t, h.runtime.Flush(ctx), "after the retried trade_buy")
	bought := h.mongoState(t, id)
	if memory := h.memoryState(t, id); memory != bought || bought.Coins != seeded.Coins-5 || bought.Items != seeded.Items+1 {
		t.Fatalf("after trade_buy memory=%+v Mongo=%+v seeded=%+v", memory, bought, seeded)
	}
	h.close(t, nil)

	// 重启：同一 WAL 目录启动恢复通过，加载得到 Mongo 状态，之后的事务正常投影。
	restarted := newTradeFixture(t, ctx, client, root, "fenced", nest.DurabilityStrict, false)
	if _, err := restarted.runtime.Repository.LoadEntity(ctx, id, EntityKindTrader); err != nil {
		t.Fatal(err)
	}
	if memory := restarted.memoryState(t, id); memory != bought {
		t.Fatalf("after restart memory=%+v Mongo=%+v", memory, bought)
	}
	if _, err := restarted.scheduler.Request(ctx, nest.NewHandlerName("trade_buy"), id, nil); err != nil {
		t.Fatal(err)
	}
	restarted.noFatal(t, restarted.runtime.Flush(ctx), "after restart")
	if memory, stored := restarted.memoryState(t, id), restarted.mongoState(t, id); memory != stored {
		t.Fatalf("after restart memory=%+v Mongo=%+v", memory, stored)
	}
	restarted.close(t, nil)
}
