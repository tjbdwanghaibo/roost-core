package roost

import (
	"context"
	"crypto/sha256"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
)

// module add ai is opt-in. It must coexist with the existing Monster AI instead of silently
// replacing its entity implementation, behavior, protocol, or heartbeats. Compare all existing
// Monster-owned sources (including consumer edits), not just the presence of the new package.
func TestModuleAIInstallsBesideExistingMonster(t *testing.T) {
	root, roost, _ := moduleProject(t, "aimixed")
	roost("module", "add", "monster")
	dirs := []string{"game/components/monsteractor", "game/modules/monster", "game/entities/monster"}
	protected := map[string][sha256.Size]byte{}
	for _, rel := range dirs {
		err := filepath.WalkDir(filepath.Join(root, rel), func(path string, info fs.DirEntry, err error) error {
			if err != nil || info.IsDir() {
				return err
			}
			raw, err := os.ReadFile(path)
			if err == nil {
				protected[path] = sha256.Sum256(raw)
			}
			return err
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	if len(protected) < len(dirs) {
		t.Fatalf("expected existing Monster sources; got %d", len(protected))
	}
	before := moduleProjectDigest(t, root)
	roost("module", "plan", "ai", "--json")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("planning decision AI changed files")
	}
	roost("module", "add", "ai")
	for path, expected := range protected {
		raw, err := os.ReadFile(path)
		if err != nil || sha256.Sum256(raw) != expected {
			t.Fatalf("module ai modified existing monster implementation %s: %v", path, err)
		}
	}
	installed := moduleProjectDigest(t, root)
	roost("module", "add", "ai", "monster")
	if moduleProjectDigest(t, root) != installed {
		t.Fatal("reinstalling AI changed consumer files")
	}
	r, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil || !r.OK {
		t.Fatalf("mixed verify err=%v report=%+v", err, r)
	}
	seen := map[string]int{}
	for _, m := range r.Modules {
		if !m.OK || m.Skipped != 0 {
			t.Fatalf("module verification incomplete: %+v", m)
		}
		seen[m.Name] = m.Passed
	}
	if seen["ai"] == 0 || seen["monster"] != 27 {
		t.Fatalf("new and incumbent scenarios did not both execute: %v", seen)
	}
	t.Logf("verified scenarios %v; preserved %d incumbent Monster sources", seen, len(protected))
}
