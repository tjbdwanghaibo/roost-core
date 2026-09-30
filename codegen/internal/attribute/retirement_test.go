package attribute

import (
	"io"
	"os"
	"path/filepath"
	"testing"
)

func TestRunRetiresOnlyOwnedProfileAfterLastMarkerIsRemoved(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "profile.go")
	if err := os.WriteFile(source, []byte("package attribute\n//roost:attribute index=1 max=4\ntype PlayerProfile struct { HP int64; dirtyMask uint64 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	generated := filepath.Join(dir, "gen_player_profile_attribute.go")
	if _, err := os.Stat(generated); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(dir, "gen_manual_attribute.go")
	if err := os.WriteFile(manual, []byte("package attribute\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("package attribute\ntype PlayerProfile struct { HP int64; dirtyMask uint64 }\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := Run([]string{"-dir", dir}, io.Discard); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(generated); !os.IsNotExist(err) {
		t.Fatalf("retired profile remains: %v", err)
	}
	if _, err := os.Stat(manual); err != nil {
		t.Fatalf("manual file removed: %v", err)
	}
}
