package protocol

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCSharpCLIIDsAndRetirement(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	def := filepath.Join(root, "protocol", "def")
	source := strings.Replace(testProtocolDef, "type GameProtocol interface {", "type GameProtocol interface {\n\t//roost:msg id=10002\n\tNotice() PingResponse", 1)
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), source)
	cs := filepath.Join(root, "client", "MessageIds.cs")
	proto := filepath.Join(root, "protocol", "proto")
	args := []string{"-def", def, "-proto", proto, "-pb", filepath.Join(root, "protocol", "pb"), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", filepath.Join(root, "protocol", "manifest.json"), "-bind", "", "-handlers", "", "-robot-protocol", "", "-csharp", cs}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(cs)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"RequestPing = 10001", "ResponsePing = 10001", "PushNotice = 10002"} {
		if !strings.Contains(string(raw), want) {
			t.Fatalf("missing %s: %s", want, raw)
		}
	}
	// Git autocrlf后的生成文件仍属于生成器，下一次导出不能误拒绝。
	writeProtocolTestFile(t, cs, strings.ReplaceAll(string(raw), "\n", "\r\n"))
	if err := Run(args, io.Discard); err != nil {
		t.Fatalf("regenerate CRLF C#: %v", err)
	}
	// 同一份定义的proto也必须保留业务字段，不把C#输出变成独立消息源。
	before, err := os.ReadFile(filepath.Join(proto, "protocol.proto"))
	if err != nil || !strings.Contains(string(before), "int64 client_time = 1") {
		t.Fatalf("proto = %s, err %v", before, err)
	}
	manual := "// application-owned\nnamespace Mine {}\n"
	writeProtocolTestFile(t, cs, manual)
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), strings.Replace(source, "id=10001", "id=20001", 1))
	if err := Run(append(args, "-force"), io.Discard); err == nil {
		t.Fatal("force replaced application-owned C# output")
	}
	after, _ := os.ReadFile(filepath.Join(proto, "protocol.proto"))
	if string(before) != string(after) {
		t.Fatal("refused C# write changed the proto first")
	}
	actual, _ := os.ReadFile(cs)
	if string(actual) != manual {
		t.Fatal("application C# changed")
	}
	// 空定义退役仅删除自己的生成文件；手写文件保留。
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), "package protocoldef\n")
	other := filepath.Join(root, "client", "other_gen.go")
	writeProtocolTestFile(t, other, protocolGeneratedHeader+"\npackage other\n")
	wrong := append([]string(nil), args...)
	wrong[len(wrong)-1] = other
	if err := Run(wrong, io.Discard); err == nil {
		t.Fatal("C# retirement accepted a Go output path")
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("C# retirement changed another Go output: %v", err)
	}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	actual, _ = os.ReadFile(cs)
	if string(actual) != manual {
		t.Fatal("retirement removed application C#")
	}
	writeProtocolTestFile(t, cs, string(raw))
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cs); !os.IsNotExist(err) {
		t.Fatalf("generated C# not retired: %v", err)
	}
}
