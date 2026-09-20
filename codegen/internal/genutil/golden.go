package genutil

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// AssertGolden compares the complete generated output after normalizing only
// checkout line endings. Git may materialize golden files as CRLF on Windows
// while go/format and protocol generators deliberately emit LF; treating that
// transport detail as template drift makes a cross-platform CI gate unusable.
func AssertGolden(t *testing.T, goldenPath string, got []byte, update bool) {
	t.Helper()
	got = normalizeGoldenNewlines(got)
	if update {
		if err := os.MkdirAll(filepath.Dir(goldenPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(goldenPath, got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(goldenPath)
	if err != nil {
		t.Fatalf("missing golden %s (run with -update after intentional template changes): %v", goldenPath, err)
	}
	if !bytes.Equal(got, normalizeGoldenNewlines(want)) {
		t.Errorf("%s differs from generated output; run with -update if the template change is intentional", goldenPath)
	}
}

func normalizeGoldenNewlines(value []byte) []byte {
	return bytes.ReplaceAll(value, []byte("\r\n"), []byte("\n"))
}
