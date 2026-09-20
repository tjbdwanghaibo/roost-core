package protocol

import (
	"path/filepath"
	"strings"
	"testing"
)

// U-0115 · C2（空洞测试）· nightly gap map `internal/protocol` 8/20。
//
// 协议定义解析是生成物正确性的第一道门：id 不是数字、结果类型不是命名类型、
// handler 名不合法、通知消息引用不存在的结构体，都必须在解析时点名拒绝；
// 定义层面的两条不变量（req / resp 共用 id、枚举名唯一且有值）与生成 bootstrap
// 时"有控制器域就必须给 handler import base"也各自要有拒绝。

func TestParseRejectsEachMarkerViolation(t *testing.T) {
	cases := []struct{ label, source, want string }{
		{"non-numeric id", strings.Replace(testProtocolDef, "id=10001", "id=abc", 1), "has invalid id"},
		{"result type is not a named type", strings.Replace(testProtocolDef, "Ping(PingRequest) PingResponse", "Ping(PingRequest) []PingResponse", 1), "invalid result"},
		{"handler name is not snake_case", strings.Replace(testProtocolDef, "handler=ping", "handler=Ping", 1), "has invalid handler"},
		{"notify names a missing struct", strings.Replace(testProtocolDef, "\tPing(PingRequest) PingResponse\n", "\tPing(PingRequest) PingResponse\n\t//roost:msg id=10002 tags=local\n\tGhost() GhostPush\n", 1), "push struct GhostPush not found"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			if tc.source == testProtocolDef {
				t.Fatal("the case did not change the fixture")
			}
			defDir := filepath.Join(t.TempDir(), "protocol", "def")
			writeProtocolTestFile(t, filepath.Join(defDir, "game.go"), tc.source)
			_, err := parseDefDir(defDir)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseDefDir = %v, want %q", err, tc.want)
			}
		})
	}
}

func TestValidateDefinitionsEnforcesIDAndEnumInvariants(t *testing.T) {
	base := func() *Definitions {
		return &Definitions{
			Structs:  []StructDef{{Name: "A"}, {Name: "B"}},
			Messages: []MsgDef{{Name: "Op", Req: "A", ReqID: 1, Resp: "B", RespID: 1}},
			Enums:    []EnumDef{{Name: "Color", Values: []EnumValueDef{{Name: "ColorRed", Value: 0}}}},
		}
	}
	if err := validateDefinitions(base()); err != nil {
		t.Fatalf("baseline definitions rejected: %v", err)
	}
	split := base()
	split.Messages[0].RespID = 2
	if err := validateDefinitions(split); err == nil || !strings.Contains(err.Error(), "req id 1 must equal resp id 2") {
		t.Fatalf("split req/resp id = %v", err)
	}
	dup := base()
	dup.Enums = append(dup.Enums, dup.Enums[0])
	if err := validateDefinitions(dup); err == nil || !strings.Contains(err.Error(), "duplicate protocol enum Color") {
		t.Fatalf("duplicate enum = %v", err)
	}
	empty := base()
	empty.Enums = []EnumDef{{Name: "Empty"}}
	if err := validateDefinitions(empty); err == nil || !strings.Contains(err.Error(), "enum Empty has no values") {
		t.Fatalf("enum without values = %v", err)
	}
}

func TestBootstrapRequiresHandlerImportBaseWhenControllersExist(t *testing.T) {
	defDir := filepath.Join(t.TempDir(), "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(defDir, "game.go"), testProtocolDef)
	defs, err := parseDefDir(defDir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := generateProtocolBootstrap(defs, ""); err == nil || !strings.Contains(err.Error(), "handler import base is required for controller domains ping") {
		t.Fatalf("bootstrap without handler import base = %v", err)
	}
	if _, err := generateProtocolBootstrap(defs, "example.com/game/internal/handlers"); err != nil {
		t.Fatalf("bootstrap with handler import base = %v", err)
	}
	// 没有控制器域时不需要 import base。
	for i := range defs.Messages {
		defs.Messages[i].Handler = ""
	}
	if _, err := generateProtocolBootstrap(defs, ""); err != nil {
		t.Fatalf("bootstrap without controller domains = %v", err)
	}
}
