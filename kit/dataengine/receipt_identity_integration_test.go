//go:build integration

package dataengine

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20260926-34 的真实 Mongo 回归（REPRO-2026-09-26-04 §2 改写），走正式 kit 装配出的
// MongoStore。真实服务端在事务内任一写错误后中止事务，之后同一事务的读返回带
// TransientTransactionError 标签的 NoSuchTransaction；投影若撞唯一键后仍在事务内读回裁决，
// 驱动会重跑回调直到 transaction_timeout，“相同即成功 / 漂移即 fatal”都退化成非 fatal 超时，
// WAL 头部卡住。这里对每种身份裁决断言结论、fatal 分类、DAO 是否落库，以及耗时远小于事务超时。

// rr34TransactionTimeout 与 newRealFixtureWithNATS 的 mongo.transaction_timeout 一致。
const rr34TransactionTimeout = 15 * time.Second

// rr34Budget 是一次裁决允许的耗时：驱动重跑到超时必然越过它。
const rr34Budget = rr34TransactionTimeout / 5

const rr34Resource = "rr34_heroes"

type rr34Outcome struct {
	err     error
	elapsed time.Duration
	fatal   uint64
	written bool
}

// projectThroughWAL 把一条记录写进独立 WAL，由手动 Projector 投影一遍：fatal 分类与
// 生产投影器是同一段代码。每次用新的 WAL/Projector，前一个用例的 fatal 不会遮住后一个。
func projectThroughWAL(t *testing.T, fx *realFixture, record coredata.CommitRecord) rr34Outcome {
	t.Helper()
	opts := nestwal.DefaultOptions(t.TempDir())
	wal, err := nestwal.Open(opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = wal.Close(context.Background()) })
	projector := manualRealProjector(t, wal, fx.runtime.Store)
	if _, err := wal.Append(fx.context(), record); err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	_, err = projector.ReplayPass(fx.context())
	elapsed := time.Since(start)
	written, countErr := fx.mongo.Database(fx.database).Collection(rr34Resource).CountDocuments(context.Background(), bson.M{"_id": record.Mutations[0].Key.ID})
	if countErr != nil {
		t.Fatal(countErr)
	}
	return rr34Outcome{err: err, elapsed: elapsed, fatal: projector.Stats().FatalProjectionConflicts, written: written == 1}
}

