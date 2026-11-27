package roost

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/codegen/internal/dao"
	"google.golang.org/protobuf/encoding/prototext"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/types/dynamicpb"
)

func daoTestProtoc(t *testing.T) string {
	t.Helper()
	path := os.Getenv("ROOST_PROTOC")
	if path == "" {
		path = "protoc"
	}
	bin, err := exec.LookPath(path)
	if err != nil {
		t.Skipf("install protoc to exercise DAO schema release checks: %v", err)
	}
	bin, err = filepath.Abs(bin)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("ROOST_PROTOC", bin)
	return bin
}

func daoTestGenerate(t *testing.T, fields, nested []string) string {
	t.Helper()
	def, out := t.TempDir(), t.TempDir()
	var source strings.Builder
	source.WriteString("package def\n\n//roost:dao coll=probes db=game dbscope=sid\ntype ProbeDao struct {\n")
	// 规格为 "Name" 或 "Name Type"；不写类型时 Detail 是 Pair，其余是 int32。
	field := func(spec string) {
		name, typ, explicit := strings.Cut(spec, " ")
		if !explicit {
			typ = "int32"
			if name == "Detail" {
				typ = "Pair"
			}
		}
		fmt.Fprintf(&source, "%s %s `store:%q`\n", name, typ, strings.ToLower(name))
	}
	for _, name := range fields {
		field(name)
	}
	source.WriteString("}\ntype Pair struct {\n")
	for _, name := range nested {
		field(name)
	}
	source.WriteString("}\n")
	if err := os.WriteFile(filepath.Join(def, "probe.go"), []byte(source.String()), 0600); err != nil {
		t.Fatal(err)
	}
	if err := dao.Run([]string{"-def", def, "-out", out, "-pkg", "db"}, io.Discard); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(out, "dao_sync.proto")
}

func daoTestVerify(previous, current string) (string, error) {
	var out bytes.Buffer
	err := Run([]string{"framework", "verify-dao-sync", "--previous", previous, "--current", current}, &out, io.Discard)
	return out.String(), err
}

// 两份 schema 均从各自的真实 DAO 定义生成；以新描述符编码，用旧描述符读相同的二进制包。
// 不能仅断言解码成功：字段换号时 protobuf 合法解码，值却到了另一个字段。
func TestDAOSyncReleaseGeneratedVersions(t *testing.T) {
	bin := daoTestProtoc(t)
	baseFields := []string{"Gold", "Silver", "Detail"}
	baseNested := []string{"Left", "Right"}
	previous := daoTestGenerate(t, baseFields, baseNested)
	old, err := readDAOSyncSchema(t.Context(), bin, previous)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name           string
		fields, nested []string
		wantIssues     []string
		gold, left     int64
	}{
		{"top-level reorder", []string{"Silver", "Gold", "Detail"}, baseNested, []string{"SyncProbeDao #2 (gold): number now belongs to silver"}, 22, 31},
		{"nested reorder", baseFields, []string{"Right", "Left"}, []string{"SyncPair #1 (left): number now belongs to right", "SyncPatchPair #1 (left): number now belongs to right"}, 11, 42},
		{"append both", []string{"Gold", "Silver", "Detail", "Extra"}, []string{"Left", "Right", "Bonus"}, nil, 11, 31},
	} {
		t.Run(tc.name, func(t *testing.T) {
			current := daoTestGenerate(t, tc.fields, tc.nested)
			out, err := daoTestVerify(previous, current)
			if len(tc.wantIssues) == 0 {
				if err != nil || !strings.Contains(out, "3 appended field(s)") {
					t.Fatalf("expected append (top, nested value and patch): %q %v", out, err)
				}
			} else {
				for _, want := range tc.wantIssues {
					if err == nil || !strings.Contains(err.Error(), want) {
						t.Fatalf("missing %q in %v", want, err)
					}
				}
			}
			next, err := readDAOSyncSchema(t.Context(), bin, current)
			if err != nil {
				t.Fatal(err)
			}
			for _, patch := range []bool{false, true} {
				top := "entity_id: 7 sync_format: 3 gold { value: 11 } silver { value: 22 }"
				inner := "value { left: 31 right: 42"
				if patch {
					inner = "patch { left { value: 31 } right { value: 42 }"
				}
				if len(tc.wantIssues) == 0 {
					top += " extra { value: 88 }"
					if patch {
						inner += " bonus { value: 99 }"
					} else {
						inner += " bonus: 99"
					}
				}
				fresh := dynamicpb.NewMessage(next.Messages().ByName("SyncProbeDao"))
				if err := prototext.Unmarshal([]byte(top+" detail { "+inner+" } }"), fresh); err != nil {
					t.Fatal(err)
				}
				raw, err := proto.Marshal(fresh)
				if err != nil {
					t.Fatal(err)
				}
				stale := dynamicpb.NewMessage(old.Messages().ByName("SyncProbeDao"))
				if err := proto.Unmarshal(raw, stale); err != nil {
					t.Fatal(err)
				}
				get := func(m protoreflect.Message, name protoreflect.Name) protoreflect.Value {
					return m.Get(m.Descriptor().Fields().ByName(name))
				}
				gold := get(get(stale, "gold").Message(), "value").Int()
				detail := get(stale, "detail").Message()
				var left int64
				if patch {
					left = get(get(get(detail, "patch").Message(), "left").Message(), "value").Int()
				} else {
					left = get(get(detail, "value").Message(), "left").Int()
				}
				if gold != tc.gold || left != tc.left {
					t.Errorf("old reader (patch=%t): gold=%d, left=%d; want %d, %d", patch, gold, left, tc.gold, tc.left)
				}
			}
		})
	}
}

