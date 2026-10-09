package saga

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RR-20261005-NC-39：规范化不改变合法重复；旧 BSON 缺字段可读，但不能从推进后状态猜原意图。
func TestStartIdentityCompatibilityAndForeignIntents(t *testing.T) {
	for _, name := range []string{"canonical", "explicit_auto_id", "data", "deadline", "version", "id", "legacy_pristine", "legacy_progressed"} {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			client := mongotest.NewClient()
			store, err := NewMongoStore(client, MongoStoreOptions{Database: "identity_compat"})
			if err != nil {
				t.Fatal(err)
			}
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			e, err := NewEngine(store, PublishFunc(func(context.Context, Command) error { return nil }), DefaultOptions())
			if err != nil {
				t.Fatal(err)
			}
			d := testDefinition()
			if err := e.Register(d); err != nil {
				t.Fatal(err)
			}
			d.Version = 2
			if err := e.Register(d); err != nil {
				t.Fatal(err)
			}
			deadline := time.Now().UTC().Add(time.Hour).Truncate(time.Millisecond)
			request := StartRequest{Type: "rally", DefinitionVersion: 1, BusinessKey: "compat", DeadlineAt: deadline}
			if name != "explicit_auto_id" {
				request.ID = "compat"
			}
			r, err := e.StartSaga(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			if len(r.StartDigest) != 64 {
				t.Fatalf("no original digest: %q", r.StartDigest)
			}
			var raw bson.M
			if err := store.sagas().FindOne(ctx, bson.M{"_id": r.ID}, &raw); err != nil {
				t.Fatal(err)
			}
			if raw["start_digest"] != r.StartDigest {
				t.Fatalf("digest not in BSON: %v", raw["start_digest"])
			}
			if name == "legacy_pristine" || name == "legacy_progressed" {
				delete(raw, "start_digest")
				if name == "legacy_progressed" {
					raw["version"] = int64(2)
					raw["completed_steps"] = int32(1)
					raw["step"] = int32(1)
				}
				if _, err := store.sagas().ReplaceOne(ctx, bson.M{"_id": r.ID}, raw); err != nil {
					t.Fatal(err)
				}
				old, err := store.Get(ctx, r.ID)
				if err != nil || old.StartDigest != "" {
					t.Fatalf("legacy decode=%+v err=%v", old, err)
				}
			}
			wantConflict := false
			switch name {
			case "canonical":
				request.ID = " compat "
				request.BusinessKey = " compat "
				request.Data = []byte{}
				request.Now = time.Now().Add(time.Minute)
				request.DeadlineAt = deadline.In(time.FixedZone("test", 8*60*60)).Add(400 * time.Microsecond)
			case "explicit_auto_id":
				request.ID = r.ID
			case "data":
				request.Data = []byte("different")
				wantConflict = true
			case "deadline":
				request.DeadlineAt = deadline.Add(time.Second)
				wantConflict = true
			case "version":
				request.DefinitionVersion = 2
				wantConflict = true
			case "id":
				request.ID = "other"
				wantConflict = true
			case "legacy_progressed":
				wantConflict = true
			}
			after, err := e.StartSaga(ctx, request)
			if wantConflict {
				if !errors.Is(err, ErrIdentityConflict) {
					t.Fatalf("foreign/unverifiable intent accepted: %v", err)
				}
			} else if err != nil || after.ID != r.ID {
				t.Fatalf("legal duplicate=%+v err=%v", after, err)
			}
			listed, err := store.List(ctx, Query{Limit: 10})
			if err != nil || len(listed) != 1 {
				t.Fatalf("duplicate created another saga: %+v err=%v", listed, err)
			}
			if name != "legacy_pristine" && name != "legacy_progressed" && listed[0].StartDigest != r.StartDigest {
				t.Fatal("List lost original digest")
			}
		})
	}
}
