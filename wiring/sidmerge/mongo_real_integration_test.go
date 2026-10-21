//go:build integration

package sidmerge

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// 真实 Mongo 上的合服迁移与回退（设计 §8 sidmerge）：两侧各按自己的映射换算源 / 目标库；非按 sid 的逻辑库位置不同
// 拒绝；outbox 非空且不共用控制库拒绝；非终态 saga 拒绝；同一 _id 内容不同整体停止、什么也不写；处置后迁移，重跑
// 幂等；B 写入之后回退，文档按 B 的当前版本回到 A，B 里新产生的只计数。
//
// 资源：ROOST_DATAENGINE_IT_MONGO_URI 指向的副本集上，库名都以 sidmergeit_<pid>_<纳秒> 开头，用后删除。共享隔离环境
// 下运行时持 remote-acceptance.lock（wiring/scripts/integration/README.md）。
func TestSidMergeMigratesAndRevertsOnRealMongo(t *testing.T) {
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	unique := fmt.Sprintf("sidmergeit_%d_%d", os.Getpid(), time.Now().UnixNano())
	client, err := connect(ctx, uri)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		names, err := client.ListDatabaseNames(context.Background(), bson.D{{Key: "name", Value: bson.D{{Key: "$regex", Value: "^" + unique}}}})
		if err != nil {
			t.Logf("cleanup: list databases: %v", err)
		}
		for _, name := range names {
			if err := client.Database(name).Drop(context.Background()); err != nil {
				t.Logf("cleanup: drop %s: %v", name, err)
			}
		}
		_ = client.Disconnect(context.Background())
	})
	const sidA, sidB = int32(1001), int32(1000)
	declared := []coredata.LogicalDatabase{{Name: "main"}, {Name: "zone", ServerScoped: true}}
	names := struct{ main, mainOther, zoneA, zoneB, controlA, controlB, saga string }{
		unique + "_main", unique + "_main_b", unique + "_za", unique + "_zb", unique + "_ctl_a", unique + "_ctl_b", unique + "_saga",
	}
	emptyWAL := func() string {
		dir := t.TempDir()
		wal, err := nestwal.Open(nestwal.DefaultOptions(dir))
		if err != nil {
			t.Fatal(err)
		}
		if err := wal.Close(ctx); err != nil {
			t.Fatal(err)
		}
		return dir
	}
	walA, walB := emptyWAL(), emptyWAL()
	config := func(main, zone, control string) *viper.Viper {
		cfg := viper.New()
		cfg.Set("mongo.uri", uri)
		cfg.Set("dataengine.databases", map[string]any{"main": main, "zone": zone})
		cfg.Set("dataengine.database", control)
		cfg.Set("saga.database", names.saga)
		return cfg
	}
	plan := func(intoMain, intoControl string) Plan {
		return Plan{
			From:     Server{SID: sidA, Config: config(names.main, names.zoneA, names.controlA), WALDir: walA},
			Into:     Server{SID: sidB, Config: config(intoMain, names.zoneB, intoControl), WALDir: walB},
			Declared: declared,
		}
	}
	source := client.Database(serverScopedDatabase(names.zoneA, sidA)).Collection("plots")
	target := client.Database(serverScopedDatabase(names.zoneB, sidB)).Collection("plots")
	insert := func(collection *mongo.Collection, documents ...bson.D) {
		t.Helper()
		for _, document := range documents {
			if _, err := collection.InsertOne(ctx, document); err != nil {
				t.Fatal(err)
			}
		}
	}
	// A：两份玩家相关文档（全局唯一 ID）、一份在各服取同一常量 ID 的文档；B：同一常量 ID、内容不同。
	insert(source,
		bson.D{{Key: "_id", Value: int64(7001)}, {Key: "_version", Value: int64(3)}, {Key: "_schema", Value: int64(1)}, {Key: "_last_tx", Value: "tx-a1"}, {Key: "owner", Value: "a1"}},
		bson.D{{Key: "_id", Value: int64(7002)}, {Key: "_version", Value: int64(5)}, {Key: "_schema", Value: int64(1)}, {Key: "_last_tx", Value: "tx-a2"}, {Key: "_deleted", Value: true}},
		bson.D{{Key: "_id", Value: "season"}, {Key: "_version", Value: int64(9)}, {Key: "round", Value: int64(4)}})
	insert(target, bson.D{{Key: "_id", Value: "season"}, {Key: "_version", Value: int64(2)}, {Key: "round", Value: int64(1)}})
	// A 的基础库（非按 sid 部分）里有数据：位置不同的映射必须被拒绝。
	insert(client.Database(names.main).Collection("player"), bson.D{{Key: "_id", Value: int64(1)}})

	expectFail := func(report Report, err error, name string) {
		t.Helper()
		if !errors.Is(err, ErrNotReady) || report.Ready {
			t.Fatalf("%s: dry-run = ready %v, %v; want ErrNotReady", name, report.Ready, err)
		}
		for _, check := range report.Checks {
			if strings.HasPrefix(check.Name, name) && check.Status == StatusFail {
				return
			}
		}
		t.Fatalf("no failed %s check in %+v", name, report.Checks)
	}

	// 1. 非按 sid 的逻辑库 main 在 B 映射到另一个库：拒绝并点名。
	report, err := DryRun(ctx, plan(names.mainOther, names.controlA))
	expectFail(report, err, "co-location:main")
	// 2. 控制库不同、A 的 outbox 里还有 effect：拒绝。
	if _, err := client.Database(names.controlA).Collection(engine.OutboxCollection).InsertOne(ctx, bson.D{{Key: "_id", Value: "effect-1"}}); err != nil {
		t.Fatal(err)
	}
	report, err = DryRun(ctx, plan(names.main, names.controlB))
	expectFail(report, err, fmt.Sprintf("outbox:%d", sidA))
	// 共用控制库时 B 会发出 A 剩下的 effect：放行。
	report, _ = DryRun(ctx, plan(names.main, names.controlA))
	for _, check := range report.Checks {
		if check.Name == fmt.Sprintf("outbox:%d", sidA) && check.Status != StatusOK {
			t.Fatalf("a shared control database = %+v", check)
		}
	}
	if _, err := client.Database(names.controlA).Collection(engine.OutboxCollection).DeleteMany(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	// 3. 非终态 saga：拒绝。
	sagaClient, err := mongodriver.NewClient(fmongo.DefaultConfig(uri), mongodriver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sagaClient.Close(context.Background()) }()
	sagas, err := coresaga.NewMongoStore(sagaClient, coresaga.MongoStoreOptions{Database: names.saga})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	pending := coresaga.Record{ID: "gift1", Type: "gift_item", DefinitionVersion: 1, BusinessKey: "gift-1", Status: coresaga.StatusPending,
		Phase: coresaga.PhaseForward, Version: 1, NextRunAt: now, CreatedAt: now, UpdatedAt: now}
	if err := sagas.Create(ctx, pending); err != nil {
		t.Fatal(err)
	}
	report, err = DryRun(ctx, plan(names.main, names.controlB))
	expectFail(report, err, "saga:"+names.saga)
	if _, err := client.Database(names.saga).Collection("_sagas").DeleteMany(ctx, bson.D{}); err != nil {
		t.Fatal(err)
	}
	// 4. 同一常量 ID、内容不同：整体停止，迁移什么也不写。
	merge := plan(names.main, names.controlB)
	_, _, err = Migrate(ctx, merge)
	if !errors.Is(err, ErrNotReady) {
		t.Fatalf("migrate with a conflict = %v", err)
	}
	if count, _ := target.CountDocuments(ctx, bson.D{}); count != 1 {
		t.Fatalf("a refused migrate wrote %d documents into the target", count-1)
	}
	report, _ = DryRun(ctx, merge)
	if len(report.Conflicts) != 1 || report.Conflicts[0].ID != `{"_id":"season"}` {
		t.Fatalf("conflicts = %+v", report.Conflicts)
	}
	// 业务处置：B 的常量文档改名（重新编号）。
	if _, err := target.DeleteOne(ctx, bson.D{{Key: "_id", Value: "season"}}); err != nil {
		t.Fatal(err)
	}
	insert(target, bson.D{{Key: "_id", Value: "season-b"}, {Key: "_version", Value: int64(2)}, {Key: "round", Value: int64(1)}})

	// 5. 迁移：三份文档原样进入 B（含版本与墓碑字段），源库保留。
	report, manifest, err := Migrate(ctx, merge)
	if err != nil {
		t.Fatalf("migrate: %v (%+v)", err, report.Checks)
	}
	if report.Copied != 3 || len(manifest.Documents) != 3 || manifest.Databases[0].Source != serverScopedDatabase(names.zoneA, sidA) || manifest.Databases[0].Target != serverScopedDatabase(names.zoneB, sidB) {
		t.Fatalf("migrate report copied %d, manifest %+v", report.Copied, manifest)
	}
	var tomb bson.M
	if err := target.FindOne(ctx, bson.D{{Key: "_id", Value: int64(7002)}}).Decode(&tomb); err != nil || tomb["_deleted"] != true || tomb["_version"] != int64(5) || tomb["_last_tx"] != "tx-a2" {
		t.Fatalf("the tombstone arrived as %v (%v)", tomb, err)
	}
	if count, _ := source.CountDocuments(ctx, bson.D{}); count != 3 {
		t.Fatalf("the source archive holds %d documents after the migrate, want 3", count)
	}
	// 重跑幂等：不写，全部记为 already_present。
	again, againManifest, err := Migrate(ctx, merge)
	if err != nil || again.Copied != 0 || again.AlreadyPresent != 3 || len(againManifest.Documents) != 3 {
		t.Fatalf("a repeated migrate = copied %d already %d, %v", again.Copied, again.AlreadyPresent, err)
	}

	// 6. 合服后 B 写入：一份迁入的文档版本前进，另有一份新文档。
	if _, err := target.UpdateOne(ctx, bson.D{{Key: "_id", Value: int64(7001)}}, bson.D{{Key: "$set", Value: bson.D{{Key: "_version", Value: int64(4)}, {Key: "owner", Value: "a1-on-b"}, {Key: "_last_tx", Value: "tx-b1"}}}}); err != nil {
		t.Fatal(err)
	}
	insert(target, bson.D{{Key: "_id", Value: int64(7003)}, {Key: "_version", Value: int64(1)}})

	// 7. 回退：清单上的文档按 B 的当前版本回到 A、从 B 删掉；B 新产生的与 B 自己的留下并计数。
	reverted, err := Revert(ctx, merge, manifest)
	if err != nil {
		t.Fatalf("revert: %v (%+v)", err, reverted.Checks)
	}
	if reverted.Reverted != 3 || reverted.NewInTarget["zone/plots"] != 2 {
		t.Fatalf("revert = %+v, want 3 reverted and 2 left in B (season-b, 7003)", reverted)
	}
	var back bson.M
	if err := source.FindOne(ctx, bson.D{{Key: "_id", Value: int64(7001)}}).Decode(&back); err != nil || back["_version"] != int64(4) || back["owner"] != "a1-on-b" {
		t.Fatalf("7001 in A after revert = %v (%v), want B's version 4", back, err)
	}
	if count, _ := target.CountDocuments(ctx, bson.D{{Key: "_id", Value: bson.D{{Key: "$in", Value: bson.A{int64(7001), int64(7002), "season"}}}}}); count != 0 {
		t.Fatalf("%d reverted documents are still in B", count)
	}
	// 再回退一次：清单上的文档已不在 B，整体拒绝，什么也不写。
	if again, err := Revert(ctx, merge, manifest); !errors.Is(err, ErrNotReady) || len(again.Missing) != 3 || again.Reverted != 0 {
		t.Fatalf("a second revert = %+v, %v", again, err)
	}
}
