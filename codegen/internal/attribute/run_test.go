package attribute

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestRunGeneratesProfile(t *testing.T) {
	dir := t.TempDir()
	source := `package attribute

//roost:attribute index=1 max=4
type PlayerProfile struct {
	HP int64
	dirtyMask uint64
}
`
	if err := os.WriteFile(filepath.Join(dir, "profile.go"), []byte(source), 0o644); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	var output bytes.Buffer
	if err := Run([]string{"-dir", dir}, &output); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "gen_player_profile_attribute.go")); err != nil {
		t.Fatalf("generated file: %v", err)
	}
}
