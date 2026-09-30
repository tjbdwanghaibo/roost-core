package roost

import (
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSyncCommitsProtocolRetirement(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"protocol"}})
	if err != nil {
		t.Fatal(err)
	}
	paths, err := Add(root, AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "game"})
	if err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	wantRemoved := []string{"protocol/proto/protocol.proto", "protocol/pb/protocol.pb.go", "protocol/msgid/msgid_gen.go", "protocol/protocol_manifest.json"}
	for _, rel := range wantRemoved {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); err != nil {
			t.Fatalf("initial generated output %s: %v", rel, err)
		}
	}
	if err := os.Remove(filepath.Join(root, filepath.FromSlash(paths[0]))); err != nil {
		t.Fatal(err)
	}
	result, err := SyncProject(root)
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range wantRemoved {
		if _, err := os.Stat(filepath.Join(root, filepath.FromSlash(rel))); !os.IsNotExist(err) {
			t.Fatalf("retired %s remains: %v", rel, err)
		}
		if !contains(result.Removed, rel) {
			t.Fatalf("%s missing from removed result: %v", rel, result.Removed)
		}
	}
}

func TestGenerateCheckDetectsMarkerlessProtocolDrift(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	_, root, err := NewProject(NewOptions{Name: "planet", Module: "example.com/planet", Out: target, Features: []string{"protocol"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Add(root, AddOptions{Kind: "protocol", Name: "PlayerLogin", Group: "game"}); err != nil {
		t.Fatal(err)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		t.Fatal(err)
	}
	for _, rel := range []string{"protocol/proto/protocol.proto", "protocol/protocol_manifest.json"} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, append(append([]byte(nil), raw...), []byte("\n ")...), 0o644); err != nil {
			t.Fatal(err)
		}
		err = Generate(root, GenerateOptions{Check: true, Stdout: io.Discard})
		if err == nil || !strings.Contains(err.Error(), rel) {
			t.Fatalf("-check accepted changed %s: %v", rel, err)
		}
		if err := os.WriteFile(path, raw, 0o644); err != nil {
			t.Fatal(err)
		}
	}
}
