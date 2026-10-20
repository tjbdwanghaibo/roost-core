package dataengine

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	engine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// DAO 写的 db= 是逻辑库名；配置 dataengine.databases 把它映射到物理库。提交经 WAL 投影后，
// 文档必须落在映射后的物理库，逻辑库名本身不出现在 Mongo 里。
func TestDataEngineModProjectsLogicalDatabaseIntoMappedPhysicalDatabase(t *testing.T) {
	coredata.DeclareDatabases(coredata.LogicalDatabase{Name: "main"}) // 生成的 gen_databases.go 在 init 里做的事
	cfg := newModConfig(t)
	cfg.Set("dataengine.databases", map[string]any{"main": "planet_x"})
	mongoClient := mongotest.NewClient()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModMongo, fmongo.IMongo(mongoClient)); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModNatsJetStream, fnats.IJetStream(&modJetStream{})); err != nil {
		t.Fatal(err)
	}
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	if err := mod.Init(cfg); err != nil {
		t.Fatalf("Init with dataengine.databases {main: planet_x}: %v", err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	if err := mod.Start(); err != nil {
		t.Fatal(err)
	}
	defer mod.Stop()

	data, err := bson.Marshal(bson.M{"_id": int64(7), "hp": 1})
	if err != nil {
		t.Fatal(err)
	}
	record := coredata.CommitRecord{ID: coredata.TransactionID{1}, Durability: coredata.DurabilityStrict, Mutations: []coredata.Mutation{{
		Key:  coredata.DocumentKey{Database: "main", Resource: "player", ID: 7},
		Kind: coredata.MutationPut, NextVersion: 1, Schema: 1, Codec: "bson-v2", Data: data,
	}}}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	ticket, err := mod.Runtime().Projector.CommitSystem(ctx, record)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-ticket.Done():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if err := ticket.Err(); err != nil {
		t.Fatal(err)
	}
	var stored bson.M
	physicalErr := mongoClient.Database("planet_x").Collection("player").FindOne(ctx, bson.M{"_id": int64(7)}, &stored)
	logicalErr := mongoClient.Database("main").Collection("player").FindOne(ctx, bson.M{"_id": int64(7)}, &bson.M{})
	if physicalErr != nil || logicalErr == nil {
		t.Fatalf("document in physical planet_x: %v; in logical main: %v — want it only in planet_x", physicalErr, logicalErr)
	}
}

// 回调对每个声明的逻辑库只在 Init 时调用一次；配置没写的逻辑库取回调的非空结果，配置写了的回调返回空串即沿用配置；
// 回调与配置对同一逻辑库给出不同物理名、配置写错（未知逻辑名、系统库）都在 Init 拒绝并点名。
func TestDataEngineModFreezesTheMappingAtInitAndRefusesInvalidConfig(t *testing.T) {
	coredata.DeclareDatabases(coredata.LogicalDatabase{Name: "wiring_main"}, coredata.LogicalDatabase{Name: "wiring_audit", ServerScoped: true})
	calls := map[string]int{}
	cfg := newModConfig(t)
	cfg.Set("sid", 12)
	cfg.Set("dataengine.databases", map[string]any{"wiring_main": "planet_main"})
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())), WithDatabaseResolver(func(logical string) (string, error) {
		calls[logical]++
		if logical == "wiring_audit" {
			return "vault_audit", nil
		}
		return "", nil
	}))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	databases := mod.cfg.mongo.Databases
	if databases.Physical("wiring_main") != "planet_main" || databases.Physical("wiring_audit") != "vault_audit" {
		t.Fatalf("frozen map = %+v", databases.Entries())
	}
	for _, logical := range []string{"wiring_main", "wiring_audit"} {
		if calls[logical] != 1 {
			t.Fatalf("resolver called %d times for %s at Init, want 1", calls[logical], logical)
		}
	}
	for i := 0; i < 3; i++ {
		_ = databases.Physical("wiring_main")
	}
	if calls["wiring_main"] != 1 {
		t.Fatal("looking up the frozen map called the resolver")
	}

	// 同一逻辑库两个来源给出不同的值：Init 拒绝，报错点名逻辑库与两个值。
	conflict := newModConfig(t)
	conflict.Set("sid", 12)
	conflict.Set("dataengine.databases", map[string]any{"wiring_main": "planet_main", "wiring_audit": "planet_audit"})
	err := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())), WithDatabaseResolver(func(logical string) (string, error) {
		if logical == "wiring_audit" {
			return "vault_audit", nil
		}
		return "", nil
	})).Init(conflict)
	if err == nil || !strings.Contains(err.Error(), `logical database "wiring_audit"`) || !strings.Contains(err.Error(), `"planet_audit"`) || !strings.Contains(err.Error(), `"vault_audit"`) {
		t.Fatalf("config planet_audit vs resolver vault_audit: Init = %v, want a refusal naming wiring_audit and both values", err)
	}

	for key, want := range map[string]string{
		"wiring_mian":  `no DAO declares logical database "wiring_mian"`,
		"wiring_audit": `is a Mongo system database`,
	} {
		bad := newModConfig(t)
		physical := "x"
		if key == "wiring_audit" {
			physical = "config"
		}
		bad.Set("dataengine.databases", map[string]any{key: physical})
		if err := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager()))).Init(bad); err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("dataengine.databases.%s=%s: Init = %v, want a refusal containing %q", key, physical, err, want)
		}
	}
}

