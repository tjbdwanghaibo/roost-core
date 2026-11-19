package physicsroom

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"os"
	"reflect"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/gameplay/level"
	"google.golang.org/protobuf/encoding/protowire"
)

func goldenLevelView() level.View {
	return level.View{Instance: "room-0011223344556677", Epoch: 9, Revision: 300, Phase: level.Finishing, Result: "clear",
		Stages:    []level.StageView{{ID: "finale", Generation: 2}, {ID: "run", Generation: 1}},
		Variables: map[string]level.Value{"laps": level.Int(-3), "open": level.Bool(false), "title": level.String("第一关"), "zero": level.Int(0)}}
}

// 流程视图的字节与 C# 客户端共用一份金样（client/physics-viewer/tests/packed.json 的 LevelView）。
func TestLevelViewGoldenSharedWithCSharp(t *testing.T) {
	raw, err := os.ReadFile("../../client/physics-viewer/tests/packed.json")
	if err != nil {
		t.Fatal(err)
	}
	var golden struct{ LevelView string }
	if err = json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}
	want, err := base64.StdEncoding.DecodeString(golden.LevelView)
	if err != nil || len(want) == 0 {
		t.Fatalf("golden missing: %v", err)
	}
	got, err := EncodeLevelView(goldenLevelView())
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("level view differs from the shared golden:\n got %s\nwant %s", base64.StdEncoding.EncodeToString(got), golden.LevelView)
	}
	view, err := DecodeLevelView(want)
	source := goldenLevelView()
	if err != nil || view.Revision != 300 || view.Phase != level.Finishing || view.Result != "clear" ||
		!reflect.DeepEqual(view.Stages, source.Stages) || !reflect.DeepEqual(view.Variables, source.Variables) {
		t.Fatalf("decoded %+v %v", view, err)
	}
	// 实例名与 Epoch 不下发。
	if bytes.Contains(want, []byte("room-0011223344556677")) {
		t.Fatal("instance name leaked into the client view")
	}
}

func TestLevelViewEncodingIsDeterministicAndBounded(t *testing.T) {
	a := level.View{Revision: 1, Phase: level.Running, Variables: map[string]level.Value{}}
	b := level.View{Revision: 1, Phase: level.Running, Variables: map[string]level.Value{}}
	keys := []string{"m", "a", "z", "k", "b", "y", "c", "x"}
	for i, key := range keys {
		a.Variables[key] = level.Int(int64(i))
		b.Variables[keys[len(keys)-1-i]] = level.Int(int64(len(keys) - 1 - i))
	}
	first, err := EncodeLevelView(a)
	if err != nil {
		t.Fatal(err)
	}
	for range 20 {
		if again, _ := EncodeLevelView(b); !bytes.Equal(first, again) {
			t.Fatal("encoding depends on map order")
		}
	}
	// 空视图只有版本与阶段；零值变量仍然写出类型。
	empty, err := EncodeLevelView(level.View{Phase: level.Preparing})
	if err != nil {
		t.Fatal(err)
	}
	if view, err := DecodeLevelView(empty); err != nil || view.Revision != 0 || view.Phase != level.Preparing || view.Result != "" || len(view.Stages) != 0 || len(view.Variables) != 0 {
		t.Fatalf("empty %+v %v", view, err)
	}
	typed, _ := EncodeLevelView(level.View{Phase: level.Running, Variables: map[string]level.Value{"i": level.Int(0), "b": level.Bool(false), "s": level.String("")}})
	view, err := DecodeLevelView(typed)
	if err != nil || view.Variables["i"] != level.Int(0) || view.Variables["b"] != level.Bool(false) || view.Variables["s"] != level.String("") {
		t.Fatalf("zero values lost their type: %+v %v", view.Variables, err)
	}
	// 上限：刚好到上限可以，多一个字节拒绝；未知类型的变量拒绝。
	sized := func(n int) level.View {
		return level.View{Phase: level.Running, Variables: map[string]level.Value{"k": level.String(strings.Repeat("x", n))}}
	}
	n := MaxLevelViewBytes - 64
	for {
		out, err := EncodeLevelView(sized(n + 1))
		if err != nil {
			break
		}
		if len(out) > MaxLevelViewBytes {
			t.Fatalf("encoded %d bytes above the limit", len(out))
		}
		n++
	}
	exact, err := EncodeLevelView(sized(n))
	if err != nil || len(exact) != MaxLevelViewBytes {
		t.Fatalf("largest accepted view is %d bytes (%v), want exactly %d", len(exact), err, MaxLevelViewBytes)
	}
	if _, err = DecodeLevelView(exact); err != nil {
		t.Fatalf("view of exactly the limit rejected: %v", err)
	}
	if _, err = EncodeLevelView(level.View{Phase: level.Running, Variables: map[string]level.Value{"k": {}}}); err == nil {
		t.Fatal("variable of unknown kind encoded")
	}
}

