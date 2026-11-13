package protocol

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunRetiresOutputsWhenLastDefinitionIsRemoved(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	def := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), testProtocolDef)
	proto := filepath.Join(root, "protocol", "proto", "protocol.proto")
	pb := filepath.Join(root, "protocol", "pb", "protocol.pb.go")
	msgid := filepath.Join(root, "protocol", "msgid", "msgid_gen.go")
	manifest := filepath.Join(root, "protocol", "protocol_manifest.json")
	args := []string{"-def", def, "-proto", filepath.Dir(proto), "-pb", filepath.Dir(pb), "-msgid", filepath.Dir(msgid), "-manifest", manifest, "-bind", "", "-handlers", "", "-robot-protocol", ""}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), "package protocoldef\n")
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{proto, pb, msgid, manifest} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("retired output remains %s: %v", path, err)
		}
	}
}

func TestRunRetiresBindingsButKeepsEmptyRegistration(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	def := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), testProtocolDef)
	bind := filepath.Join(root, "protocol", "player_bind")
	registration := filepath.Join(root, "internal", "access", "player", "protocols_gen.go")
	args := []string{"-def", def, "-proto", filepath.Join(root, "protocol", "proto"), "-pb", filepath.Join(root, "protocol", "pb"), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", filepath.Join(root, "protocol", "protocol_manifest.json"), "-bind", bind, "-handlers", registration, "-robot-protocol", ""}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if raw, err := os.ReadFile(registration); err != nil || !strings.Contains(string(raw), "RegisterPingPlayerProtocols(protocols, registry)") {
		t.Fatalf("registration lacks the ping domain: %v\n%s", err, raw)
	}
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), "package protocoldef\n")
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(bind, "bind_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("retired binding remains: %v", err)
	}
	raw, err := os.ReadFile(registration)
	if err != nil || strings.Contains(string(raw), "Ping") || !strings.Contains(string(raw), "func RegisterPlayerProtocols(") {
		t.Fatalf("empty registration was not updated: %v\n%s", err, raw)
	}
}

func TestRunPreservesUnrecognizedFilesWhenDefinitionsAreEmpty(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), "package protocoldef\n")
	proto := filepath.Join(root, "protocol", "proto", "protocol.proto")
	pb := filepath.Join(root, "protocol", "pb", "protocol.pb.go")
	manifest := filepath.Join(root, "protocol", "protocol_manifest.json")
	want := map[string]string{proto: "syntax = \"proto3\";\n// hand written\n", pb: "package pb\n", manifest: "{\"owner\":\"human\"}\n"}
	for path, body := range want {
		writeProtocolTestFile(t, path, body)
	}
	args := []string{"-def", def, "-proto", filepath.Dir(proto), "-pb", filepath.Dir(pb), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", manifest, "-bind", "", "-robot-protocol", ""}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	for path, body := range want {
		raw, err := os.ReadFile(path)
		if err != nil || string(raw) != body {
			t.Fatalf("hand-written %s changed: %v\n%s", path, err, raw)
		}
	}
}

func TestRunDropsRemovedHandlerDomainFromRegistration(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	def := filepath.Join(root, "protocol", "def")
	source := strings.Replace(testProtocolDef, "type GameProtocol interface {", "type PongRequest struct { Value int64 `pb:\"1\"` }\n"+
		"type PongResponse struct { Value int64 `pb:\"1\"` }\n"+
		"type GameProtocol interface {\n\t//roost:msg id=10002 tags=local handler=pong\n\tPong(PongRequest) PongResponse", 1)
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), source)
	registration := filepath.Join(root, "internal", "access", "player", "protocols_gen.go")
	args := []string{"-def", def, "-proto", filepath.Join(root, "protocol", "proto"), "-pb", filepath.Join(root, "protocol", "pb"), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", filepath.Join(root, "protocol", "protocol_manifest.json"), "-bind", filepath.Join(root, "protocol", "player_bind"), "-handlers", registration, "-robot-protocol", ""}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, "\t//roost:msg id=10002 tags=local handler=pong\n\tPong(PongRequest) PongResponse\n", "", 1)
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), source)
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(registration)
	if err != nil || strings.Contains(string(raw), "PongController") || !strings.Contains(string(raw), "PingController") {
		t.Fatalf("registration after dropping pong: %v\n%s", err, raw)
	}
}