// 工具读同一份服务配置、按同一条规则得到物理库名；DAOCollection 对按 sid 分库的 DAO 加 _<sid>。
func TestToolsReadTheSameDatabaseMapping(t *testing.T) {
	coredata.DeclareDatabases(coredata.LogicalDatabase{Name: "wiring_main"})
	path := filepath.Join(t.TempDir(), "config.game.yaml")
	if err := os.WriteFile(path, []byte("sid: 1000\ndataengine:\n  databases: {wiring_main: planet_tool}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	databases, err := LoadDatabaseMapFile(path, 0, nil)
	if err != nil || databases.Physical("wiring_main") != "planet_tool" {
		t.Fatalf("LoadDatabaseMapFile = %v, %v", databases, err)
	}
	client := mongotest.NewClient()
	ctx := context.Background()
	if _, err := DAOCollection(client, databases, scopedDAO{global: true}, 1000).InsertOne(ctx, bson.M{"_id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if _, err := DAOCollection(client, databases, scopedDAO{}, 1000).InsertOne(ctx, bson.M{"_id": int64(2)}); err != nil {
		t.Fatal(err)
	}
	if client.Collection("planet_tool", "player").Len() != 1 || client.Collection("planet_tool_1000", "player").Len() != 1 {
		t.Fatal("a global DAO must land in planet_tool and a dbscope=sid DAO in planet_tool_1000")
	}
}

type scopedDAO struct{ global bool }

func (scopedDAO) DbName() string   { return "wiring_main" }
func (scopedDAO) CollName() string { return "player" }
func (d scopedDAO) DbScope() coredata.DatabaseScope {
	if d.global {
		return coredata.DatabaseGlobal
	}
	return coredata.DatabaseServer
}

// Remote 存储写 DAO 文档用 DataEngine 冻结的同一张表：Provide 把它绑定到 remote_entity.atomic_store，并发布
// dataengine.databases 能力给业务与服务代码。
type bindingRemoteStore struct {
	remoteStoreStub
	bound *coredata.DatabaseMap
}

func (store *bindingRemoteStore) BindDatabaseMap(databases *coredata.DatabaseMap) error {
	store.bound = databases
	return nil
}

func TestDataEngineModBindsTheFrozenMapToRemoteStorageAndPublishesIt(t *testing.T) {
	cfg := newModConfig(t)
	registry := app.NewRegistry(cfg)
	store := &bindingRemoteStore{}
	for name, value := range map[app.ModName]any{
		mods.ModMongo: fmongo.IMongo(mongotest.NewClient()), mods.ModNatsJetStream: fnats.IJetStream(&modJetStream{}),
		mods.ModRemoteEntity: entity.IRemoteEntityManager(remoteManagerStub{}), mods.ModRemoteEntityAtomicStore: engine.RemoteProjectionStore(store),
	} {
		if err := registry.Register(name, value); err != nil {
			t.Fatal(err)
		}
	}
	mod := NewMod(WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())), WithRemoteProjection(true))
	if err := mod.Init(cfg); err != nil {
		t.Fatal(err)
	}
	if err := mod.Provide(registry); err != nil {
		t.Fatal(err)
	}
	published, err := Databases(registry)
	if err != nil || store.bound == nil || store.bound != mod.cfg.mongo.Databases || published != store.bound {
		t.Fatalf("bound=%p published=%p (%v) frozen=%p; want the one frozen map everywhere", store.bound, published, err, mod.cfg.mongo.Databases)
	}
}
