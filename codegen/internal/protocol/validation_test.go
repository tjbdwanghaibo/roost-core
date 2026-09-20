package protocol

import (
	"path/filepath"
	"strings"
	"testing"
)

// Every structural rule the parser enforces used to be enforced by nothing
// else: a temporary revert of each check left the suite green (U-0032). Each
// case takes the valid fixture and breaks exactly one rule.
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
