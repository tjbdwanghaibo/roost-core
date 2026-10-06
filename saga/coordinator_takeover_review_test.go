package saga

import (
	"context"
	"errors"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
)

// N06 S5 review（2026-10-06）：协调器 A 领取记录后租约过期，B 接管；A 之后才执行 Apply。承诺：A 的晚到 Apply
// 一律 ErrConflict，不覆盖 B 的结论、不重复推进（attempt 不多加、outbox 不多出一条命令）。fence 由 MongoStore.Apply
// 的过滤条件给出：version 相等，且 ExpectedLease 的 owner + token 相等（ClaimDue 对每次领取 $inc lease_token）。
// 覆盖三种到达顺序：B 已领取未 Apply、B 已 Apply、以及 A 在超时分支上与 completion 交错。
//
// 领取走完整的 MongoStore.ClaimDue；真实 Mongo 上的同一组用例见 -tags integration 的 TestRealMongoCoordinatorLeaseTakeover。
// 之前 mongotest 不支持 ClaimDue 候选查询里的 `$in []Status`，这里只能跑领取的第二段；替身已按底层类型展开具名切片
// （RR-20261006-08，O-S5-6），绕行去掉。
func TestCoordinatorLeaseTakeoverFencesTheLateApply(t *testing.T) {
	runCoordinatorTakeoverCases(t, func(t *testing.T) fmongo.IMongo { return mongotest.NewClient() }, "takeover")
}

func runCoordinatorTakeoverCases(t *testing.T, newClient func(*testing.T) fmongo.IMongo, database string) {
	ctx := context.Background()
	setup := func(t *testing.T, status Status) (*MongoStore, *Engine, *Engine, Record, time.Time) {
		t.Helper()
		// 每个子用例用自己的集合：ClaimDue 领取全部到期记录，共用集合时前一个子用例留下的记录会被一起领到。
		suffix := "_" + NewID()
		store, err := NewMongoStore(newClient(t), MongoStoreOptions{Database: database, SagaCollection: "_sagas" + suffix, OutboxCollection: "_saga_outbox" + suffix, CompletionCollection: "_saga_completions" + suffix, OperationCollection: "_saga_operations" + suffix})
		if err != nil {
			t.Fatal(err)
		}
		if err := store.EnsureInfrastructure(ctx); err != nil {
			t.Fatal(err)
		}
		newEngine := func(owner string) *Engine {
			options := DefaultOptions()
			options.Owner = owner
			engine, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), options)
			if err != nil {
				t.Fatal(err)
			}
			if err := engine.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			return engine
		}
		now := time.Now().UTC().Truncate(time.Millisecond)
		id := "takeover-" + NewID()
		record := Record{ID: id, Type: "rally", DefinitionVersion: 1, BusinessKey: id, Status: StatusPending, Phase: PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
		if err := store.Create(ctx, record); err != nil {
			t.Fatal(err)
		}
		a, b := newEngine("coordinator-a"), newEngine("coordinator-b")
		if status == StatusWaiting {
			if err := a.processClaimed(ctx, record, now); err != nil {
				t.Fatal(err)
			}
			current, err := store.Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			record = current
			now = current.NextRunAt // 第一次尝试的截止：协调器下一次领取它是为了判超时
		}
		return store, a, b, record, now
	}
	claim := func(t *testing.T, store *MongoStore, owner string, now time.Time) Record {
		t.Helper()
		records, err := store.ClaimDue(ctx, ClaimRequest{Owner: owner, Now: now, LeaseDuration: 2 * time.Second, Limit: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(records) != 1 {
			t.Fatalf("%s claimed %d records, want 1", owner, len(records))
		}
		return records[0]
	}
	queued := func(t *testing.T, store *MongoStore, now time.Time) []OutboxRecord {
		t.Helper()
		items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: now.Add(time.Hour), LeaseDuration: time.Minute, Limit: 16})
		if err != nil {
			t.Fatal(err)
		}
		return items
	}

	t.Run("B claimed, A applies before B", func(t *testing.T) {
		store, a, b, _, now := setup(t, StatusPending)
		byA := claim(t, store, "coordinator-a", now)
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second)) // A 的租约已过期
		if err := a.processClaimed(ctx, byA, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late dispatch after B took the lease = %v, want ErrConflict", err)
		}
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); err != nil {
			t.Fatalf("B's dispatch after A's rejected one = %v", err)
		}
		final, err := store.Get(ctx, byB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != StatusWaiting || final.Attempt != 1 || final.Version != byB.Version+1 {
			t.Fatalf("after takeover: %+v, want one dispatch (attempt 1)", final)
		}
		if items := queued(t, store, now); len(items) != 1 || items[0].Command.ID != final.CommandID {
			t.Fatalf("outbox after takeover: %+v, want exactly B's command %s", items, final.CommandID)
		}
	})

	t.Run("B already applied, A applies late", func(t *testing.T) {
		store, a, b, _, now := setup(t, StatusPending)
		byA := claim(t, store, "coordinator-a", now)
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second))
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := a.processClaimed(ctx, byA, now.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late dispatch after B applied = %v, want ErrConflict", err)
		}
		final, err := store.Get(ctx, byB.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Attempt != 1 || final.Version != byB.Version+1 {
			t.Fatalf("A's late apply moved the record: %+v", final)
		}
		if items := queued(t, store, now); len(items) != 1 {
			t.Fatalf("outbox has %d commands, want 1", len(items))
		}
	})

	t.Run("A's timeout decision loses to B's and to a completion", func(t *testing.T) {
		store, a, b, waiting, now := setup(t, StatusWaiting)
		byA := claim(t, store, "coordinator-a", now) // A 领取它是为了判超时
		byB := claim(t, store, "coordinator-b", now.Add(3*time.Second))
		// 结果在 B 判超时之前到达：Complete 只按版本 fence，接收它。
		if _, err := b.Complete(ctx, Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: waiting.ID, Success: true}); err != nil {
			t.Fatal(err)
		}
		if err := b.processClaimed(ctx, byB, now.Add(3*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("B's timeout after the completion = %v, want ErrConflict", err)
		}
		if err := a.processClaimed(ctx, byA, now.Add(4*time.Second)); !errors.Is(err, ErrConflict) {
			t.Fatalf("A's late timeout = %v, want ErrConflict", err)
		}
		final, err := store.Get(ctx, waiting.ID)
		if err != nil {
			t.Fatal(err)
		}
		if final.Status != StatusPending || final.Step != 1 || final.CompletedSteps != 1 || final.Attempt != 0 {
			t.Fatalf("the accepted completion was overwritten by a stale timeout: %+v", final)
		}
	})
}

