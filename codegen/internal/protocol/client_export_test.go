package protocol

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// client_export 复用 -csharp 的生成函数：缺省命名空间时与 -csharp 的输出逐字节相同，清单与常量一一对应。
func TestExportClientIDsMatchesCSharpOutput(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	def := filepath.Join(root, "protocol", "def")
	source := strings.Replace(testProtocolDef, "type GameProtocol interface {", "type GameProtocol interface {\n\t//roost:msg id=10000\n\tNotice() PingResponse", 1)
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), source)
	cs := filepath.Join(root, "client", "MessageIds.cs")
	args := []string{"-def", def, "-proto", filepath.Join(root, "protocol", "proto"), "-pb", filepath.Join(root, "protocol", "pb"), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", filepath.Join(root, "protocol", "manifest.json"), "-bind", "", "-handlers", "", "-robot-protocol", "", "-csharp", cs}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile(cs)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := ExportClientIDs(def, DefaultCSharpIDsNamespace)
	if err != nil {
		t.Fatal(err)
	}
	if string(ids.CSharp) != string(want) {
		t.Fatalf("export MessageIds.cs differs from -csharp:\n%s\nwant:\n%s", ids.CSharp, want)
	}
	got := make([]string, 0, len(ids.Messages))
	for _, message := range ids.Messages {
		got = append(got, message.Direction+" "+message.Name+" "+message.Type)
	}
	// 按号排序：推送 10000 在前，同号的请求在应答前。
	if strings.Join(got, "|") != "push Notice PingResponse|request Ping PingRequest|response Ping PingResponse" {
		t.Fatalf("messages = %v", got)
	}
	if ids.Messages[1].ID != 10001 || ids.Messages[0].ID != 10000 {
		t.Fatalf("ids = %+v", ids.Messages)
	}
	named, err := ExportClientIDs(def, "Game.Protocol")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(named.CSharp), "\nnamespace Game.Protocol\n") || strings.Contains(string(named.CSharp), "Roost.Generated") {
		t.Fatalf("namespace not applied:\n%s", named.CSharp)
	}
	empty, err := ExportClientIDs(filepath.Join(root, "missing"), DefaultCSharpIDsNamespace)
	if err != nil || len(empty.Messages) != 0 || !strings.Contains(string(empty.CSharp), "public static class MessageIds") {
		t.Fatalf("missing def dir: %+v %v", empty, err)
	}
}
