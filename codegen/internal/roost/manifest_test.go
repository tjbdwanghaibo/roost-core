package roost

import (
	"strings"
	"testing"
)

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
	if got := manifest.Services["game"].Mods; !contains(got, "syncbus") || contains(got, "sync") || contains(got, "room") {
		t.Fatalf("service mods were not canonicalized: %v", got)
	}
	// A legacy name in shared_mods must canonicalize on the same path.
	shared := Manifest{Schema: 1, Project: ProjectSpec{Name: "planet", Module: "example.com/planet"},
		Services: map[string]ServiceSpec{"game": {Mods: []string{"nest"}}}, SharedMods: []string{"sync"}}
	_ = shared.Validate()
	if !contains(shared.SharedMods, "syncbus") || contains(shared.SharedMods, "sync") || contains(shared.SharedMods, "room") {
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
	if text := string(raw); indexOf(text, "- syncbus") < 0 || indexOf(text, "- room") >= 0 {
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

// Managers are selected per service type, so a process-wide manager mod would
// start one service's singletons inside every service. shared_mods must reject
// it with a message that says where it belongs.
func TestValidateRejectsManagerModInSharedMods(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", []string{"game"},
		[]string{"nest"}, []string{"nest"})
	manifest.SharedMods = []string{"lock", "manager"}

	err := manifest.Validate()
	if err == nil {
		t.Fatal("shared_mods accepted the manager mod")
	}
	if !strings.Contains(err.Error(), "per service") {
		t.Fatalf("error does not explain where the manager mod belongs: %v", err)
	}
}

// The manager mod is part of the default set, and a default manifest must
// still validate — a default that fails its own validator is worse than no
// default.
func TestDefaultManifestIncludesManagerModAndValidates(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	if err := manifest.Validate(); err != nil {
		t.Fatalf("the default manifest does not validate: %v", err)
	}
	mods := manifest.Services["game"].Mods
	if !contains(mods, "manager") {
		t.Fatalf("default mods do not include the manager mod: %v", mods)
	}
}

// The manager mod's constructor must be wired per service, taking that
// service's own manager list. A shared constructor would give every service
// the same managers, which is exactly what the per-service rule prevents.
func TestBootstrapWiresManagerModPerService(t *testing.T) {
	manifest := DefaultManifest("planet", "example.com/planet",
		[]string{"game", "gate"}, []string{"manager"}, []string{"nest"})
	rendered := renderBootstrap(manifest)

	for _, want := range []string{
		`kitmanager "github.com/tjbdwanghaibo/roost-core/wiring/manager"`,
		"kitmanager.NewManagerMod(serviceGame.Managers()...)",
		"kitmanager.NewManagerMod(serviceGate.Managers()...)",
	} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("bootstrap is missing %q:\n%s", want, rendered)
		}
	}
	// A bare constructor would mean the service's own list was ignored.
	if strings.Contains(rendered, "kitmanager.NewManagerMod()") {
		t.Fatalf("bootstrap wired a manager mod with no managers:\n%s", rendered)
	}
}

// The generated hook must share service.go's package clause, or the two files
// cannot live in the same directory.
func TestServiceManagersHookMatchesServicePackageClause(t *testing.T) {
	for _, name := range []string{"game", "world_boss"} {
		service := renderService(name)
		managers := renderServiceManagers(name)
		clause := func(source string) string {
			line, _, _ := strings.Cut(source, "\n")
			return line
		}
		if clause(service) != clause(managers) {
			t.Fatalf("%s: service.go says %q but managers.go says %q",
				name, clause(service), clause(managers))
		}
		if !strings.Contains(managers, "func Managers() []app.IManager") {
			t.Fatalf("%s: hook does not expose Managers()", name)
		}
	}
}