// N06 S5 review（2026-10-06）：发布器与协调器在同一操作的 outbox 上交错。
//   - 发布器领取了第 1 次尝试的命令、正在发布，协调器判超时后派发第 2 次尝试（Apply 带 Outbox 时按 IdempotencyKey
//     删掉旧命令再插入新命令）：发布器之后的 Ack / Nack 都是 ErrConflict，不能把被替换的命令写回去；outbox 只剩第 2 次。
//   - 发布成功但 Ack 结果未知（Ack 没有执行）：租约过期后另一个发布器重新领取到同一条命令，CommandID 不变——它就是
//     JetStream 的 MsgID，去重窗口内的重发被 broker 去重，窗口外的重发由步骤收件箱按 CommandID 去重。
func TestOutboxSupersedeAndUnknownAckOnMongoStore(t *testing.T) {
	ctx := context.Background()
	engine, store, record := waitingOnMongo(t)
	now := record.CreatedAt
	// waitingOnMongo 直接写入 Waiting 记录，没有 outbox；先按正常路径派发出第 1 次尝试的命令。
	pending := record.Clone()
	pending.Version++
	pending.Status, pending.Attempt, pending.OperationKey, pending.CommandID = StatusPending, 0, "", ""
	if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: record.Version, After: pending}); err != nil {
		t.Fatal(err)
	}
	if err := engine.processClaimed(ctx, pending, now); err != nil {
		t.Fatal(err)
	}
	first, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	claimed, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "publisher-a", Now: now, LeaseDuration: 2 * time.Second, Limit: 4})
	if err != nil || len(claimed) != 1 || claimed[0].Command.ID != first.CommandID {
		t.Fatalf("publisher-a claim = %+v, %v; want the first attempt %s", claimed, err, first.CommandID)
	}

	t.Run("unknown ack: the same CommandID is published again after the lease", func(t *testing.T) {
		again, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "publisher-b", Now: now.Add(3 * time.Second), LeaseDuration: 2 * time.Second, Limit: 4})
		if err != nil || len(again) != 1 || again[0].Command.ID != first.CommandID {
			t.Fatalf("re-claim after publisher-a's lease = %+v, %v; want the same command %s", again, err, first.CommandID)
		}
		if err := store.AckOutbox(ctx, first.CommandID, claimed[0].Lease); !errors.Is(err, ErrConflict) {
			t.Fatalf("publisher-a's late ack after publisher-b took the lease = %v, want ErrConflict", err)
		}
		claimed = again
	})

	t.Run("supersede while a publisher holds the old command", func(t *testing.T) {
		// 第 1 次尝试超时：退避后派发第 2 次。
		if err := engine.processClaimed(ctx, first, first.NextRunAt); err != nil {
			t.Fatal(err)
		}
		backoff, err := store.Get(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if err := engine.processClaimed(ctx, backoff, backoff.NextRunAt); err != nil {
			t.Fatal(err)
		}
		second, err := store.Get(ctx, record.ID)
		if err != nil {
			t.Fatal(err)
		}
		if second.Attempt != 2 {
			t.Fatalf("want the second attempt dispatched: %+v", second)
		}
		holder := claimed[0]
		if err := store.NackOutbox(ctx, holder.Command.ID, holder.Lease, now.Add(time.Minute), "publish failed"); !errors.Is(err, ErrConflict) {
			t.Fatalf("nack of a superseded command = %v, want ErrConflict", err)
		}
		if err := store.AckOutbox(ctx, holder.Command.ID, holder.Lease); !errors.Is(err, ErrConflict) {
			t.Fatalf("ack of a superseded command = %v, want ErrConflict", err)
		}
		items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "audit", Now: now.Add(time.Hour), LeaseDuration: time.Minute, Limit: 8})
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 1 || items[0].Command.ID != second.CommandID {
			t.Fatalf("outbox after supersede = %+v, want only %s", items, second.CommandID)
		}
	})
}
