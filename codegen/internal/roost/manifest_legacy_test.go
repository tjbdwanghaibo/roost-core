package roost

import "testing"

// roost.yaml is hand-written and lives in user repositories, so the pre-rename
// vocabulary ("sync" mod, "replication-*" features) has to keep loading after
// the packages behind those names were renamed. Validate is the single gate
// every construction path passes through, so the rewrite is asserted there
// rather than only on the YAML decode path.
func TestValidateCanonicalizesLegacyModAndFeatureNames(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", []string{"game"},
		[]string{"sync"}, []string{"nest", "replication-quic"})
	manifest.SharedMods = []string{"lock"}

	if err := manifest.Validate(); err != nil {
		t.Fatalf("legacy manifest rejected: %v", err)
	}
	if got := manifest.Services["game"].Mods; !contains(got, "room") || contains(got, "sync") {
		t.Fatalf("service mods were not canonicalized: %v", got)
	}
	// A legacy name in shared_mods must canonicalize on the same path.
	shared := Manifest{Schema: 1, Project: ProjectSpec{Name: "planet", Module: "example.com/planet"},
		Services: map[string]ServiceSpec{"game": {Mods: []string{"nest"}}}, SharedMods: []string{"sync"}}
	_ = shared.Validate()
	if !contains(shared.SharedMods, "room") || contains(shared.SharedMods, "sync") {
		t.Fatalf("shared mods were not canonicalized: %v", shared.SharedMods)
	}
	if !contains(manifest.Features, "nettransport-quic") || contains(manifest.Features, "replication-quic") {
		t.Fatalf("features were not canonicalized: %v", manifest.Features)
	}
	// The canonical spelling is what gets written back, so a legacy manifest
	// converts itself the first time `project sync` runs.
	raw, err := manifest.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if text := string(raw); indexOf(text, "- room") < 0 {
		t.Fatalf("marshalled manifest did not persist the canonical mod name:\n%s", text)
	}
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// An unknown name must still be an error: canonicalization maps a known legacy
// spelling, it does not make validation permissive.
func TestValidateStillRejectsUnknownModAndFeature(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", []string{"game"},
		[]string{"not_a_mod"}, []string{"nest"})
	if err := manifest.Validate(); err == nil {
		t.Fatal("unknown mod accepted")
	}
	manifest = DefaultManifest("planet", "example.com/planet", []string{"game"},
		[]string{"room"}, []string{"not_a_feature"})
	if err := manifest.Validate(); err == nil {
		t.Fatal("unknown feature accepted")
	}
}
