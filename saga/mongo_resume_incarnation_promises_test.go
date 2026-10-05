package saga

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20261005-NC-38：Resume 代际必须跨正式 MongoStore 持久化、重载和重新派发。
// 只在内存 Store 检查 CommandID 会漏掉 BSON 转换丢字段导致的旧回执冲突。
func TestMongoResumePersistsGenerationAndAcceptsFreshCompletion(t *testing.T) {
	for _, phase := range []Phase{PhaseForward, PhaseCompensate} {
		t.Run(phase.String(), func(t *testing.T) {
			ctx := context.Background()
			client := mongotest.NewClient()
			store, err := NewMongoStore(client, MongoStoreOptions{Database: "resume"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			newEngine := func() *Engine {
				e, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
				if err != nil {
					t.Fatal(err)
				}
				if err := e.Register(testDefinition()); err != nil {
					t.Fatal(err)
				}
				return e
			}
			e := newEngine()
			now := time.Now().UTC().Truncate(time.Millisecond)
			r := Record{ID: "resume", Type: "rally", DefinitionVersion: 1, BusinessKey: "resume", Status: StatusPending, Phase: phase, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if phase == PhaseCompensate {
				r.Status = StatusCompensating
				r.CompletedSteps = 1
			}
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			var oldIDs []string
			for generation := uint32(0); generation < 3; generation++ {
				// 新协调器/新 Store 从持久来源恢复；不使用 Resume 返回的内存对象派发。
				store, err = NewMongoStore(client, MongoStoreOptions{Database: "resume"})
				if err != nil {
					t.Fatal(err)
				}
				e = newEngine()
				before, err := store.Get(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				if before.Incarnation != generation {
					t.Errorf("persisted generation=%d, want %d", before.Incarnation, generation)
				}
				if err := e.processClaimed(ctx, before, now); err != nil {
					t.Fatal(err)
				}
				waiting, err := store.Get(ctx, r.ID)
				if err != nil {
					t.Fatal(err)
				}
				for _, old := range oldIDs {
					if waiting.CommandID == old {
						t.Errorf("redispatch reused old command ID %q at generation %d", old, generation)
					}
				}
				oldIDs = append(oldIDs, waiting.CommandID)
				items, err := store.ClaimOutbox(ctx, ClaimRequest{Owner: "review", Now: now, LeaseDuration: time.Minute, Limit: 1})
				if err != nil {
					t.Fatal(err)
				}
				if len(items) != 1 || items[0].Command.ID != waiting.CommandID {
					t.Fatalf("outbox did not carry current command: %+v", items)
				}
				result := Completion{CommandID: waiting.CommandID, IdempotencyKey: waiting.OperationKey, SagaID: r.ID}
				if generation == 2 {
					result.Success = true
				} else {
					result.Error = "repair needed"
				}
				after, err := e.Complete(ctx, result)
				if err != nil {
					t.Fatalf("generation %d fresh completion failed: %v", generation, err)
				}
				if generation == 2 {
					want := StatusPending
					if phase == PhaseCompensate {
						want = StatusCompensated
					}
					if after.Status != want {
						t.Fatalf("completion did not advance: %+v", after)
					}
					if _, err := e.Complete(ctx, result); err != nil {
						t.Fatalf("same receipt redelivery: %v", err)
					}
					break
				}
				want := StatusFailed
				if phase == PhaseCompensate {
					want = StatusManualRequired
				}
				if after.Status != want {
					t.Fatalf("failure did not enter repair state: %+v", after)
				}
				resumed, err := e.Resume(ctx, ResumeRequest{ID: r.ID, Now: now})
				if err != nil {
					t.Fatal(err)
				}
				if resumed.Incarnation != generation+1 {
					t.Errorf("Resume generation=%d, want %d", resumed.Incarnation, generation+1)
				}
			}
		})
	}
}

func TestMongoIncarnationSurvivesEveryRecordReadAndReplace(t *testing.T) {
	for _, generation := range []uint32{0, 1, 17, ^uint32(0)} {
		t.Run(fmt.Sprint(generation), func(t *testing.T) {
			ctx := context.Background()
			store, err := NewMongoStore(mongotest.NewClient(), MongoStoreOptions{Database: "roundtrip"})
			if err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC().Truncate(time.Millisecond)
			r := Record{ID: "compat", Type: "rally", DefinitionVersion: 1, BusinessKey: "compat", Status: StatusPending, Phase: PhaseForward, Incarnation: generation, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
			if err := store.Create(ctx, r); err != nil {
				t.Fatal(err)
			}
			var persisted bson.M
			if err := store.sagas().FindOne(ctx, bson.M{"_id": r.ID}, &persisted); err != nil {
				t.Fatal(err)
			}
			if generation == 0 {
				if _, ok := persisted["incarnation"]; ok {
					t.Fatal("legacy generation zero should keep historical BSON shape")
				}
			}
			for _, read := range []func() (Record, error){func() (Record, error) { return store.Get(ctx, r.ID) }, func() (Record, error) { return store.GetByBusinessKey(ctx, r.Type, r.BusinessKey) }} {
				got, err := read()
				if err != nil {
					t.Fatal(err)
				}
				if got.Incarnation != generation {
					t.Errorf("record read lost generation: got %d want %d", got.Incarnation, generation)
				}
			}
			rows, err := store.List(ctx, Query{Limit: 1})
			if err != nil {
				t.Fatal(err)
			}
			if len(rows) != 1 || rows[0].Incarnation != generation {
				t.Fatalf("List lost generation: %+v", rows)
			}
			r.Version++
			if _, err := store.Apply(ctx, ApplyRequest{ExpectedVersion: 1, After: r}); err != nil {
				t.Fatal(err)
			}
			got, err := store.Get(ctx, r.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.Incarnation != generation || got.Version != 2 {
				t.Fatalf("replacement lost generation: %+v", got)
			}
		})
	}
}
