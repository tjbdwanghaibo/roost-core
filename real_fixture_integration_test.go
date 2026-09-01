//go:build integration

package dataengine

import (
	"context"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/cube-core/app"
	coredata "github.com/tjbdwanghaibo/cube-core/dataengine"
	"github.com/tjbdwanghaibo/cube-core/entity"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	fnats "github.com/tjbdwanghaibo/cube-core/nats"
	corenest "github.com/tjbdwanghaibo/cube-core/nest"
	"github.com/tjbdwanghaibo/cube-kit/mods"
	kitmongo "github.com/tjbdwanghaibo/cube-kit/mongo"
	kitnats "github.com/tjbdwanghaibo/cube-kit/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

const realIntegrationTimeout = 30 * time.Second

var realFixtureSequence atomic.Uint64

type realFixture struct {
	ctx       context.Context
	cancel    context.CancelFunc
	database  string
	stream    string
	effectSub string
	mongo     fmongo.IMongo
	jetStream fnats.IJetStream
	runtime   *Runtime
	mongoMod  *kitmongo.MongoMod
	natsMod   *kitnats.NatsMod
	dataMod   *Mod
	closeOnce sync.Once
}

func newRealFixture(t *testing.T) *realFixture {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 or use scripts/integration/dataengine-env.sh test")
	}
	mongoURI := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if mongoURI == "" || natsURL == "" {
		t.Fatal("isolated integration environment variables are incomplete")
	}

	id := realFixtureSequence.Add(1)
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), id)
	ctx, cancel := context.WithTimeout(context.Background(), realIntegrationTimeout)
	fx := &realFixture{
		ctx: ctx, cancel: cancel,
		database:  fmt.Sprintf("roost_it_%s", suffix),
		stream:    fmt.Sprintf("ROOST_IT_EFFECTS_%s", suffix),
		effectSub: fmt.Sprintf("roost.it.%s", suffix),
		mongoMod:  kitmongo.NewMongoMod(),
		natsMod:   kitnats.NewNatsMod(nil),
	}
	t.Cleanup(fx.close)

	cfg := viper.New()
	cfg.Set("persistence.engine", "dataengine")
	cfg.Set("sid", int32(901))
	cfg.Set("server_type", "dataengine-it")
	cfg.Set("mongo.uri", mongoURI)
	cfg.Set("mongo.require_replica_set", true)
	cfg.Set("mongo.connect_timeout", 5*time.Second)
	cfg.Set("mongo.transaction_timeout", 15*time.Second)
	cfg.Set("nats.url", natsURL)
	cfg.Set("dataengine.database", fx.database)
	cfg.Set("dataengine.wal.writer_version", 2)
	cfg.Set("dataengine.wal.dir", t.TempDir())
	cfg.Set("dataengine.effects.stream", fx.stream)
	cfg.Set("dataengine.effects.subject_prefix", fx.effectSub)
	cfg.Set("dataengine.effects.replicas", 3)
	cfg.Set("dataengine.effects.max_bytes", int64(64<<20))
	cfg.Set("dataengine.outbox.poll_interval", 50*time.Millisecond)
	cfg.Set("dataengine.outbox.retry_min", 100*time.Millisecond)
	cfg.Set("dataengine.outbox.retry_max", time.Second)
	cfg.Set("dataengine.startup_timeout", realIntegrationTimeout)
	cfg.Set("dataengine.shutdown_timeout", realIntegrationTimeout)

	registry := app.NewRegistry(cfg)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	fx.dataMod = NewMod(WithEntityAccess(access))

	for name, initialize := range map[string]func(*viper.Viper) error{
		"mongo": fx.mongoMod.Init,
		"nats":  fx.natsMod.Init,
		"data":  fx.dataMod.Init,
	} {
		if err := initialize(cfg); err != nil {
			t.Fatalf("initialize %s mod: %v", name, err)
		}
	}
	if err := fx.mongoMod.Provide(registry); err != nil {
		t.Fatalf("provide mongo mod: %v", err)
	}
	if err := fx.mongoMod.Start(); err != nil {
		t.Fatalf("start mongo mod: %v", err)
	}
	fx.mongo = fx.mongoMod.Client()
	if err := fx.natsMod.Provide(registry); err != nil {
		t.Fatalf("provide nats mod: %v", err)
	}
	var ok bool
	fx.jetStream, ok = app.Lookup[fnats.IJetStream](registry, mods.ModNatsJetStream)
	if !ok || fx.jetStream == nil {
		t.Fatal("NATS mod did not provide JetStream")
	}
	if err := fx.natsMod.Start(); err != nil {
		t.Fatalf("start nats mod: %v", err)
	}
	if err := fx.dataMod.Provide(registry); err != nil {
		t.Fatalf("provide data engine mod: %v", err)
	}
	if err := fx.dataMod.Start(); err != nil {
		t.Fatalf("start data engine mod: %v", err)
	}
	fx.runtime = fx.dataMod.Runtime()
	if fx.runtime == nil || !fx.runtime.Ready() {
		t.Fatal("data engine runtime did not become ready")
	}
	return fx
}

