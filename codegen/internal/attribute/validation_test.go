package attribute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Every rule the profile parser enforces was enforced by nothing else: a
// temporary revert of each of the nine checks left the one existing test
// green (U-0034). Each case breaks one rule in an otherwise valid profile.
func TestParseDirRejectsEachProfileViolation(t *testing.T) {
	const head = "package attribute\n\n"
	cases := []struct{ label, source, want string }{
		{"more fields than max", head + "//roost:attribute index=1 max=1\ntype P struct {\n\tHP int64\n\tMP int64\n\tdirtyMask uint64\n}\n", "more than max=1"},
		{"duplicate field", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tHP int64\n\tdirtyMask uint64\n}\n", "duplicate field HP"},
		{"duplicate formula", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int64) int64 { return HP }\nfunc (p P) _Total(HP int64) int64 { return HP }\n", "formula duplicated"},
		{"two results", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int64) (int64, int64) { return HP, HP }\n", "must return exactly one value"},
		{"result type mismatch", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int64) int32 { return 0 }\n", "returns int32, want int64"},
		{"unknown input field", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(Nope int64) int64 { return Nope }\n", "references unknown field Nope"},
		{"input type mismatch", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int32) int64 { return int64(HP) }\n", "param HP has type int32, want int64"},
		{"no inputs", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total() int64 { return 1 }\n", "must reference at least one attribute field"},
		// The one field a declaration must carry itself (RR-20260917-06).
		{"no dirty mask", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n}\n", "must declare an unexported `dirtyMask uint64` field"},
		{"formula cycle", head + "//roost:attribute index=1 max=4\ntype P struct {\n\tA int64\n\tB int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _A(B int64) int64 { return B }\nfunc (p P) _B(A int64) int64 { return A }\n", "formula cycle"},
	}
	for _, testCase := range cases {
		t.Run(testCase.label, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "profile.go"), []byte(testCase.source), 0o644); err != nil {
				t.Fatal(err)
			}
			_, err := parseDir(dir)
			if err == nil {
				t.Fatalf("accepted a profile that breaks %q", testCase.label)
			}
			if !strings.Contains(err.Error(), testCase.want) {
				t.Fatalf("error %q does not contain %q", err, testCase.want)
			}
		})
	}
	// And the valid profile with a formula still parses, so the cases above
	// fail for the reason they name rather than for the fixture being broken.
	dir := t.TempDir()
	valid := head + "//roost:attribute index=1 max=4\ntype P struct {\n\tHP int64\n\tTotal int64\n\tdirtyMask uint64\n}\n\nfunc (p P) _Total(HP int64) int64 { return HP * 2 }\n"
	if err := os.WriteFile(filepath.Join(dir, "profile.go"), []byte(valid), 0o644); err != nil {
		t.Fatal(err)
	}
	profiles, err := parseDir(dir)
	if err != nil || len(profiles) != 1 {
		t.Fatalf("valid profile: profiles=%d err=%v", len(profiles), err)
	}
}
