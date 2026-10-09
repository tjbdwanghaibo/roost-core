package saga

import (
	"context"
	"errors"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// RR-20261005-NC-41：候选扫描不授予发布权；另一个发布者 Nack 延后消息之后，
// 原扫描者必须在原子领取处复查 NextAttemptAt，不能绕过退避。正式 MongoStore，
// 后端为 mongotest；钩子在 Find 返回后同步执行另一个 Store，确定交错而不靠 sleep。
type outboxScanClient struct {
	fmongo.IMongo
	afterScan func()
}

func (c *outboxScanClient) Database(name string) fmongo.IDatabase {
	return outboxScanDatabase{IDatabase: c.IMongo.Database(name), client: c}
}

type outboxScanDatabase struct {
	fmongo.IDatabase
	client *outboxScanClient
}

func (d outboxScanDatabase) Collection(name string) fmongo.ICollection {
	return outboxScanCollection{ICollection: d.IDatabase.Collection(name), client: d.client}
}

type outboxScanCollection struct {
	fmongo.ICollection
	client *outboxScanClient
}

func (c outboxScanCollection) Find(ctx context.Context, filter, result any, options ...fmongo.FindOption) error {
	err := c.ICollection.Find(ctx, filter, result, options...)
	if _, ok := result.(*[]outboxDoc); ok && err == nil && c.client.afterScan != nil {
		hook := c.client.afterScan
		c.client.afterScan = nil
		hook()
	}
	return err
}

func TestOutboxClaimRechecksDueAfterAnotherPublisher(t *testing.T) {
	for _, mode := range []string{"future_nack", "still_leased", "due_nack"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			backend := mongotest.NewClient()
			client := &outboxScanClient{IMongo: backend}
			opts := MongoStoreOptions{Database: "outbox_due_review"}
			stale, err := NewMongoStore(client, opts)
			if err != nil {
				t.Fatal(err)
			}
			fresh, err := NewMongoStore(backend, opts)
			if err != nil {
				t.Fatal(err)
			}
			if err := fresh.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(fresh, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			if err := e.Register(testDefinition()); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			r, err := e.StartSaga(ctx, StartRequest{ID: "outbox-due", Type: "rally", DefinitionVersion: 1, BusinessKey: "outbox-due", Now: now})
			if err != nil {
				t.Fatal(err)
			}
			if err := e.processClaimed(ctx, r, now); err != nil {
				t.Fatal(err)
			}
			query := ClaimRequest{Owner: "stale", Now: now, LeaseDuration: time.Minute, Limit: 1}
			var claimed OutboxRecord
			fired := false
			client.afterScan = func() {
				fired = true
				other := query
				other.Owner = "fresh"
				items, err := fresh.ClaimOutbox(ctx, other)
				if err != nil || len(items) != 1 {
					t.Fatalf("fresh claim: %v %v", items, err)
				}
				claimed = items[0]
				if mode != "still_leased" {
					next := now
					if mode == "future_nack" {
						next = now.Add(time.Hour)
					}
					if err := fresh.NackOutbox(ctx, claimed.Command.ID, claimed.Lease, next, "publish failed"); err != nil {
						t.Fatal(err)
					}
				}
			}
			items, err := stale.ClaimOutbox(ctx, query)
			if !fired {
				t.Fatal("scan hook did not execute")
			}
			if err != nil {
				t.Fatal(err)
			}
			if mode == "due_nack" {
				if len(items) != 1 {
					t.Fatalf("due retry was refused: %v", items)
				}
				return
			}
			if len(items) != 0 {
				t.Fatalf("stale scan claimed unavailable retry (%s): next=%v now=%v", mode, items[0].NextAttemptAt, now)
			}
			if mode == "still_leased" {
				query.Now = now.Add(time.Minute)
			} else {
				query.Now = now.Add(time.Hour)
			}
			query.Owner = "recovery"
			items, err = fresh.ClaimOutbox(ctx, query)
			if err != nil || len(items) != 1 || items[0].Lease.Token <= claimed.Lease.Token {
				t.Fatalf("due recovery: %v %v", items, err)
			}
			if err := fresh.AckOutbox(ctx, claimed.Command.ID, claimed.Lease); !errors.Is(err, ErrConflict) {
				t.Fatalf("old ack passed fence: %v", err)
			}
			if err := fresh.NackOutbox(ctx, claimed.Command.ID, claimed.Lease, query.Now, "old"); !errors.Is(err, ErrConflict) {
				t.Fatalf("old nack passed fence: %v", err)
			}
			if err := fresh.AckOutbox(ctx, items[0].Command.ID, items[0].Lease); err != nil {
				t.Fatal(err)
			}
			if got, err := fresh.ClaimOutbox(ctx, query); err != nil || len(got) != 0 {
				t.Fatalf("outbox did not drain: %v %v", got, err)
			}
		})
	}
}