func (fx *realFixture) context() context.Context {
	return fx.ctx
}

func (fx *realFixture) close() {
	if fx == nil {
		return
	}
	fx.closeOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), realIntegrationTimeout)
		defer cancel()
		if fx.dataMod != nil {
			_ = fx.dataMod.StopWithContext(ctx)
		}
		if fx.mongo != nil && fx.database != "" {
			_ = fx.mongo.Database(fx.database).Drop(ctx)
		}
		if fx.natsMod != nil {
			_ = fx.natsMod.StopWithContext(ctx)
		}
		if fx.mongoMod != nil {
			_ = fx.mongoMod.StopWithContext(ctx)
		}
		if fx.cancel != nil {
			fx.cancel()
		}
	})
}

func realRecord(seed byte, mutations []coredata.Mutation) coredata.CommitRecord {
	var id coredata.TransactionID
	id[len(id)-1] = seed
	return coredata.CommitRecord{
		ID: id, Handler: "real-integration", CreatedAt: time.Now().UnixNano(),
		Durability: corenest.DurabilityStrict, Mutations: mutations,
	}
}

func realPut(t *testing.T, database, resource string, id int64, expected, next uint64, fields bson.M) coredata.Mutation {
	t.Helper()
	return realPutWithSchema(t, database, resource, id, expected, next, 1, fields)
}

func realPutWithSchema(t *testing.T, database, resource string, id int64, expected, next uint64, schema uint32, fields bson.M) coredata.Mutation {
	t.Helper()
	doc := bson.M{"_id": id}
	for key, value := range fields {
		doc[key] = value
	}
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return coredata.Mutation{
		Key:  coredata.DocumentKey{Database: database, Resource: resource, ID: id},
		Kind: coredata.MutationPut, ExpectedVersion: expected, NextVersion: next,
		Mask: coredata.AllFields, Schema: schema, Codec: "bson-v2", Data: raw,
	}
}

func findDocument(t *testing.T, fx *realFixture, resource string, id int64) bson.M {
	t.Helper()
	var doc bson.M
	if err := fx.mongo.Database(fx.database).Collection(resource).FindOne(fx.context(), bson.M{"_id": id}, &doc); err != nil {
		t.Fatalf("find %s/%d: %v", resource, id, err)
	}
	return doc
}

func documentVersion(t *testing.T, doc bson.M) uint64 {
	t.Helper()
	switch version := doc["_version"].(type) {
	case int32:
		return uint64(version)
	case int64:
		return uint64(version)
	case uint32:
		return uint64(version)
	case uint64:
		return version
	case int:
		return uint64(version)
	default:
		t.Fatalf("unexpected document version %#v (%T)", doc["_version"], doc["_version"])
		return 0
	}
}

func assertDocumentVersion(t *testing.T, fx *realFixture, resource string, id int64, want uint64) {
	t.Helper()
	if got := documentVersion(t, findDocument(t, fx, resource, id)); got != want {
		t.Fatalf("%s/%d version=%d, want %d", resource, id, got, want)
	}
}

func assertCollectionCount(t *testing.T, fx *realFixture, resource string, want int64) {
	t.Helper()
	got, err := fx.mongo.Database(fx.database).Collection(resource).CountDocuments(fx.context(), bson.M{})
	if err != nil {
		t.Fatalf("count %s: %v", resource, err)
	}
	if got != want {
		t.Fatalf("%s count=%d, want %d", resource, got, want)
	}
}
