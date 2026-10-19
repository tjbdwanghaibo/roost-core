//go:build daoruntime

// 旧持久键不再读取（DESIGN-TODO D13，维护者 2026-10-19 调整：撤销方案 A），在真实 Mongo 副本集上走正式 MongoStore
// 投影：ROOST_DATAENGINE_IT_MONGO_URI 没设置时跳过（scripts 里 source 隔离环境 env.sh 后运行）。每个用例用一次性库名，
// 结束时删库，绝不写共享 game 库。
package testdata

import (
	"bytes"
	"context"
	"crypto/rand"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
)

type storedKeysEnv struct {
	ctx      context.Context
	client   fmongo.IMongo
	store    *engine.MongoStore
	database string
	coll     fmongo.ICollection
}

func newStoredKeysEnv(t *testing.T) *storedKeysEnv {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; source the isolated environment env.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_d13_it_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		_ = client.Database(database).Drop(context.Background())
		_ = client.Close(context.Background())
	})
	store, err := engine.NewMongoStore(client, engine.MongoStoreConfig{DefaultDatabase: database})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	return &storedKeysEnv{ctx: ctx, client: client, store: store, database: database, coll: client.Database(database).Collection(HeroDaoCollection)}
}

// insertLegacy 写一份“旧代码”留下的文档：它有当前 HeroDao 不再声明的键（相当于字段 Gold 改名后没保留 bson 名，
// 嵌套 Position 删了一个字段 Z，EquipInfo 删了 Color）。
func (env *storedKeysEnv) insertLegacy(t *testing.T, id int64) {
	t.Helper()
	doc := bson.D{
		{Key: "_id", Value: id}, {Key: "_schema", Value: int64(HeroDaoSchemaVersion)}, {Key: "_version", Value: int64(1)}, {Key: "_last_tx", Value: "old-code"},
		{Key: "name", Value: "hero"}, {Key: "level", Value: int32(7)}, {Key: "exp", Value: int64(0)},
		{Key: "pos", Value: bson.D{{Key: "x", Value: int32(3)}, {Key: "y", Value: int32(4)}, {Key: "old_z", Value: int32(9)}}},
		{Key: "equips", Value: bson.D{{Key: "1", Value: bson.D{{Key: "level", Value: int32(5)}, {Key: "old_color", Value: "red"}}}}},
		{Key: "old_gold", Value: int64(40)},
	}
	if _, err := env.coll.InsertOne(env.ctx, doc); err != nil {
		t.Fatal(err)
	}
}

// load 是冷加载的正式两步：MongoStore 读出文档，生成的 RestorePersisted 水合。
func (env *storedKeysEnv) load(t *testing.T, id int64) *HeroDao {
	t.Helper()
	docs, err := env.store.Load(env.ctx, coredata.LoadSpec{Database: env.database, Scope: coredata.DatabaseGlobal, Resource: HeroDaoCollection, Filter: map[string]any{"_id": id}})
	if err != nil || len(docs) != 1 {
		t.Fatalf("load %d: docs=%d err=%v", id, len(docs), err)
	}
	d := NewHeroDao()
	if err := d.RestorePersisted(docs[0].Data, docs[0].Schema, docs[0].Version); err != nil {
		t.Fatalf("restore %d: %v", id, err)
	}
	return d
}

// write 把 DAO 的一次修改按正式路径做成 mutation（full=true 时整文档 Put，否则单字段 patch），投影进 Mongo。
func (env *storedKeysEnv) write(t *testing.T, d *HeroDao, full bool) (coredata.CommitRecord, error) {
	t.Helper()
	change := nest.PersistChange{Mask: coredata.AllFields}
	if !full {
		change = nest.PersistChange{Mask: heroDaoFieldName}
	}
	mutation, err := d.PrepareMutation(change)
	if err != nil {
		t.Fatal(err)
	}
	mutation.Key.Database, mutation.Key.Scope = env.database, coredata.DatabaseGlobal
	record := coredata.CommitRecord{ID: newTransactionID(t), Handler: "d13", CreatedAt: time.Now().UnixNano(), Durability: coredata.DurabilityStrict, Mutations: []coredata.Mutation{mutation}}
	if err := env.store.Project(env.ctx, record); err != nil {
		return record, err
	}
	return record, d.AcceptMutation(mutation)
}

