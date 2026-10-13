// 替身事务：写错误后像服务端一样中止、吞掉错误的提交被重跑、私有写与快照读、跨集合发布。

package mongotest

import (
	"context"
	"errors"
	"fmt"
	"testing"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 真实服务端在事务内任一写错误后中止事务，之后同一事务的语句返回 NoSuchTransaction（TransientTransactionError），驱动据此重跑整个回调；
// 替身同样中止，“事务内撞键读回裁决”这类缺陷在单测里才会变红。真实服务端行为由 mongo/driver 的 integration 测试
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

// 私有写、快照读、冲突重试与跨集合发布；abort 不撤回其他写者。历史：RR-20261004-NC-29。
func TestTransactionSnapshotPromises(t *testing.T) {
	ctx := context.Background()
	t.Run("private_reads_outside_write_survives_abort", func(t *testing.T) {
		client := NewClient()
		c := client.Collection("tx", "c")
		s, _ := client.StartSession(ctx)
		if err := c.Seed(bson.M{"_id": 1, "n": 1}); err != nil {
			t.Fatal(err)
		}
		abort := errors.New("abort")
		err := s.WithTransaction(ctx, func(tx context.Context) error {
			if _, err := c.UpdateOne(tx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"n": 9}}); err != nil {
				return err
			}
			var own struct {
				N int `bson:"n"`
			}
			if err := c.FindOne(tx, bson.M{"_id": 1}, &own); err != nil || own.N != 9 {
				t.Fatalf("own=%v err=%v", own, err)
			}
			if got := boundaryStoredNumber(t, c); got != 1 {
				t.Fatalf("uncommitted visible=%d", got)
			}
			if _, err := c.InsertOne(ctx, bson.M{"_id": 2}); err != nil {
				return err
			}
			count, err := c.CountDocuments(tx, bson.M{})
			if err != nil || count != 1 {
				t.Fatalf("snapshot count=%d err=%v", count, err)
			}
			return abort
		})
		if !errors.Is(err, abort) {
			t.Fatal(err)
		}
		count, err := c.CountDocuments(ctx, bson.M{})
		if err != nil || count != 2 || boundaryStoredNumber(t, c) != 1 {
			t.Fatalf("outside count=%d err=%v", count, err)
		}
	})
	for _, exhaust := range []bool{false, true} {
		name := "conflict_retry_atomic_publish"
		if exhaust {
			name = "conflict_budget_keeps_outside_write"
		}
		t.Run(name, func(t *testing.T) {
			client := NewClient()
			a := client.Collection("a", "same")
			z := client.Collection("z", "same")
			for _, c := range []*Collection{a, z} {
				if err := c.Seed(bson.M{"_id": 1, "n": 1}); err != nil {
					t.Fatal(err)
				}
			}
			s, _ := client.StartSession(ctx)
			attempts := 0
			err := s.WithTransaction(ctx, func(tx context.Context) error {
				attempts++
				for _, c := range []*Collection{z, a} {
					if _, err := c.UpdateOne(tx, bson.M{"_id": 1}, bson.M{"$inc": bson.M{"n": 1}}); err != nil {
						return err
					}
				}
				if got := boundaryStoredNumber(t, z); got != 1 {
					t.Fatalf("partial publish=%d", got)
				}
				if attempts == 1 || exhaust {
					_, err := a.UpdateOne(ctx, bson.M{"_id": 1}, bson.M{"$set": bson.M{"n": 40}})
					return err
				}
				return nil
			})
			if exhaust {
				if !errors.Is(err, ErrTransactionConflict) || attempts != transientAttemptLimit || boundaryStoredNumber(t, a) != 40 || boundaryStoredNumber(t, z) != 1 {
					t.Fatalf("attempts=%d err=%v", attempts, err)
				}
			} else if err != nil || attempts != 2 || boundaryStoredNumber(t, a) != 41 || boundaryStoredNumber(t, z) != 2 {
				t.Fatalf("attempts=%d err=%v", attempts, err)
			}
		})
	}
	t.Run("disjoint_commit_survives", func(t *testing.T) {
		client := NewClient()
		a := client.Collection("tx", "a")
		b := client.Collection("tx", "b")
		s, _ := client.StartSession(ctx)
		other, _ := client.StartSession(ctx)
		err := s.WithTransaction(ctx, func(tx context.Context) error {
			if _, err := a.InsertOne(tx, bson.M{"_id": 1}); err != nil {
				return err
			}
			return other.WithTransaction(ctx, func(bt context.Context) error { _, err := b.InsertOne(bt, bson.M{"_id": 2}); return err })
		})
		if err != nil {
			t.Fatal(err)
		}
		for _, c := range []*Collection{a, b} {
			n, err := c.CountDocuments(ctx, bson.M{})
			if err != nil || n != 1 {
				t.Fatalf("n=%d err=%v", n, err)
			}
		}
	})
	for _, panicCallback := range []bool{false, true} {
		name := "cancel_discards_and_closes_context"
		if panicCallback {
			name = "panic_discards_and_closes_context"
		}
		t.Run(name, func(t *testing.T) {
			client := NewClient()
			c := client.Collection("tx", "finished")
			s, _ := client.StartSession(ctx)
			var escaped context.Context
			canceled, cancel := context.WithCancel(ctx)
			defer cancel()
			var err error
			func() {
				defer func() {
					if p := recover(); panicCallback && p != "boom" {
						t.Fatalf("panic=%v", p)
					}
				}()
				err = s.WithTransaction(canceled, func(tx context.Context) error {
					escaped = tx
					_, e := c.InsertOne(tx, bson.M{"_id": 1})
					if panicCallback {
						panic("boom")
					}
					cancel()
					return e
				})
			}()
			if !panicCallback && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
			if _, err := c.InsertOne(escaped, bson.M{"_id": 2}); !errors.Is(err, ErrNoSuchTransaction) {
				t.Fatal(err)
			}
			n, err := c.CountDocuments(ctx, bson.M{})
			if err != nil || n != 0 {
				t.Fatalf("n=%d err=%v", n, err)
			}
		})
	}
	t.Run("foreign_client_rejected", func(t *testing.T) {
		client := NewClient()
		foreign := NewClient().Collection("tx", "other")
		s, _ := client.StartSession(ctx)
		err := s.WithTransaction(ctx, func(tx context.Context) error { _, err := foreign.InsertOne(tx, bson.M{"_id": 1}); return err })
		if !errors.Is(err, ErrUnsupported) {
			t.Fatal(err)
		}
		n, _ := foreign.CountDocuments(ctx, bson.M{})
		if n != 0 {
			t.Fatal(n)
		}
	})
	t.Run("transaction_index_explicitly_unsupported", func(t *testing.T) {
		client := NewClient()
		c := client.Collection("tx", "index")
		s, _ := client.StartSession(ctx)
		err := s.WithTransaction(ctx, func(tx context.Context) error {
			return c.EnsureIndexes(tx, []fmongo.IndexModel{{Keys: bson.D{{Key: "n", Value: 1}}}})
		})
		if !errors.Is(err, ErrUnsupported) || c.HasIndex("n") {
			t.Fatalf("err=%v has=%v", err, c.HasIndex("n"))
		}
	})
}
