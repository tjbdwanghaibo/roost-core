package protocol

import (
	"path/filepath"
	"strings"
	"testing"
)

// The rules U-0032 pinned were the structural ones; these are the value and
// reference rules: enum values must be int32 integer literals, every field
// carries a pb number, oneof names are identifiers, a message's request and
// response structs must exist, and an enum type is declared once across the
// definition directory.
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
