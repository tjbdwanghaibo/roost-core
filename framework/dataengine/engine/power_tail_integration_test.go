//go:build integration

package engine

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Model loss of complete unsynced WAL frames, not SIGKILL or a claim about the hardware.
// A projected batch may replay as a singleton. Its marker (and digest), not a greater
// document version by itself, proves that this transaction already ran.
func TestRealMongoShorterWALAfterProjectedBatch(t *testing.T) {
	for _, keep := range []int{0, 1, 2} {
		t.Run(fmt.Sprintf("survives-%d", keep), func(t *testing.T) {
			ctx, client, db := isolatedRealMongo(t)
			mkStore := func() *MongoStore {
				s, err := NewMongoStore(client, MongoStoreConfig{DefaultDatabase: db, Databases: identityDatabases(t)})
				if err != nil {
					t.Fatal(err)
				}
				return s
			}
			store := mkStore()
			if err := store.EnsureInfrastructure(ctx); err != nil {
				t.Fatal(err)
			}
			coll := client.Database(db).Collection("heroes")
			if _, err := coll.InsertOne(ctx, bson.M{"_id": int64(7), "_version": uint64(1), "v": 1}); err != nil {
				t.Fatal(err)
			}
			opts := nestwal.DefaultOptions(t.TempDir())
			opts.GroupCommitInterval = time.Hour
			open := func() (*nestwal.WAL, *Projector) {
				w, err := nestwal.Open(opts)
				if err != nil {
					t.Fatal(err)
				}
				p, err := NewProjector(w, mkStore(), ProjectorOptions{ManualReplay: true, CloseWAL: true, ProjectionWorkers: 1})
				if err != nil {
					t.Fatal(err)
				}
				return w, p
			}
			w, p := open()
			baselineSyncs := w.Stats().Syncs
			var records []coredata.CommitRecord
			offsets := []int64{0}
			var segment string
			for v := 1; v <= 2; v++ {
				raw, err := bson.Marshal(bson.M{"v": v + 1})
				if err != nil {
					t.Fatal(err)
				}
				r := coredata.CommitRecord{Handler: "power-tail", Durability: coredata.DurabilityAsync, Mutations: []coredata.Mutation{{Key: coredata.DocumentKey{Database: db, Resource: "heroes", ID: 7}, Kind: coredata.MutationPatch, ExpectedVersion: uint64(v), NextVersion: uint64(v + 1), Mask: 1, Schema: 1, Codec: "bson-v2", Patch: coredata.FieldPatch{SetBSON: raw}}}}
				r.ID[0] = byte(v)
				f, err := w.Append(ctx, r)
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, r)
				offsets = append(offsets, f.Offset)
				segment = filepath.Join(opts.Dir, fmt.Sprintf("segment-%020d.wal", f.Segment))
			}
			stop := errors.New("before WAL ack")
			p.OverrideAck(func(context.Context, corenest.CommitFence) error { return stop })
			if n, err := p.ReplayPass(ctx); n != 2 || !errors.Is(err, stop) {
				t.Fatalf("first projection %d %v", n, err)
			}
			markers, err := client.Database(db).Collection(TransactionCollection).CountDocuments(ctx, bson.M{})
			if err != nil || markers != 2 {
				t.Fatalf("markers=%d err=%v", markers, err)
			}
			if s := w.Stats(); s.Syncs != baselineSyncs || s.Acknowledged != 0 {
				t.Fatalf("expected projected but unsynced/unacked records: %+v", s)
			}
			if err := p.Close(ctx); err != nil {
				t.Fatal(err)
			}
			if err := os.Truncate(segment, offsets[keep]); err != nil {
				t.Fatal(err)
			}
			_, recovered := open()
			defer recovered.Close(ctx)
			n, err := recovered.ReplayPass(ctx)
			if err != nil || n != keep {
				t.Fatalf("surviving prefix=%d replayed=%d error=%v", keep, n, err)
			}
			// Use the same final drain as Runtime.Start; it must not be fenced by a false conflict.
			if err := (&Runtime{Projector: recovered}).drainAtStartup(ctx); err != nil {
				t.Fatal(err)
			}
			var hero struct {
				Version uint64 `bson:"_version"`
				V       int    `bson:"v"`
			}
			if err := coll.FindOne(ctx, bson.M{"_id": int64(7)}, &hero); err != nil || hero.Version != 3 || hero.V != 3 {
				t.Fatalf("after recovery %+v %v", hero, err)
			}
			t.Logf("survived=%d replayed=%d Mongo v=%d marker count=%d", keep, n, hero.Version, markers)
			unknown := records[0]
			unknown.ID[1] = 77
			if err := store.Project(ctx, unknown); !errors.Is(err, ErrProjectionConflict) {
				t.Fatalf("unrelated stale transaction accepted: %v", err)
			}
			changed := records[0]
			changed.Handler = "different"
			if err := store.Project(ctx, changed); !errors.Is(err, ErrTransactionIdentity) {
				t.Fatalf("digest mismatch: %v", err)
			}
		})
	}
}
