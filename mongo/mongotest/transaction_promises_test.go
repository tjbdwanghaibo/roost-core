// RR-20261004-NC-29：私有写、快照读、冲突重试与跨集合发布；abort 不撤回其他写者。
package mongotest

import (
	"context"
	"errors"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"testing"
)

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
