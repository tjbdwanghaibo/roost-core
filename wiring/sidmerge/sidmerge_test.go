package sidmerge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"
	kitsyncbus "github.com/tjbdwanghaibo/roost-core/wiring/syncbus"
)

// testDeclared 是测试用的逻辑库：main 共享，zone 有 dbscope=sid 的 DAO。
var testDeclared = []coredata.LogicalDatabase{{Name: "main"}, {Name: "zone", ServerScoped: true}}

func serverConfig(values map[string]any) *viper.Viper {
	cfg := viper.New()
	for key, value := range values {
		cfg.Set(key, value)
	}
	return cfg
}

// 两侧各按自己的配置换算：同一个逻辑库在 A、B 可以映射到不同的物理库，按 sid 分库再各加自己的 sid。
func TestEachSideResolvesWithItsOwnMapping(t *testing.T) {
	coredata.DeclareDatabases(testDeclared...)
	plan := Plan{
		From: Server{SID: 1001, Config: serverConfig(map[string]any{
			"mongo.uri": "mongodb://user:secret@h2:27017,H1:27017/?replicaSet=rs0&w=majority", "dataengine.databases": map[string]any{"main": "planet", "zone": "zone_a"},
			"dataengine.database": "planet_ctl"})},
		Into: Server{SID: 1000, Config: serverConfig(map[string]any{
			"mongo.uri": "mongodb://h1:27017,h2:27017/?replicaSet=rs0", "dataengine.databases": map[string]any{"main": "planet", "zone": "zone_b"},
			"dataengine.database": "planet_ctl"})},
		Declared: testDeclared,
	}
	from, into, err := plan.resolve()
	if err != nil {
		t.Fatal(err)
	}
	if from.mongo != into.mongo || strings.Contains(from.mongo, "secret") {
		t.Fatalf("the same replica set written two ways must compare equal and drop credentials: %q vs %q", from.mongo, into.mongo)
	}
	r := &run{plan: plan, from: from, into: into}
	scoped := r.scopedDatabase("zone")
	if scoped.Source != "zone_a_1001" || scoped.Target != "zone_b_1000" {
		t.Fatalf("zone moves %s -> %s, want zone_a_1001 -> zone_b_1000", scoped.Source, scoped.Target)
	}
	if from.walDir != "data/wal/dataengine/1001" || into.walDir != "data/wal/dataengine/1000" {
		t.Fatalf("WAL directories %q / %q, want the per-sid default", from.walDir, into.walDir)
	}
	if from.controlDB != "planet_ctl" || from.saga.Store.Database != "saga" {
		t.Fatalf("control database %q, saga database %q", from.controlDB, from.saga.Store.Database)
	}
	if _, _, err := (Plan{From: plan.From, Into: Server{SID: 1001, Config: viper.New()}}).resolve(); err == nil {
		t.Fatal("merging a sid into itself was accepted")
	}
}

// 清单按 A 的配置说出哪些总线用法会把发往 A 的消息留在持久流里：同一工程，传输选择不同，清单不同。
func TestTheBusChecklistFollowsTheConfiguredTransports(t *testing.T) {
	plain, err := BuildChecklist(Server{SID: 1001, Config: serverConfig(map[string]any{"nats.prefix": "planet"})}, testDeclared)
	if err != nil {
		t.Fatal(err)
	}
	persistent, err := BuildChecklist(Server{SID: 1001, Config: serverConfig(map[string]any{
		"nats.prefix": "planet", "syncbus.transport": "jetstream", "syncbus.prefix": "planet.sync",
		"nats.rpc.transport": "jetstream", "nats.reliable.enabled": true,
	})}, testDeclared)
	if err != nil {
		t.Fatal(err)
	}
	item := func(list Checklist, name string) BusItem {
		for _, item := range list.Bus {
			if item.Name == name {
				return item
			}
		}
		t.Fatalf("checklist has no %q item: %+v", name, list.Bus)
		return BusItem{}
	}
	for _, name := range []string{"syncbus", "service rpc", "reliable bus"} {
		if item(plain, name).Persistent {
			t.Fatalf("%s is persistent under the default transports", name)
		}
		if got := item(persistent, name); !got.Persistent || !got.AddressedToSID || got.Action == "none" {
			t.Fatalf("%s under persistent transports = %+v", name, got)
		}
	}
	if got := item(persistent, "syncbus").Where; !strings.Contains(got, "PLANET_SYNC") || !strings.Contains(got, "_1001_") {
		t.Fatalf("syncbus item names %q, want the stream and A's durable pattern", got)
	}
	if got := item(persistent, "service rpc").Where; !strings.Contains(got, "ROOST_RPC_REQUESTS") || !strings.Contains(got, "rpc_<service>_1001_<method>") {
		t.Fatalf("rpc item names %q", got)
	}
	if busCheck(plain.Bus).Status != StatusOK || busCheck(persistent.Bus).Status != StatusConfirm {
		t.Fatalf("bus check: plain %v persistent %v", busCheck(plain.Bus), busCheck(persistent.Bus))
	}
	if len(plain.Databases) != 2 || !plain.Databases[1].ServerScoped || plain.Databases[0].ServerScoped {
		t.Fatalf("databases = %+v", plain.Databases)
	}
	if len(plain.Business) == 0 {
		t.Fatal("the business list is empty")
	}
}

