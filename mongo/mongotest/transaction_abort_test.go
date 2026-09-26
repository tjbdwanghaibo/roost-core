package mongotest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20260926-34：真实服务端在事务内任一写错误后中止事务，之后同一事务的语句返回
// NoSuchTransaction（TransientTransactionError），驱动据此重跑整个回调。伪 Mongo 若在撞键
// 后继续服务读，“事务内撞键读回裁决”这类缺陷在单测里就永远是绿的。
// 对应的真实服务端行为由 mongo/driver 的 integration 测试
// TestRealMongoWriteErrorAbortsTransactionAndKeepsDriverChain 记录。
func TestTransactionWriteErrorAbortsLikeTheServer(t *testing.T) {
	client := NewClient()
	coll := client.Collection("game", "receipts")
	if err := coll.Seed(bson.M{"_id": "r1", "digest": "d1"}); err != nil {
		t.Fatal(err)
	}
	session, err := client.StartSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(context.Background())
	var insertErr, findErr, laterWriteErr error
	calls := 0
	err = session.WithTransaction(context.Background(), func(ctx context.Context) error {
		calls++
		_, insertErr = coll.InsertOne(ctx, bson.M{"_id": "r1", "digest": "d1"})
		var found bson.M
		findErr = coll.FindOne(ctx, bson.M{"_id": "r1"}, &found)
		_, laterWriteErr = coll.InsertOne(ctx, bson.M{"_id": "r2"})
		return findErr
	})
	if !errors.Is(insertErr, fmongo.ErrDuplicateKey) || hasErrorLabel(insertErr, TransientTransactionError) {
		t.Fatalf("insert=%v, want a plain duplicate key", insertErr)
	}
	for name, got := range map[string]error{"find": findErr, "later write": laterWriteErr} {
		if !errors.Is(got, ErrNoSuchTransaction) || !hasErrorLabel(got, TransientTransactionError) {
			t.Fatalf("%s after the write error=%v, want transient NoSuchTransaction", name, got)
		}
	}
	// The driver keeps re-running a transient callback; the fake bounds it.
	if calls != transientAttemptLimit || client.Attempts() != transientAttemptLimit {
		t.Fatalf("calls=%d attempts=%d, want %d", calls, client.Attempts(), transientAttemptLimit)
	}
	if !errors.Is(err, ErrNoSuchTransaction) || !hasErrorLabel(err, TransientTransactionError) {
		t.Fatalf("final err=%v, want the last transient error", err)
	}
	if _, ok := coll.Lookup("r2"); ok || coll.Len() != 1 {
		t.Fatalf("aborted transaction left writes behind: %v", coll.Documents())
	}
	// Like the driver, the retry decision reads the first labelled error in the
	// chain: a duplicate key joined in front of the transient error is returned
	// at once.
	calls = 0
	err = session.WithTransaction(context.Background(), func(ctx context.Context) error {
		calls++
		_, dupErr := coll.InsertOne(ctx, bson.M{"_id": "r1"})
		var found bson.M
		return errors.Join(dupErr, coll.FindOne(ctx, bson.M{"_id": "r1"}, &found))
	})
	if calls != 1 || !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("calls=%d err=%v, want the joined duplicate key without a retry", calls, err)
	}
	// Outside a transaction the same statements are independent.
	if _, err := coll.InsertOne(context.Background(), bson.M{"_id": "r1"}); !errors.Is(err, fmongo.ErrDuplicateKey) {
		t.Fatalf("plain insert=%v", err)
	}
	var found bson.M
	if err := coll.FindOne(context.Background(), bson.M{"_id": "r1"}, &found); err != nil {
		t.Fatalf("plain find after a plain duplicate key=%v", err)
	}
}

// 回调吞掉写错误并返回 nil 时，服务端拒绝提交已中止的事务（NoSuchTransaction，transient），
// 驱动重跑回调；下一次尝试从事务前的状态开始。
func TestTransactionCommitAfterSwallowedWriteErrorIsRetried(t *testing.T) {
	client := NewClient()
	coll := client.Collection("game", "markers")
	if err := coll.Seed(bson.M{"_id": "m1"}); err != nil {
		t.Fatal(err)
	}
	session, err := client.StartSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(context.Background())
	calls := 0
	err = session.WithTransaction(context.Background(), func(ctx context.Context) error {
		calls++
		if _, err := coll.InsertOne(ctx, bson.M{"_id": fmt.Sprintf("attempt-%d", calls)}); err != nil {
			return err
		}
		if calls == 1 {
			_, _ = coll.InsertOne(ctx, bson.M{"_id": "m1"}) // swallowed duplicate key
		}
		return nil
	})
	if err != nil || calls != 2 {
		t.Fatalf("err=%v calls=%d, want a retried commit that succeeds", err, calls)
	}
	if _, ok := coll.Lookup("attempt-1"); ok {
		t.Fatal("the aborted attempt's write survived")
	}
	if _, ok := coll.Lookup("attempt-2"); !ok {
		t.Fatal("the retried attempt did not commit")
	}
}

// “没有匹配的文档”不是服务端错误，不中止事务；非 transient 的回调错误不重试。
func TestTransactionNotFoundDoesNotAbortAndPlainErrorsAreNotRetried(t *testing.T) {
	client := NewClient()
	coll := client.Collection("game", "heroes")
	session, err := client.StartSession(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer session.EndSession(context.Background())
	boom := errors.New("business refusal")
	calls := 0
	err = session.WithTransaction(context.Background(), func(ctx context.Context) error {
		calls++
		var doc bson.M
		if err := coll.FindOneAndUpdate(ctx, bson.M{"_id": int64(1)}, bson.M{"$set": bson.M{"v": 1}}, &doc); !errors.Is(err, fmongo.ErrNotFound) {
			return fmt.Errorf("miss=%v", err)
		}
		if _, err := coll.InsertOne(ctx, bson.M{"_id": int64(1)}); err != nil {
			return fmt.Errorf("insert after a miss=%w", err)
		}
		return boom
	})
	if !errors.Is(err, boom) || calls != 1 {
		t.Fatalf("err=%v calls=%d, want the plain error once", err, calls)
	}
	if coll.Len() != 0 {
		t.Fatal("aborted transaction kept its insert")
	}
}