func TestRealProjectionIdentityVerdictsAcrossTransactions(t *testing.T) {
	fx := newRealFixture(t)
	defer fx.close()
	// 让 outbox worker 不领取这些 effect：投递后 outbox 文档会被删除，effect ID 复用的
	// 身份裁决只在文档仍在时成立（见 RR-20260926-34 修复记录）。
	later := time.Now().Add(time.Hour).UnixNano()
	receipt := coredata.Receipt{Namespace: "saga-step", ID: "rr34-cmd-1", Digest: []byte("d1"), Payload: []byte("p1")}
	put := func(seed byte, id int64) coredata.CommitRecord {
		return realRecord(seed, []coredata.Mutation{realPut(t, fx.database, rr34Resource, id, 0, 1, bson.M{"level": int32(1)})})
	}
	first := put(0x31, 1)
	first.Receipts = []coredata.Receipt{receipt}
	first.Effects = []coredata.Effect{{ID: "rr34-effect-1", Topic: "hero.changed", Payload: []byte{1}, AvailableAt: later}}
	if err := fx.runtime.Store.Project(fx.context(), first); err != nil {
		t.Fatalf("first projection: %v", err)
	}

	t.Run("identical receipt from another transaction succeeds", func(t *testing.T) {
		record := put(0x32, 2)
		record.Receipts = []coredata.Receipt{receipt}
		got := projectThroughWAL(t, fx, record)
		if got.err != nil || got.fatal != 0 || !got.written {
			t.Fatalf("err=%v fatal=%d written=%v elapsed=%s, want success with the DAO written", got.err, got.fatal, got.written, got.elapsed)
		}
		if got.elapsed >= rr34Budget {
			t.Fatalf("elapsed=%s, want well under transaction_timeout=%s", got.elapsed, rr34TransactionTimeout)
		}
	})

	t.Run("receipt payload drift is a fatal receipt identity conflict", func(t *testing.T) {
		record := put(0x33, 3)
		record.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "rr34-cmd-1", Digest: []byte("d1"), Payload: []byte("other")}}
		got := projectThroughWAL(t, fx, record)
		if !errors.Is(got.err, engine.ErrReceiptIdentity) || got.fatal != 1 || got.written {
			t.Fatalf("err=%v fatal=%d written=%v elapsed=%s, want fatal ErrReceiptIdentity and a rolled-back DAO", got.err, got.fatal, got.written, got.elapsed)
		}
		if got.elapsed >= rr34Budget {
			t.Fatalf("elapsed=%s, want well under transaction_timeout=%s", got.elapsed, rr34TransactionTimeout)
		}
	})

	// saga start 的 effect ID 是命令内容哈希（saga/nest.go），同一 start 在两个事务各发一次
	// 就是这种形状：按现有设计是 fatal 身份冲突，修复保持不变。
	t.Run("effect id reused by another transaction is a fatal transaction identity conflict", func(t *testing.T) {
		record := put(0x34, 4)
		record.Effects = []coredata.Effect{{ID: "rr34-effect-1", Topic: "hero.changed", Payload: []byte{1}, AvailableAt: later}}
		got := projectThroughWAL(t, fx, record)
		if !errors.Is(got.err, engine.ErrTransactionIdentity) || got.fatal != 1 || got.written {
			t.Fatalf("err=%v fatal=%d written=%v elapsed=%s, want fatal ErrTransactionIdentity and a rolled-back DAO", got.err, got.fatal, got.written, got.elapsed)
		}
		if got.elapsed >= rr34Budget {
			t.Fatalf("elapsed=%s, want well under transaction_timeout=%s", got.elapsed, rr34TransactionTimeout)
		}
	})

	t.Run("replaying the committed record is absorbed by its marker", func(t *testing.T) {
		start := time.Now()
		if err := fx.runtime.Store.Project(fx.context(), first); err != nil {
			t.Fatalf("replay: %v", err)
		}
		if elapsed := time.Since(start); elapsed >= rr34Budget {
			t.Fatalf("replay elapsed=%s", elapsed)
		}
		assertCollectionCount(t, fx, engine.OutboxCollection, 1)
		assertCollectionCount(t, fx, engine.ReceiptCollection, 1)
	})

	t.Run("concurrent projection of one record converges on one marker", func(t *testing.T) {
		record := put(0x35, 5)
		record.Effects = []coredata.Effect{{ID: "rr34-effect-5", Topic: "hero.changed", AvailableAt: later}}
		record.Receipts = []coredata.Receipt{{Namespace: "saga-step", ID: "rr34-cmd-5", Digest: []byte("d5")}}
		errs := make([]error, 8)
		var wg sync.WaitGroup
		start := time.Now()
		for i := range errs {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				errs[i] = fx.runtime.Store.Project(fx.context(), record)
			}(i)
		}
		wg.Wait()
		elapsed := time.Since(start)
		for i, err := range errs {
			if err != nil {
				t.Fatalf("projection %d: %v", i, err)
			}
		}
		markers, err := fx.mongo.Database(fx.database).Collection(engine.TransactionCollection).CountDocuments(fx.context(), bson.M{"_id": record.ID.String()})
		if err != nil {
			t.Fatal(err)
		}
		if markers != 1 {
			t.Fatalf("markers=%d, want 1", markers)
		}
		if elapsed >= rr34TransactionTimeout {
			t.Fatalf("concurrent elapsed=%s reached transaction_timeout", elapsed)
		}
	})
}