// 本包自己读的键与对应 Mod 的声明取同一个缺省值：工具与进程按同一份配置得出同一个答案。
func TestToolKeysMatchModDeclarations(t *testing.T) {
	declared := func(schema app.ConfigSchema, key string) string {
		for _, entry := range schema.Keys {
			if entry.Name == key {
				return entry.Default
			}
		}
		t.Fatalf("no Mod declares %s", key)
		return ""
	}
	tool := func(schema app.ConfigSchema, key string) string { return declared(schema, key) }
	for _, pair := range []struct {
		key      string
		mod, own app.ConfigSchema
	}{
		{"mongo.uri", kitmongo.NewMongoMod().ConfigSchema(), app.SchemaOf(mongoKeys{})},
		{"syncbus.transport", kitsyncbus.NewSyncBusMod(0).ConfigSchema(), app.SchemaOf(syncbusKeys{})},
	} {
		if want, got := declared(pair.mod, pair.key), tool(pair.own, pair.key); want != got {
			t.Fatalf("%s: the Mod defaults to %q, sidmerge to %q", pair.key, want, got)
		}
	}
}

func walRecord(sequence byte) corenest.CommitRecord {
	var id corenest.TransactionID
	id[len(id)-1] = sequence
	return corenest.CommitRecord{
		ID: id, Handler: "sidmerge.test", RequestID: "request", CreatedAt: 1,
		Durability: corenest.DurabilityStrict.Record(),
		Mutations: []corenest.EntityMutation{{
			Key:  coredata.DocumentKey{ID: int64(sequence), Database: "main", Resource: "player"},
			Kind: coredata.MutationPut, ExpectedVersion: uint64(sequence) - 1, NextVersion: uint64(sequence), Schema: 1, Codec: "bson", Data: []byte{sequence},
		}},
	}
}

// WAL 检查：进程持有时拒绝；有未确认的尾部时不通过；全部确认后通过；配置的库名映射与该进程最后一次运行记下的
// 不同时不通过（这份配置不是那个进程用的）。
func TestTheWALCheckRequiresADrainedWALAndTheSameMapping(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	wal, err := nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.WriteDatabaseMapping(map[string]string{"main": "planet", "zone": "zone_a"}); err != nil {
		t.Fatal(err)
	}
	ticket, err := wal.Append(ctx, walRecord(1))
	if err != nil {
		t.Fatal(err)
	}
	coredata.DeclareDatabases(testDeclared...)
	resolveWith := func(mapping map[string]any) side {
		t.Helper()
		resolved, err := resolveSide(Server{SID: 1001, WALDir: dir, Config: serverConfig(map[string]any{"dataengine.databases": mapping})})
		if err != nil {
			t.Fatal(err)
		}
		return resolved
	}
	matching := resolveWith(map[string]any{"main": "planet", "zone": "zone_a"})
	if check := walCheck(ctx, matching); check.Status != StatusFail || !strings.Contains(check.Detail, "stop it first") {
		t.Fatalf("a WAL held by a running writer = %+v", check)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if report, err := WALDrained(ctx, dir); !errors.Is(err, ErrWALNotDrained) || report.Drained {
		t.Fatalf("an unacknowledged record passed: %+v, %v", report, err)
	}
	wal, err = nestwal.Open(nestwal.DefaultOptions(dir))
	if err != nil {
		t.Fatal(err)
	}
	if err := wal.Ack(ctx, ticket); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if check := walCheck(ctx, matching); check.Status != StatusOK {
		t.Fatalf("a drained WAL with the same mapping = %+v", check)
	}
	if check := walCheck(ctx, resolveWith(map[string]any{"main": "planet", "zone": "zone_other"})); check.Status != StatusFail || !strings.Contains(check.Detail, "last ran with") {
		t.Fatalf("a config whose mapping is not what the process ran with = %+v", check)
	}
	missing := matching
	missing.walDir = dir + "/not-here"
	if check := walCheck(ctx, missing); check.Status != StatusFail {
		t.Fatalf("a WAL directory that is not on this machine = %+v", check)
	}
	missing.walChecked = true
	if check := walCheck(ctx, missing); check.Status != StatusConfirm {
		t.Fatalf("an operator-confirmed WAL elsewhere = %+v", check)
	}
}