// map 值为切片时 protobuf 需要包装消息；它的名字必须只由元素类型决定，
// 否则前面多生成一个消息就会让已发布的包装消息“改名”，合法的追加被门禁拒绝。
func TestDAOSyncReleaseMapOfSliceWrappers(t *testing.T) {
	daoTestProtoc(t)
	bag := "Bag map[string][]int32"
	for _, tc := range []struct {
		name                           string
		prevFields, prevNested, fields []string
		nested                         []string
		want                           string
	}{
		{"append after map of slice", []string{"Gold", bag}, []string{"Left"}, []string{"Gold", bag, "Detail"}, []string{"Left"}, ""},
		{"earlier nested type gains map of slice", []string{"Gold", bag, "Detail"}, []string{"Left"}, []string{"Gold", bag, "Detail"}, []string{"Left", "Tags map[string][]string", "Codes map[int32][]int32"}, ""},
		{"element type change", []string{"Gold", bag}, []string{"Left"}, []string{"Gold", "Bag map[string][]int64"}, []string{"Left"},
			"SyncProbeDaoBagField #1 (value): map<string,roost.dao.sync.SyncListInt32> changed to map<string,roost.dao.sync.SyncListInt64>"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			previous := daoTestGenerate(t, tc.prevFields, tc.prevNested)
			current := daoTestGenerate(t, tc.fields, tc.nested)
			out, err := daoTestVerify(previous, current)
			if tc.want == "" {
				if err != nil || !strings.Contains(out, "compatible:") {
					t.Fatalf("expected compatible: %q %v", out, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}

func TestDAOSyncReleasePolicy(t *testing.T) {
	daoTestProtoc(t)
	write := func(body string) string {
		p := filepath.Join(t.TempDir(), "dao_sync.proto")
		text := `syntax="proto3"; package roost.dao.sync; message SyncTestDao { int64 entity_id=1; uint32 sync_format=2047; ` + body + "}"
		if err := os.WriteFile(p, []byte(text), 0600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, tc := range []struct{ name, previous, current, want string }{
		{"text reorder", "int32 a=2; int32 b=3;", "int32 b=3; int32 a=2;", ""},
		{"rename", "int32 a=2;", "int32 renamed=2;", "#2 (a): number now belongs to renamed"},
		{"renumber", "int32 a=2;", "int32 a=3;", "#2 (a): released field removed or renumbered"},
		{"delete", "int32 a=2;", "", "released field removed"},
		{"delete with tombstones", "int32 a=2;", `reserved 2; reserved "a";`, "released field removed"},
		{"fill hole", "int32 a=4;", "int32 b=3; int32 a=4;", "not an append"},
		{"type", "int32 a=2;", "sint32 a=2;", "int32 changed to sint32"},
		{"map value", "map<string,int32> a=2;", "map<string,uint32> a=2;", "map<string,int32> changed to map<string,uint32>"},
		{"cardinality", "repeated int32 a=2;", "int32 a=2;", "changed to int32"},
		{"reserved split", `reserved 9 to 12; reserved "dead";`, `reserved 11 to 12, 9 to 10; reserved "dead";`, ""},
		{"reserved number reused", "reserved 9 to 12;", "reserved 9 to 11; int32 new=12;", "reserved range 9..12 no longer fully reserved"},
		{"reserved name lost", `reserved "dead";`, "int32 dead=14;", "reserved name dead no longer reserved"},
		{"invalid candidate", "int32 a=2;", "int32 a=2; int32 b=2;", "protoc:"},
		{"invalid baseline", "not a proto", "int32 a=2;", "previous"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			out, err := daoTestVerify(write(tc.previous), write(tc.current))
			if tc.want == "" {
				if err != nil || !strings.Contains(out, "compatible:") {
					t.Fatalf("expected compatible: %q %v", out, err)
				}
			} else if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("expected %q, got %v", tc.want, err)
			}
		})
	}
}
