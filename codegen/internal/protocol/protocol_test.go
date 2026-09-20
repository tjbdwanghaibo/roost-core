package protocol

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testProtocolDef = "package protocoldef\n\n" +
	"//roost:proto package=test.protocol go_package=example.com/game/protocol/pb;pb\n\n" +
	"type PingRequest struct {\n\tClientTime int64 " + "\x60pb:\"1\"\x60" + "\n}\n\n" +
	"type PingResponse struct {\n\tServerTime int64 " + "\x60pb:\"1\"\x60" + "\n}\n\n" +
	"type GameProtocol interface {\n" +
	"\t//roost:msg id=10001 tags=local handler=ping\n" +
	"\tPing(PingRequest) PingResponse\n}\n"

func TestParseAndGenerateProtocol(t *testing.T) {
	root := t.TempDir()
	defDir := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(defDir, "game.go"), testProtocolDef)

	defs, err := parseDefDir(defDir)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(defs.Messages) != 1 || defs.Messages[0].Name != "Ping" || defs.Messages[0].ReqID != 10001 {
		t.Fatalf("messages = %#v", defs.Messages)
	}
	proto, err := generateProto(defs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(proto, []byte("message PingRequest")) {
		t.Fatalf("unexpected proto:\n%s", proto)
	}
	pb, err := generatePBGo(defs)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(pb, []byte("type PingRequest struct")) {
		t.Fatalf("unexpected pb:\n%s", pb)
	}
}

func TestRunDiscoversTargetModule(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	defDir := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(defDir, "game.go"), testProtocolDef)
	var stdout bytes.Buffer
	err := Run([]string{
		"-def", defDir,
		"-proto", filepath.Join(root, "protocol", "proto"),
		"-pb", filepath.Join(root, "protocol", "pb"),
		"-msgid", filepath.Join(root, "protocol", "msgid"),
		"-bind", "",
		"-handlers", "",
		"-robot-protocol", "",
		"-manifest", filepath.Join(root, "protocol", "manifest.json"),
		"-force",
	}, &stdout)
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(root, "protocol", "proto", "protocol.proto"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "example.com/game/protocol/pb;pb") {
		t.Fatalf("target module not used:\n%s", raw)
	}
}

func TestDuplicateMessageIDRejected(t *testing.T) {
	root := t.TempDir()
	defDir := filepath.Join(root, "protocol", "def")
	duplicate := strings.Replace(testProtocolDef, "type GameProtocol interface {", "type GameProtocol interface {\n\t//roost:msg id=10001\n\tOther(PingRequest) PingResponse", 1)
	writeProtocolTestFile(t, filepath.Join(defDir, "game.go"), duplicate)
	if _, err := parseDefDir(defDir); err == nil {
		t.Fatal("expected duplicate message ID error")
	}
}

func writeProtocolTestFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
