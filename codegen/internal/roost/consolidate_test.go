package roost

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeProjectFile(t *testing.T, root, rel, content string) string {
	t.Helper()
	p := filepath.Join(root, filepath.FromSlash(rel))
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}

const legacyGoMod = `module example.com/planet

go 1.27.0

require (
	github.com/tjbdwanghaibo/roost-core v1.12.0
	github.com/tjbdwanghaibo/roost-kit v1.12.6
	github.com/tjbdwanghaibo/roost-skill v1.10.3
	github.com/tjbdwanghaibo/roost-service v1.5.4
)
`

// The upgrader must move every relocated import, decide split packages per
// symbol, keep the identifier a file already uses when the package name
// changes, and leave files on the new layout alone. It maps package paths
// only: a symbol that changed home or name inside the merge (nats.Permanent,
// syncstream.HealthOptions) is left for the compiler to report — there is no
// per-symbol table to maintain (TROUBLESHOOTING T-45).
func TestConsolidateProjectRewritesImportsGoModAndManifest(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	m := DefaultManifest("planet", "example.com/planet", nil, nil, nil)
	m.Versions.Skill = "v1.10.3"
	raw, err := m.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	writeProjectFile(t, root, ManifestName, string(raw))
	mixed := writeProjectFile(t, root, "internal/wiring/wiring.go", `package wiring

import (
	"github.com/tjbdwanghaibo/roost-kit/dataengine"
	kitnats "github.com/tjbdwanghaibo/roost-kit/nats"
	kitredis "github.com/tjbdwanghaibo/roost-kit/redis"
	"github.com/tjbdwanghaibo/roost-kit/syncstream"
	"github.com/tjbdwanghaibo/roost-service/servicemods"
	"github.com/tjbdwanghaibo/roost-skill/skill"
)

var (
	_ = skill.Program{}
	_ = kitredis.NewRedisMod
	_ = kitredis.NewClient
	_ = kitnats.NewClient
	_ = kitnats.Permanent
	_ = dataengine.NewEntityRepository
	_ = syncstream.HealthOptions{}
	_ = servicemods.ModMail
)
`)
	untouched := writeProjectFile(t, root, "internal/fresh/fresh.go", `package fresh

import (
	"github.com/tjbdwanghaibo/roost-core/skill"
	kitmods "github.com/tjbdwanghaibo/roost-kit/mods"
)

var (
	_ = skill.Program{}
	_ = kitmods.ModBus
)
`)
	before, _ := os.ReadFile(untouched)

	var stdout bytes.Buffer
	result, err := ConsolidateProject(root, false, &stdout)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || result.Files[0] != "internal/wiring/wiring.go" || !result.GoMod || !result.Manifest {
		t.Fatalf("result = %+v", result)
	}
	rewritten, _ := os.ReadFile(mixed)
	for _, want := range []string{
		`dataengine "github.com/tjbdwanghaibo/roost-core/dataengine/engine"`, // package name changes: keep the identifier
		`kitredis "github.com/tjbdwanghaibo/roost-kit/redis"`,                // Mod glue stays
		`coreredis "github.com/tjbdwanghaibo/roost-core/redis/driver"`,       // moved symbols get a second import (driver subpackage)
		`_ = coreredis.NewClient`,
		`_ = kitredis.NewRedisMod`,
		`"github.com/tjbdwanghaibo/roost-core/syncstream"`,
		`syncstream.HealthOptions{}`,                            // no symbol table: the compiler reports the new name
		`servicemods "github.com/tjbdwanghaibo/roost-kit/mods"`, // folded package: keep the identifier
		`"github.com/tjbdwanghaibo/roost-core/skill"`,
		`kitnats "github.com/tjbdwanghaibo/roost-core/nats/driver"`, // whole import moves to the driver
		`_ = kitnats.Permanent`,                                     // contract symbol: left for the compiler, no second import is invented
		`_ = kitnats.NewClient`,
	} {
		if !strings.Contains(string(rewritten), want) {
			t.Errorf("rewritten file missing %q:\n%s", want, rewritten)
		}
	}
	for _, bad := range []string{"roost-skill", "roost-service", "roost-kit/dataengine", "roost-kit/syncstream", "natscontract", "PublisherHealthOptions"} {
		if strings.Contains(string(rewritten), bad) {
			t.Errorf("rewritten file still mentions %q:\n%s", bad, rewritten)
		}
	}
	if after, _ := os.ReadFile(untouched); !bytes.Equal(before, after) {
		t.Fatalf("a file already on the new layout was modified:\n%s", after)
	}
	goMod, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	for _, bad := range []string{"roost-skill", "roost-service"} {
		if strings.Contains(string(goMod), bad) {
			t.Errorf("go.mod still requires %s:\n%s", bad, goMod)
		}
	}
	// Versions are left to the dependency resolution step: writing an
	// unpublished boundary release here would break the very go get that
	// follows (upgrade-compat caught exactly that).
	for _, want := range []string{"roost-core v1.12.0", "roost-kit v1.12.6"} {
		if !strings.Contains(string(goMod), want) {
			t.Errorf("go.mod versions must be untouched (%q):\n%s", want, goMod)
		}
	}
	manifest, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	if manifest.Versions.Skill != "" || manifest.Versions.Service != "" {
		t.Fatalf("manifest kept the removed module policies: %+v", manifest.Versions)
	}
	if !strings.Contains(stdout.String(), "rewrote 1 Go file(s), go.mod, roost.yaml") {
		t.Fatalf("report = %q", stdout.String())
	}

	// Idempotent: a second run finds nothing to do.
	again, err := ConsolidateProject(root, false, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(again.Files) != 0 || again.GoMod || again.Manifest {
		t.Fatalf("second run was not a no-op: %+v", again)
	}
}

// --dry-run reports the same plan without touching a single file.
func TestConsolidateProjectDryRunWritesNothing(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	file := writeProjectFile(t, root, "x.go", "package x\n\nimport \"github.com/tjbdwanghaibo/roost-skill/skill\"\n\nvar _ = skill.Program{}\n")
	before, _ := os.ReadFile(file)
	result, err := ConsolidateProject(root, true, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Files) != 1 || !result.GoMod {
		t.Fatalf("dry run plan = %+v", result)
	}
	after, _ := os.ReadFile(file)
	goMod, _ := os.ReadFile(filepath.Join(root, "go.mod"))
	if !bytes.Equal(before, after) || string(goMod) != legacyGoMod {
		t.Fatal("dry run modified the project")
	}
}

// An import on a removed module that the map does not know is an error, not
// a silent leftover that fails at go build time.
func TestConsolidateProjectRefusesUnmappedRemovedImports(t *testing.T) {
	root := t.TempDir()
	writeProjectFile(t, root, "go.mod", legacyGoMod)
	writeProjectFile(t, root, "x.go", "package x\n\nimport \"github.com/tjbdwanghaibo/roost-skill/internal/wire\"\n\nvar _ = wire.Thing\n")
	_, err := ConsolidateProject(root, false, nil)
	if err == nil || !strings.Contains(err.Error(), "roost-skill/internal/wire") {
		t.Fatalf("unmapped removed import accepted: %v", err)
	}
}

// The embedded relocation map itself must stay coherent: every rename points
// at a mapped package, and its boundary sits at or below the generator floor.
//
// The two used to be required equal, and that stopped being true: the
// boundary is the version where the two-module layout arrived (below it, a
// project needs `upgrade --consolidate` to rewrite imports), while the floor
// is the oldest framework the CURRENT generated code compiles against. The
// generators moved on; the layout did not. What must hold is the direction —
// a floor below the boundary would promise support for a layout this
// generator can no longer emit for.
func TestConsolidationMapMatchesTheGeneratorFloor(t *testing.T) {
	table, m, err := loadConsolidationMap()
	if err != nil {
		t.Fatal(err)
	}
	for _, pair := range []struct {
		name              string
		boundary, floor   string
	}{
		{"core", m.Boundary.Core, minimumVersions.Core},
		{"kit", m.Boundary.Kit, minimumVersions.Kit},
		{"codegen", m.Boundary.Codegen, minimumVersions.Codegen},
	} {
		major, minor, patch, ok := releaseVersion(pair.boundary)
		if !ok {
			t.Fatalf("%s boundary %q is not a release version", pair.name, pair.boundary)
		}
		if !versionAtLeast(pair.floor, major, minor, patch) {
			t.Fatalf("%s floor %s is below the consolidation boundary %s", pair.name, pair.floor, pair.boundary)
		}
	}
	for _, old := range []string{"github.com/tjbdwanghaibo/roost-skill/skill", "github.com/tjbdwanghaibo/roost-service/mail", "github.com/tjbdwanghaibo/roost-kit/nestwal", "github.com/tjbdwanghaibo/roost-kit/redis"} {
		if _, ok := table[old]; !ok {
			t.Errorf("map lacks %s", old)
		}
	}
}
