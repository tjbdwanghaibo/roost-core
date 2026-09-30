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

func TestRunRetiresHandlersButKeepsEmptyBootstrap(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	def := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), testProtocolDef)
	bind := filepath.Join(root, "protocol", "player_bind")
	handlers := filepath.Join(root, "game", "protocol_handlers")
	bootstrap := filepath.Join(root, "game", "protocol_bootstrap", "protocol_gen.go")
	args := []string{"-def", def, "-proto", filepath.Join(root, "protocol", "proto"), "-pb", filepath.Join(root, "protocol", "pb"), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", filepath.Join(root, "protocol", "protocol_manifest.json"), "-bind", bind, "-handlers", handlers, "-handler-bootstrap", bootstrap, "-robot-protocol", ""}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), "package protocoldef\n")
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{filepath.Join(bind, "bind_gen.go"), filepath.Join(handlers, "ping", "protocol_gen.go")} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("retired output remains %s: %v", path, err)
		}
	}
	raw, err := os.ReadFile(bootstrap)
	if err != nil || strings.Contains(string(raw), "pingprotocol") {
		t.Fatalf("empty bootstrap was not updated: %v\n%s", err, raw)
	}
}

func TestRunPreservesUnrecognizedFilesWhenDefinitionsAreEmpty(t *testing.T) {
	root := t.TempDir()
	def := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), "package protocoldef\n")
	proto := filepath.Join(root, "protocol", "proto", "protocol.proto")
	pb := filepath.Join(root, "protocol", "pb", "protocol.pb.go")
	manifest := filepath.Join(root, "protocol", "protocol_manifest.json")
	handler := filepath.Join(root, "game", "protocol_handlers", "ping", "protocol_gen.go")
	want := map[string]string{proto: "syntax = \"proto3\";\n// hand written\n", pb: "package pb\n", manifest: "{\"owner\":\"human\"}\n", handler: "package ping\n"}
	for path, body := range want {
		writeProtocolTestFile(t, path, body)
	}
	args := []string{"-def", def, "-proto", filepath.Dir(proto), "-pb", filepath.Dir(pb), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", manifest, "-bind", "", "-handlers", filepath.Join(root, "game", "protocol_handlers"), "-robot-protocol", ""}
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

func TestRunRetiresOnlyRemovedHandlerDomain(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	def := filepath.Join(root, "protocol", "def")
	source := strings.Replace(testProtocolDef, "type GameProtocol interface {", "type PongRequest struct { Value int64 `pb:\"1\"` }\n"+
		"type PongResponse struct { Value int64 `pb:\"1\"` }\n"+
		"type GameProtocol interface {\n\t//roost:msg id=10002 tags=local handler=pong\n\tPong(PongRequest) PongResponse", 1)
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), source)
	handlers := filepath.Join(root, "game", "protocol_handlers")
	args := []string{"-def", def, "-proto", filepath.Join(root, "protocol", "proto"), "-pb", filepath.Join(root, "protocol", "pb"), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", filepath.Join(root, "protocol", "protocol_manifest.json"), "-bind", filepath.Join(root, "protocol", "player_bind"), "-handlers", handlers, "-robot-protocol", ""}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	source = strings.Replace(source, "\t//roost:msg id=10002 tags=local handler=pong\n\tPong(PongRequest) PongResponse\n", "", 1)
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), source)
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(handlers, "pong", "protocol_gen.go")); !os.IsNotExist(err) {
		t.Fatalf("retired pong handler remains: %v", err)
	}
	if _, err := os.Stat(filepath.Join(handlers, "ping", "protocol_gen.go")); err != nil {
		t.Fatalf("live ping handler was removed: %v", err)
	}
}

func TestRunDoesNotRetireOutputsBeforeEmptyBootstrapIsValidated(t *testing.T) {
	root := t.TempDir()
	writeProtocolTestFile(t, filepath.Join(root, "go.mod"), "module example.com/game\n\ngo 1.26.5\n")
	def := filepath.Join(root, "protocol", "def")
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), testProtocolDef)
	proto := filepath.Join(root, "protocol", "proto", "protocol.proto")
	bootstrap := filepath.Join(root, "game", "protocol_bootstrap", "protocol_gen.go")
	args := []string{"-def", def, "-proto", filepath.Dir(proto), "-pb", filepath.Join(root, "protocol", "pb"), "-msgid", filepath.Join(root, "protocol", "msgid"), "-manifest", filepath.Join(root, "protocol", "protocol_manifest.json"), "-bind", filepath.Join(root, "protocol", "player_bind"), "-handlers", filepath.Join(root, "game", "protocol_handlers"), "-handler-bootstrap", bootstrap, "-robot-protocol", ""}
	if err := Run(args, io.Discard); err != nil {
		t.Fatal(err)
	}
	writeProtocolTestFile(t, filepath.Join(def, "game.go"), "package protocoldef\n")
	if err := os.Remove(filepath.Join(root, "go.mod")); err != nil {
		t.Fatal(err)
	}
	if err := Run(args, io.Discard); err == nil {
		t.Fatal("missing module should refuse empty bootstrap generation")
	}
	if _, err := os.Stat(proto); err != nil {
		t.Fatalf("failed generation removed previous proto: %v", err)
	}
}
