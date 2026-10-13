package protocol

import (
	"path/filepath"
	"strings"
	"testing"
)

// 解析器的每条结构规则：每例拿合法夹具只破坏一条规则。
func TestParseRejectsEachStructuralViolation(t *testing.T) {
	cases := []struct {
		label, source, want string
	}{
		{"duplicate struct", testProtocolDef + "\ntype PingRequest struct {\n\tAgain int64 `pb:\"1\"`\n}\n", "duplicate protocol struct"},
		{"unexported protocol type", strings.ReplaceAll(testProtocolDef, "PingRequest", "pingRequest"), "must be exported"},
		{"duplicate field number", strings.Replace(testProtocolDef, "type PingRequest struct {\n\tClientTime int64 `pb:\"1\"`\n}", "type PingRequest struct {\n\tClientTime int64 `pb:\"1\"`\n\tExtra int64 `pb:\"1\"`\n}", 1), "field number 1 duplicated"},
		{"enum first value not zero", testProtocolDef + "\ntype Color int32\n\nconst (\n\tColorRed Color = 1\n\tColorBlue Color = 2\n)\n", "first value must be 0"},
		{"enum duplicate value name", testProtocolDef + "\ntype Color int32\n\nconst (\n\tColorRed Color = 0\n)\n\nconst (\n\tColorRed Color = 1\n)\n", "duplicate value name"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			if testCase.source == testProtocolDef {
				t.Fatal("the case did not change the fixture; the rule is not being exercised")
			}
			root := t.TempDir()
			defDir := filepath.Join(root, "protocol", "def")
			writeProtocolTestFile(t, filepath.Join(defDir, "game.go"), testCase.source)
			_, err := parseDefDir(defDir)
			if err == nil {
				t.Fatalf("accepted a definition that breaks %q", testCase.label)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not contain %q", err, testCase.want)
			}
		})
	}
}

// 值与引用规则：枚举值必须是 int32 整数字面量，每个字段带 pb 编号，oneof 名是标识符，消息的请求 / 响应结构体必须存在，
// 一个枚举类型在定义目录里只声明一次。
func TestParseRejectsEachValueAndReferenceViolation(t *testing.T) {
	cases := []struct{ label, source, want string }{
		{"enum value beyond int32", testProtocolDef + "\ntype Color int32\n\nconst (\n\tColorRed Color = 0\n\tColorHuge Color = 2147483648\n)\n", "out of int32 range"},
		{"enum value not an integer literal", testProtocolDef + "\ntype Color int32\n\nconst (\n\tColorRed Color = 0\n\tColorOdd Color = \"1\"\n)\n", "enum value must be integer literal"},
		{"field without pb tag", strings.Replace(testProtocolDef, "type PingRequest struct {\n\tClientTime int64 `pb:\"1\"`\n}", "type PingRequest struct {\n\tClientTime int64 `pb:\"1\"`\n\tUntagged int64\n}", 1), "PingRequest.Untagged missing valid pb tag"},
		{"oneof name is not an identifier", strings.Replace(testProtocolDef, "`pb:\"1\"`\n}\n\ntype PingResponse", "`pb:\"1,oneof=not valid\"`\n}\n\ntype PingResponse", 1), `invalid oneof name "not valid"`},
		{"request struct missing", strings.Replace(testProtocolDef, "Ping(PingRequest) PingResponse", "Ping(GhostRequest) PingResponse", 1), "message req struct GhostRequest not found"},
		{"response struct missing", strings.Replace(testProtocolDef, "Ping(PingRequest) PingResponse", "Ping(PingRequest) GhostResponse", 1), "message resp struct GhostResponse not found"},
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
	// The same enum declared in two files of the directory is one enum too many.
	defDir := filepath.Join(t.TempDir(), "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(defDir, "game.go"), testProtocolDef+"\ntype Color int32\n\nconst (\n\tColorRed Color = 0\n)\n")
	writeProtocolTestFile(t, filepath.Join(defDir, "more.go"), "package protocoldef\n\ntype Color int32\n\nconst (\n\tColorGreen Color = 0\n)\n")
	if _, err := parseDefDir(defDir); err == nil || !strings.Contains(err.Error(), "duplicate protocol enum Color") {
		t.Fatalf("enum declared twice = %v", err)
	}
}

// 协议定义解析是生成物正确性的第一道门：id 不是数字、结果类型不是命名类型、handler 名不合法、通知消息引用不存在的结构体，
// 都必须在解析时点名拒绝；定义层面的两条不变量（req / resp 共用 id、枚举名唯一且有值）与生成 bootstrap 时
// “有控制器域就必须给 handler import base”也各自要有拒绝。

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
