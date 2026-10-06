//go:build integration

// RR-20261006-10 的真实依赖验收（APP-7，真实进程演练第 4 项，docs/bugfix/REAL-PROCESS-DRILLS-2026-10-06.md）：
// 修复时 kit MongoMod 的 Close 口径只用不可达地址验证过。这里经正式的 Init → Provide → Start 接真实
// Mongo 副本集，钉住：并发 Stop 无竞争（-race）且全部返回 nil；重复 Stop 返回 nil；Stop 之后经 Mod
// 发布的 IMongo 的调用（Ping、集合读写、会话里的第一条命令）快速返回可 errors.Is 到
// mongo.ErrClientDisconnected 的错误（mongo 驱动的“已关闭”，mongo/driver/README.md 的 Close 一行）。
package mongo

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
)

func TestRealMongoModCloseContract(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; run against an isolated environment (scripts/mirror-local.sh or kit/scripts/integration/dataengine-env.sh)")
	}
	cfg := viper.New()
	cfg.Set("mongo.uri", uri)
	cfg.Set("mongo.require_replica_set", true)
	m := NewMongoMod()
	if err := m.Init(cfg); err != nil {
		t.Fatal(err)
	}
	r := app.NewRegistry(cfg)
	if err := m.Provide(r); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = m.StopWithContext(context.Background()) })
	if err := m.Start(); err != nil {
		t.Fatalf("Start against the isolated replica set = %v", err)
	}
	client, _ := app.Lookup[fmongo.IMongo](r, mods.ModMongo)
	if client == nil {
		t.Fatal("IMongo not published")
	}
	// 库名唯一，用完删掉（Stop 之前，经同一个客户端）。
	dbName := fmt.Sprintf("rr1006_10_%d", time.Now().UnixNano())
	coll := client.Database(dbName).Collection("close_contract")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := coll.InsertOne(ctx, map[string]any{"_id": "before", "n": 1}); err != nil {
		t.Fatalf("InsertOne before Stop = %v", err)
	}
	if err := client.Database(dbName).Drop(ctx); err != nil {
		t.Fatalf("drop %s = %v", dbName, err)
	}

	// 并发 Stop：-race 下无竞争，全部 nil（后到者等第一个做完）。
	var wg sync.WaitGroup
	errs := make([]error, 4)
	for i := range errs {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			stopCtx, stopCancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer stopCancel()
			errs[i] = m.StopWithContext(stopCtx)
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("concurrent Stop #%d = %v, want nil (all = %v)", i, err, errs)
		}
	}
	if m.Client() != nil {
		t.Fatal("client still held after every Stop returned nil")
	}
	for i := 0; i < 2; i++ {
		if err := m.StopWithContext(context.Background()); err != nil {
			t.Fatalf("Stop #%d after the Mod stopped = %v, want nil", i+2, err)
		}
	}
	// 客户端自己的 Close 也已幂等。
	if err := client.Close(context.Background()); err != nil {
		t.Fatalf("IMongo.Close after the Mod stopped = %v, want nil", err)
	}

	type call struct {
		name string
		do   func(context.Context) error
	}
	var found struct{ N int }
	calls := []call{
		{"IMongo.Ping", func(ctx context.Context) error { return client.Ping(ctx) }},
		{"ICollection.InsertOne", func(ctx context.Context) error {
			_, err := coll.InsertOne(ctx, map[string]any{"_id": "after"})
			return err
		}},
		{"ICollection.FindOne", func(ctx context.Context) error {
			return coll.FindOne(ctx, map[string]any{"_id": "before"}, &found)
		}},
		{"ICollection.UpdateOne", func(ctx context.Context) error {
			_, err := coll.UpdateOne(ctx, map[string]any{"_id": "before"}, map[string]any{"$set": map[string]any{"n": 2}})
			return err
		}},
		{"ICollection.CountDocuments", func(ctx context.Context) error {
			_, err := coll.CountDocuments(ctx, map[string]any{})
			return err
		}},
		{"ISession.WithTransaction", func(ctx context.Context) error {
			session, err := client.StartSession(ctx)
			if err != nil {
				return err
			}
			defer session.EndSession(ctx)
			return session.WithTransaction(ctx, func(txCtx context.Context) error {
				_, err := coll.InsertOne(txCtx, map[string]any{"_id": "tx"})
				return err
			})
		}},
	}
	for _, c := range calls {
		callCtx, callCancel := context.WithTimeout(context.Background(), 5*time.Second)
		started := time.Now()
		err := c.do(callCtx)
		callCancel()
		elapsed := time.Since(started)
		t.Logf("%s after Stop = %v (%s)", c.name, err, elapsed.Round(time.Millisecond))
		if !errors.Is(err, mongo.ErrClientDisconnected) {
			t.Errorf("%s after Stop = %v, want an error that errors.Is mongo.ErrClientDisconnected", c.name, err)
		}
		if elapsed > time.Second {
			t.Errorf("%s after Stop took %s; a closed client must fail fast", c.name, elapsed)
		}
	}
}