func (env *storedKeysEnv) stored(t *testing.T, id int64) bson.M {
	t.Helper()
	var doc bson.M
	if err := env.coll.FindOne(env.ctx, bson.M{"_id": id}, &doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func newTransactionID(t *testing.T) coredata.TransactionID {
	var id coredata.TransactionID
	if _, err := rand.Read(id[:]); err != nil {
		t.Fatal(err)
	}
	return id
}

// legacyKeys 报出 doc 里还在的旧键（insertLegacy 写下的三个）。
func legacyKeys(doc bson.M) []string {
	var left []string
	if _, ok := doc["old_gold"]; ok {
		left = append(left, "old_gold")
	}
	if subValue(doc, "pos", "old_z") != nil {
		left = append(left, "pos.old_z")
	}
	if subValue(doc, "equips", "1", "old_color") != nil {
		left = append(left, "equips.1.old_color")
	}
	return left
}

func subValue(doc bson.M, path ...string) any {
	var current any = doc
	for _, key := range path {
		switch v := current.(type) {
		case bson.M:
			current = v[key]
		case bson.D:
			found := false
			for _, e := range v {
				if e.Key == key {
					current, found = e.Value, true
					break
				}
			}
			if !found {
				return nil
			}
		default:
			return nil
		}
	}
	return current
}

// 改 Go 字段名（没保留 bson 名）、删字段之后：加载 WARN 一次、照常加载但不读入旧键的值，DAO 内存与它交出的任何
// 文档（整文档写入、回滚快照）里都没有旧键。单字段补丁（$set）不碰别的键，旧键留在库里但不再被读取；下一次整文档
// 写回把旧键去掉（与 D13 之前的写入语义一致），重放同一条记录幂等。要保留数据，业务给字段写原 bson 名或先离线迁移。
func TestOldStoredKeysAreNotLoadedAndTheNextFullWriteDropsThemRealMongo(t *testing.T) {
	env := newStoredKeysEnv(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	const id = 13001
	env.insertLegacy(t, id)
	d := env.load(t, id)
	text := logs.String()
	if !strings.Contains(text, "key=old_gold") || !strings.Contains(text, "no longer read") {
		t.Errorf("loading the legacy document did not warn that the key is no longer read:\n%s", text)
	}
	if d.GetLevel() != 7 || d.GetPos().GetX() != 3 {
		t.Fatalf("declared fields not loaded: level=%d pos.x=%d", d.GetLevel(), d.GetPos().GetX())
	}
	full, err := d.marshalCommitState()
	if err != nil {
		t.Fatal(err)
	}
	if left := heroDaoStoredKeys.UnknownKeys(full); len(left) != 0 {
		t.Fatalf("the loaded DAO still carries old keys %v in its full document", left)
	}
	snapshot, err := d.CaptureRollbackState()
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(snapshot, []byte("old_gold")) || bytes.Contains(snapshot, []byte("old_z")) || bytes.Contains(snapshot, []byte("old_color")) {
		t.Fatalf("the rollback snapshot carries old keys: %s", bson.Raw(snapshot))
	}

	// 单字段补丁只 $set 自己的键：旧键留在库里，但不再被读取。
	d.name = "patched"
	if _, err := env.write(t, d, false); err != nil {
		t.Fatalf("patch: %v", err)
	}
	afterPatch := env.stored(t, id)
	if afterPatch["name"] != "patched" {
		t.Fatalf("patch did not land: %v", afterPatch)
	}
	if left := legacyKeys(afterPatch); len(left) != 3 {
		t.Fatalf("a single-field patch touched other keys: old keys left %v, want all three", left)
	}

	// 整文档写回：旧键（顶层与嵌套）都被去掉。
	d.name = "renamed"
	record, err := env.write(t, d, true)
	if err != nil {
		t.Fatalf("full write: %v", err)
	}
	afterPut := env.stored(t, id)
	if afterPut["name"] != "renamed" || afterPut["_version"] != int64(3) {
		t.Fatalf("full write did not land: %v", afterPut)
	}
	if left := legacyKeys(afterPut); len(left) != 0 {
		t.Fatalf("after a full write the old keys %v are still stored, want them dropped", left)
	}
	if afterPut["level"] != int32(7) || subValue(afterPut, "pos", "x") != int32(3) || subValue(afterPut, "equips", "1", "level") != int32(5) {
		t.Fatalf("a full write lost declared values: %v", afterPut)
	}

	// 投影重放（同一条 WAL 记录再投影一次）幂等：不把旧键带回来，也不改版本。
	if err := env.store.Project(env.ctx, record); err != nil {
		t.Fatalf("replay: %v", err)
	}
	replayed := env.stored(t, id)
	if left := legacyKeys(replayed); len(left) != 0 || replayed["_version"] != int64(3) {
		t.Fatalf("after a replay: old keys %v, version %v", left, replayed["_version"])
	}
	docs, err := env.store.Load(env.ctx, coredata.LoadSpec{Database: env.database, Scope: coredata.DatabaseGlobal, Resource: HeroDaoCollection, Filter: map[string]any{"_id": int64(id)}})
	if err != nil || len(docs) != 1 {
		t.Fatalf("reload: %v %v", docs, err)
	}
	if left := heroDaoStoredKeys.UnknownKeys(docs[0].Data); len(left) != 0 {
		t.Fatalf("the stored document still has keys the DAO does not declare: %v", left)
	}
}

// Remote 托管实体：DAO 数据放在版本信封的 data 二进制里，写入是 data 原样替换。从信封里水合的 DAO 交给 Remote 提交的
// 数据（MarshalPersist，生成实体的 RemoteDataMutation.Data）不含旧键，信封替换后旧键同样被去掉。
func TestRemoteEnvelopePayloadDropsOldStoredKeysRealMongo(t *testing.T) {
	env := newStoredKeysEnv(t)
	const id = 13003
	legacy, err := bson.Marshal(bson.D{
		{Key: "_id", Value: int64(id)}, {Key: "_schema", Value: int64(HeroDaoSchemaVersion)}, {Key: "name", Value: "guild"},
		{Key: "pos", Value: bson.D{{Key: "x", Value: int32(3)}, {Key: "old_z", Value: int32(9)}}},
		{Key: "equips", Value: bson.D{{Key: "1", Value: bson.D{{Key: "old_color", Value: "red"}}}}},
		{Key: "old_gold", Value: int64(40)},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.coll.InsertOne(env.ctx, bson.D{{Key: "_id", Value: int64(id)}, {Key: "_ver", Value: int64(3)}, {Key: "data", Value: legacy}}); err != nil {
		t.Fatal(err)
	}
	docs, err := env.store.Load(env.ctx, coredata.LoadSpec{Database: env.database, Scope: coredata.DatabaseGlobal, Resource: HeroDaoCollection, Filter: map[string]any{"_id": int64(id)}})
	if err != nil || len(docs) != 1 || !docs[0].Enveloped || docs[0].Version != 3 {
		t.Fatalf("envelope load: %+v %v", docs, err)
	}
	var envelope struct {
		Data []byte `bson:"data"`
	}
	if err := bson.Unmarshal(docs[0].Data, &envelope); err != nil {
		t.Fatal(err)
	}
	d := NewHeroDao()
	if err := d.RestorePersisted(envelope.Data, HeroDaoSchemaVersion, docs[0].Version); err != nil {
		t.Fatal(err)
	}
	d.name = "renamed"
	var payload bson.M
	if err := bson.Unmarshal(d.MarshalPersist(heroDaoFieldName), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["name"] != "renamed" || subValue(payload, "pos", "x") != int32(3) {
		t.Fatalf("payload lost declared values: %v", payload)
	}
	if left := legacyKeys(payload); len(left) != 0 {
		t.Fatalf("remote payload carries old keys %v, want them dropped", left)
	}
}
