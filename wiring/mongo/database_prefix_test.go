package mongo

import (
	"context"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// 库名前缀（维护者 2026-11-19：同一台机器共用一个 Mongo 的不同工程不撞库）：mongo.database_prefix 非空时，Mongo Mod
// 交给其他 Mod 的客户端打开的每个业务库都是 <前缀>_<库名>（按 sid 分库时 <前缀>_<库名>_<sid>），系统库 admin / local /
// config 原样；为空时库名就是业务自己写的名字。前缀是上层封装（WithDatabasePrefix），Mongo 驱动不知道它。
func TestMongoModHandsOutAClientThatPrefixesEveryBusinessDatabase(t *testing.T) {
	for _, c := range []struct {
		body, game, sid string
	}{
		{"mongo:\n  database_prefix: proj\n", "proj_game", "proj_game_1000"},
		{"mongo:\n  uri: mongodb://127.0.0.1:1\n", "game", "game_1000"},
	} {
		mod := NewMongoMod()
		cfg := mongoConfig(t, c.body)
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
		registry := app.NewRegistry(cfg) // 自带 health
		if err := mod.Provide(registry); err != nil {
			t.Fatal(err)
		}
		client, ok := app.Lookup[fmongo.IMongo](registry, mods.ModMongo)
		if !ok {
			t.Fatal("no mongo capability")
		}
		got := []string{client.Database("game").Name(), client.DatabaseForSid("game", 1000).Name(), client.Database("admin").Name(), client.Database("local").Name(), client.Database("config").Name()}
		want := []string{c.game, c.sid, "admin", "local", "config"}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%q: database names = %q, want %q", c.body, got, want)
		}
		_ = mod.StopWithContext(context.Background())
	}
}

// 封装本身：对任何 IMongo 都同一规则，前缀为空时原样返回同一个客户端。
func TestWithDatabasePrefixWrapsAnyClient(t *testing.T) {
	inner := mongotest.NewClient()
	if WithDatabasePrefix(inner, "") != fmongo.IMongo(inner) {
		t.Fatal("an empty prefix must hand back the client itself")
	}
	wrapped := WithDatabasePrefix(inner, "proj")
	if _, err := wrapped.Database("game").Collection("player").InsertOne(context.Background(), map[string]any{"_id": int64(1)}); err != nil {
		t.Fatal(err)
	}
	if inner.Collection("proj_game", "player").Len() != 1 || inner.Collection("game", "player").Len() != 0 {
		t.Fatal("a write through the prefixed client must land in proj_game only")
	}
}

// 前缀写错在 Init 拒绝：Mongo 库名不允许的字符、系统库名、超过 40 字节（给 _<库名>_<sid> 留位置，Mongo 库名上限 63）。
func TestMongoInitRefusesABadDatabasePrefix(t *testing.T) {
	for _, prefix := range []string{"a.b", "my proj", "admin", strings.Repeat("p", 41)} {
		cfg := mongoConfig(t, "mongo:\n  database_prefix: \""+prefix+"\"\n")
		if err := NewMongoMod().Init(cfg); err == nil || !strings.Contains(err.Error(), "mongo.database_prefix") {
			t.Errorf("database_prefix %q: Init = %v; want a refusal naming mongo.database_prefix", prefix, err)
		}
	}
}
