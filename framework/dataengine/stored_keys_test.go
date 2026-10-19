package dataengine

import (
	"bytes"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

type storedKeysGem struct {
	ID    int32 `bson:"id"`
	Level int32 `bson:"level"`
}

type storedKeysEquip struct {
	Level int32                    `bson:"level"`
	Gems  map[int32]*storedKeysGem `bson:"gems"`
	Runes []storedKeysGem          `bson:"runes"`
}

type storedKeysSelf struct {
	Name  string          `bson:"name"`
	Child *storedKeysSelf `bson:"child"`
}

type storedKeysOpaque struct{ value string }

func (o *storedKeysOpaque) UnmarshalBSON([]byte) error { return nil }

type storedKeysDoc struct {
	ID       int64                       `bson:"_id"`
	Schema   uint32                      `bson:"_schema"`
	Gold     int64                       `bson:"gold"`
	Equips   map[int64]*storedKeysEquip  `bson:"equips"`
	Main     storedKeysEquip             `bson:"main"`
	Self     storedKeysSelf              `bson:"self"`
	At       time.Time                   `bson:"at"`
	Opaque   storedKeysOpaque            `bson:"opaque"`
	Free     map[string]any              `bson:"free"`
	Blob     []byte                      `bson:"blob"`
	Untagged int32                       // bson 缺省键：小写字段名
	Tags     map[string]storedKeysInline `bson:"tags"`
}

type storedKeysInline struct {
	Kind  string         `bson:"kind"`
	Extra map[string]any `bson:",inline"`
}

func storedKeysRaw(t *testing.T, doc bson.D) []byte {
	t.Helper()
	raw, err := bson.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// 键集合从文档结构体按 bson 解码器的规则推出：已知键、缺失键、顶层元数据不报；未知键按解码层级报出，map 值与切片
// 元素用 * 代替真实键；自定义解码、time.Time、interface 值、inline map 不深入。
func TestStoredKeySetReportsKeysTheDocumentStructDoesNotDeclare(t *testing.T) {
	set := NewStoredKeySet[storedKeysDoc]("WalletDao")
	raw := storedKeysRaw(t, bson.D{
		{Key: "_id", Value: int64(7)}, {Key: "_schema", Value: int32(1)}, {Key: "_version", Value: int64(3)}, {Key: "_last_tx", Value: "tx"},
		{Key: "gold", Value: int64(1)},
		{Key: "coins", Value: int64(40)},
		{Key: "equips", Value: bson.D{
			{Key: "1", Value: bson.D{{Key: "level", Value: int32(1)}, {Key: "color", Value: "red"},
				{Key: "gems", Value: bson.D{{Key: "9", Value: bson.D{{Key: "id", Value: int32(9)}, {Key: "shine", Value: true}}}}}}},
			{Key: "2", Value: nil},
		}},
		{Key: "main", Value: bson.D{{Key: "runes", Value: bson.A{bson.D{{Key: "id", Value: int32(1)}, {Key: "old", Value: int32(1)}}}}}},
		{Key: "self", Value: bson.D{{Key: "child", Value: bson.D{{Key: "child", Value: bson.D{{Key: "deep", Value: int32(1)}}}}}}},
		{Key: "at", Value: time.Unix(1, 0)},
		{Key: "opaque", Value: bson.D{{Key: "anything", Value: int32(1)}}},
		{Key: "free", Value: bson.D{{Key: "anything", Value: int32(1)}}},
		{Key: "blob", Value: []byte{1}},
		{Key: "untagged", Value: int32(1)},
		{Key: "tags", Value: bson.D{{Key: "a", Value: bson.D{{Key: "kind", Value: "x"}, {Key: "whatever", Value: int32(1)}}}}},
		{Key: "_nested_meta_is_top_level_only", Value: bson.D{{Key: "_x", Value: int32(1)}}},
	})
	got := set.UnknownKeys(raw)
	want := []string{"coins", "equips.*.color", "equips.*.gems.*.shine", "main.runes.*.old", "self.child.child.deep"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("unknown keys = %v, want %v", got, want)
	}
	nested := storedKeysRaw(t, bson.D{{Key: "main", Value: bson.D{{Key: "_level", Value: int32(1)}}}})
	if got := set.UnknownKeys(nested); !reflect.DeepEqual(got, []string{"main._level"}) {
		t.Fatalf("an _-prefixed key inside a sub-document is not metadata: %v", got)
	}
	if got := set.UnknownKeys([]byte{1, 2, 3}); len(got) != 0 {
		t.Fatalf("malformed BSON reported keys: %v", got)
	}
	truncated := raw[:len(raw)/2]
	_ = set.UnknownKeys(truncated) // 不 panic；随后的解码报真正的错误
}

// 同一进程同一 DAO 同一键只打一次 WARN，不按实体刷屏；日志带 DAO、键与第一次见到的实体 ID。
func TestStoredKeySetWarnsOncePerKey(t *testing.T) {
	var logs bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	defer slog.SetDefault(previous)

	set := NewStoredKeySet[storedKeysDoc]("WalletDao")
	for id := int64(1); id <= 5; id++ {
		set.WarnUnknown(storedKeysRaw(t, bson.D{{Key: "_id", Value: id}, {Key: "coins", Value: int64(40)}, {Key: "gold", Value: int64(1)}}))
	}
	set.WarnUnknown(storedKeysRaw(t, bson.D{{Key: "_id", Value: int64(9)}, {Key: "gold", Value: int64(1)}}))
	text := logs.String()
	if n := strings.Count(text, "key=coins"); n != 1 {
		t.Fatalf("coins warned %d times, want once:\n%s", n, text)
	}
	for _, want := range []string{"level=WARN", "dao=WalletDao", "first_id=1", "SCHEMA-MIGRATION"} {
		if !strings.Contains(text, want) {
			t.Errorf("warning lacks %q:\n%s", want, text)
		}
	}
	if strings.Contains(text, "key=gold") {
		t.Errorf("a declared key was reported:\n%s", text)
	}
}

type storedKeysCarrier struct {
	ID      int64             `bson:"_id"`
	Gold    int64             `bson:"gold"`
	Main    storedKeysEquip   `bson:"main"`
	Unknown UnknownStoredKeys `bson:",inline"`
}

// 收未知键的 inline UnknownStoredKeys 不声明任何键（否则未知键检查会被它整体关掉）；它让解码收下、编码写回
// 结构体之外的键，Persisted 去掉顶层框架元数据并复制值。Declares 与报告路径同形，清理入口据此拒绝删除在用的键。
func TestUnknownStoredKeysAreCarriedButNotDeclared(t *testing.T) {
	set := NewStoredKeySet[storedKeysCarrier]("CarrierDao")
	raw := storedKeysRaw(t, bson.D{{Key: "_id", Value: int64(1)}, {Key: "_version", Value: int64(4)}, {Key: "gold", Value: int64(2)}, {Key: "old_gold", Value: int64(40)}})
	if got := set.UnknownKeys(raw); !reflect.DeepEqual(got, []string{"old_gold"}) {
		t.Fatalf("unknown keys = %v, want [old_gold]", got)
	}
	var doc storedKeysCarrier
	if err := bson.Unmarshal(raw, &doc); err != nil {
		t.Fatal(err)
	}
	kept := doc.Unknown.Persisted()
	if len(kept) != 1 || kept["old_gold"].Int64() != 40 {
		t.Fatalf("kept = %v, want only old_gold", kept)
	}
	raw[len(raw)-2] ^= 0xff // 改坏解码缓冲区：保留下来的值不能引用它
	if kept["old_gold"].Int64() != 40 {
		t.Fatal("Persisted aliases the decode buffer")
	}
	out, err := bson.Marshal(storedKeysCarrier{ID: 1, Gold: 3, Unknown: kept})
	if err != nil {
		t.Fatal(err)
	}
	if v, err := bson.Raw(out).LookupErr("old_gold"); err != nil || v.Int64() != 40 {
		t.Fatalf("encoding dropped the carried key: %s", bson.Raw(out))
	}
	for path, want := range map[string]bool{
		"gold": true, "main": true, "main.level": true, "main.gems.*.id": true, "main.runes.*.level": true,
		"old_gold": false, "main.color": false, "main.gems.*.shine": false, "main.gems.id": false, "gold.x": false, "": false,
	} {
		if got := set.Declares(path); got != want {
			t.Errorf("Declares(%q) = %v, want %v", path, got, want)
		}
	}
}