func TestDecodeLevelViewRejectsMalformedInput(t *testing.T) {
	tag := func(b []byte, n protowire.Number, typ protowire.Type) []byte { return protowire.AppendTag(b, n, typ) }
	str := func(b []byte, n protowire.Number, v string) []byte {
		return protowire.AppendString(tag(b, n, protowire.BytesType), v)
	}
	num := func(b []byte, n protowire.Number, v uint64) []byte {
		return protowire.AppendVarint(tag(b, n, protowire.VarintType), v)
	}
	sub := func(b []byte, n protowire.Number, v []byte) []byte {
		return protowire.AppendBytes(tag(b, n, protowire.BytesType), v)
	}
	phase := str(nil, 2, "Running")
	if _, err := DecodeLevelView(phase); err != nil {
		t.Fatalf("minimal view rejected: %v", err)
	}
	for name, data := range map[string][]byte{
		"empty":                    nil,
		"no phase":                 num(nil, 1, 4),
		"unknown field":            num(bytes.Clone(phase), 6, 1),
		"unknown bytes field":      str(bytes.Clone(phase), 6, "x"),
		"unknown nested message":   sub(bytes.Clone(phase), 7, str(nil, 1, "a")),
		"field zero":               {0x00, 0x00},
		"revision as bytes":        str(bytes.Clone(phase), 1, "x"),
		"phase as varint":          num(nil, 2, 1),
		"revision twice":           num(num(bytes.Clone(phase), 1, 1), 1, 2),
		"phase twice":              str(bytes.Clone(phase), 2, "Closed"),
		"result twice":             str(str(bytes.Clone(phase), 3, "a"), 3, "b"),
		"truncated":                append(bytes.Clone(phase), 0x22, 0x05, 0x0a),
		"stage without id":         sub(bytes.Clone(phase), 4, num(nil, 2, 1)),
		"stage unknown field":      sub(bytes.Clone(phase), 4, num(str(nil, 1, "a"), 3, 1)),
		"stage id twice":           sub(bytes.Clone(phase), 4, str(str(nil, 1, "a"), 1, "b")),
		"stage generation as text": sub(bytes.Clone(phase), 4, str(str(nil, 1, "a"), 2, "x")),
		"stage repeated":           sub(sub(bytes.Clone(phase), 4, str(nil, 1, "a")), 4, str(nil, 1, "a")),
		"variable without key":     sub(bytes.Clone(phase), 5, num(nil, 2, 1)),
		"variable without value":   sub(bytes.Clone(phase), 5, str(nil, 1, "k")),
		"variable with two values": sub(bytes.Clone(phase), 5, num(num(str(nil, 1, "k"), 2, 1), 3, 1)),
		"variable int as text":     sub(bytes.Clone(phase), 5, str(str(nil, 1, "k"), 2, "x")),
		"variable flag of 2":       sub(bytes.Clone(phase), 5, num(str(nil, 1, "k"), 3, 2)),
		"variable unknown field":   sub(bytes.Clone(phase), 5, num(num(str(nil, 1, "k"), 2, 1), 5, 1)),
		"variable repeated":        sub(sub(bytes.Clone(phase), 5, num(str(nil, 1, "k"), 2, 1)), 5, num(str(nil, 1, "k"), 2, 2)),
		"above the byte limit":     str(bytes.Clone(phase), 3, strings.Repeat("x", MaxLevelViewBytes)),
	} {
		if view, err := DecodeLevelView(data); err == nil {
			t.Errorf("%s: accepted as %+v", name, view)
		}
	}
}
