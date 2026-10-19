//go:build daoruntime

// 旧持久键的保留与清理（DESIGN-TODO D13 方案 A），在真实 Mongo 副本集上走正式 MongoStore 投影：
// ROOST_DATAENGINE_IT_MONGO_URI 没设置时跳过（scripts 里 source 隔离环境 env.sh 后运行）。每个用例用一次性库名，
// 结束时删库，绝不写共享 game 库。
package testdata

import (
	"bytes"
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
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

func requireLegacyKeys(t *testing.T, step string, doc bson.M) {
	t.Helper()
	if doc["old_gold"] != int64(40) {
		t.Errorf("%s: old_gold = %#v, want int64(40) kept", step, doc["old_gold"])
	}
	if z := subValue(doc, "pos", "old_z"); z != int32(9) {
		t.Errorf("%s: pos.old_z = %#v, want int32(9) kept", step, z)
	}
	if color := subValue(doc, "equips", "1", "old_color"); color != "red" {
		t.Errorf("%s: equips.1.old_color = %#v, want red kept", step, color)
	}
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

// 改 Go 字段名（没保留 bson 名）之后：加载 WARN、照常加载；业务的整文档写回、单字段补丁、投影重放、回滚
// 都不删库里的旧键，值不变；迁移把旧值搬到新键，显式清理入口删掉旧键，之后再加载不再告警。
func TestOldStoredKeysSurviveFullWritesUntilExplicitCleanupRealMongo(t *testing.T) {
	env := newStoredKeysEnv(t)
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	const id = 13001
	env.insertLegacy(t, id)
	d := env.load(t, id)
	if !strings.Contains(logs.String(), "key=old_gold") {
		t.Errorf("loading the legacy document did not warn:\n%s", logs.String())
	}

	// 回滚快照带着未知键往返：事务失败恢复后，再写回仍然保留。
	snapshot, err := d.CaptureRollbackState()
	if err != nil {
		t.Fatal(err)
	}
	d.name = "changed-then-rolled-back"
	if err := d.RestoreRollbackState(snapshot); err != nil {
		t.Fatal(err)
	}

	d.name = "renamed"
	record, err := env.write(t, d, true)
	if err != nil {
		t.Fatalf("full write: %v", err)
	}
	afterPut := env.stored(t, id)
	if afterPut["name"] != "renamed" || afterPut["_version"] != int64(2) {
		t.Fatalf("full write did not land: %v", afterPut)
	}
	requireLegacyKeys(t, "after a full write", afterPut)

	// 投影重放（同一条 WAL 记录再投影一次）幂等，旧键仍在。
	if err := env.store.Project(env.ctx, record); err != nil {
		t.Fatalf("replay: %v", err)
	}
	requireLegacyKeys(t, "after a replay", env.stored(t, id))

	// 单字段补丁本来就不碰别的键。
	d.name = "patched"
	if _, err := env.write(t, d, false); err != nil {
		t.Fatalf("patch: %v", err)
	}
	requireLegacyKeys(t, "after a patch", env.stored(t, id))

	// 第二次整文档写回（从重新加载的实例）同样保留。
	reloaded := env.load(t, id)
	reloaded.level = 8
	if _, err := env.write(t, reloaded, true); err != nil {
		t.Fatalf("second full write: %v", err)
	}
	requireLegacyKeys(t, "after a second full write", env.stored(t, id))

	// 离线迁移：停写后把旧值搬到新键（这里是 exp），版本加 1。
	current := env.stored(t, id)
	version := current["_version"].(int64)
	result, err := env.coll.UpdateOne(env.ctx, bson.M{"_id": id, "_version": version},
		bson.A{bson.M{"$set": bson.M{"exp": "$old_gold", "_version": version + 1, "_last_tx": "schema-migration:d13"}}})
	if err != nil || result.MatchedCount != 1 {
		t.Fatalf("migration: %v %v", result, err)
	}

	// 清理入口：拒绝仍被声明的键与元数据；删掉指定旧键。
	for _, bad := range [][]string{{"name"}, {"_version"}, {"equips.*.old_color"}, {"pos.x"}} {
		if _, err := engine.CleanupStoredKeys(env.ctx, engine.StoredKeyCleanup{Name: "d13", Collection: env.coll, Declared: heroDaoStoredKeys, Keys: bad}); !errors.Is(err, engine.ErrCleanupInvalid) {
			t.Errorf("cleanup of %v: err = %v, want ErrCleanupInvalid", bad, err)
		}
	}
	dry, err := engine.CleanupStoredKeys(env.ctx, engine.StoredKeyCleanup{Name: "d13", Collection: env.coll, Declared: heroDaoStoredKeys, Keys: []string{"old_gold", "pos.old_z"}, DryRun: true})
	if err != nil || dry.Matched != 1 || dry.Cleaned != 0 {
		t.Fatalf("dry run = %+v, %v", dry, err)
	}
	requireLegacyKeys(t, "after a dry run", env.stored(t, id))
	report, err := engine.CleanupStoredKeys(env.ctx, engine.StoredKeyCleanup{Name: "d13", Collection: env.coll, Declared: heroDaoStoredKeys, Keys: []string{"old_gold", "pos.old_z"}})
	if err != nil || report.Cleaned != 1 {
		t.Fatalf("cleanup = %+v, %v", report, err)
	}
	cleaned := env.stored(t, id)
	if _, ok := cleaned["old_gold"]; ok || subValue(cleaned, "pos", "old_z") != nil {
		t.Fatalf("cleanup left the keys: %v", cleaned)
	}
	if cleaned["exp"] != int64(40) || cleaned["_version"] != version+2 || cleaned["_last_tx"] != "stored-key-cleanup:d13" {
		t.Fatalf("cleanup result: exp=%v version=%v last_tx=%v", cleaned["exp"], cleaned["_version"], cleaned["_last_tx"])
	}
	if subValue(cleaned, "equips", "1", "old_color") != "red" {
		t.Fatalf("cleanup removed a key it was not asked to")
	}
	after := env.load(t, id)
	if after.GetExp() != 40 {
		t.Fatalf("migrated value not loaded: exp=%d", after.GetExp())
	}
}

// 两个进程实例同时加载并整文档写回：版本条件让后到的一方冲突（不放宽），先到的一方写回的文档保留旧键。
func TestConcurrentFullWritesKeepOldStoredKeysRealMongo(t *testing.T) {
	env := newStoredKeysEnv(t)
	const id = 13002
	env.insertLegacy(t, id)
	first, second := env.load(t, id), env.load(t, id)
	first.name, second.name = "first", "second"
	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i, d := range []*HeroDao{first, second} {
		wg.Add(1)
		go func(i int, d *HeroDao) {
			defer wg.Done()
			_, errs[i] = env.write(t, d, true)
		}(i, d)
	}
	wg.Wait()
	won := 0
	for _, err := range errs {
		switch {
		case err == nil:
			won++
		case !errors.Is(err, engine.ErrProjectionConflict):
			t.Fatalf("unexpected error: %v", err)
		}
	}
	if won != 1 {
		t.Fatalf("winners = %d (errors %v), want exactly one", won, errs)
	}
	doc := env.stored(t, id)
	if doc["_version"] != int64(2) {
		t.Fatalf("version = %v, want 2", doc["_version"])
	}
	requireLegacyKeys(t, "after concurrent full writes", doc)
}

// Remote 托管实体：DAO 数据放在版本信封的 data 二进制里，写入是 data 原样替换。从信封里水合的 DAO，交给 Remote 提交的
// 数据（MarshalPersist，生成实体的 RemoteDataMutation.Data）带着旧键，所以信封替换也不丢它们。
func TestRemoteEnvelopePayloadKeepsOldStoredKeysRealMongo(t *testing.T) {
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
	if payload["name"] != "renamed" {
		t.Fatalf("payload lost the change: %v", payload)
	}
	requireLegacyKeys(t, "remote payload", payload)
	// 清理入口不改写 Remote 信封。
	if _, err := engine.CleanupStoredKeys(env.ctx, engine.StoredKeyCleanup{Name: "d13", Collection: env.coll, Declared: heroDaoStoredKeys, Keys: []string{"old_gold"}}); !errors.Is(err, engine.ErrCleanupRemoteEnvelope) {
		t.Fatalf("cleanup over an envelope: err = %v, want ErrCleanupRemoteEnvelope", err)
	}
}

// 清理期间文档被别的写入者改了：版本条件不成立，整次中止，不覆盖别人的写入。
func TestCleanupStopsWhenTheCollectionIsStillWrittenRealMongo(t *testing.T) {
	env := newStoredKeysEnv(t)
	const id = 13004
	env.insertLegacy(t, id)
	racing := &racingCollection{ICollection: env.coll, race: func() {
		_, _ = env.coll.UpdateOne(env.ctx, bson.M{"_id": int64(id)}, bson.M{"$inc": bson.M{"_version": int64(1)}})
	}}
	_, err := engine.CleanupStoredKeys(env.ctx, engine.StoredKeyCleanup{Name: "d13", Collection: racing, Declared: heroDaoStoredKeys, Keys: []string{"old_gold"}})
	if !errors.Is(err, engine.ErrCleanupNotStopped) {
		t.Fatalf("err = %v, want ErrCleanupNotStopped", err)
	}
	requireLegacyKeys(t, "after an aborted cleanup", env.stored(t, id))
}

// racingCollection 在清理读完候选文档之后、写回之前插入一次别人的写入。
type racingCollection struct {
	fmongo.ICollection
	race func()
}

func (c *racingCollection) Find(ctx context.Context, filter any, results any, opts ...fmongo.FindOption) error {
	if err := c.ICollection.Find(ctx, filter, results, opts...); err != nil {
		return err
	}
	c.race()
	return nil
}
