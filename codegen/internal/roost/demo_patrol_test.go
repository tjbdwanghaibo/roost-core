package roost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Both projects must use the same application-owned route in a real movement
// decision. A one-point shift in that file must break BOTH consumer suites.
func TestPatrolRouteUsedByDemoAndMonster(t *testing.T) {
	if testing.Short() {
		t.Skip("generates two consumer projects")
	}
	const route = "game/modules/monster/patrol/route.go"
	for _, mode := range []string{"demo", "module"} {
		t.Run(mode, func(t *testing.T) {
			var root, pkg, target, cases string
			if mode == "demo" {
				root = copyOfNewProject(t, "game-demo")
				useLocalRoostCore(t, root)
				pkg, target, cases = "./game/actor", "TestPatrolWalksTheFilteredRouteAndRetriesARefusedStart", "^TestPatrol"
			} else {
				var install func(...string) string
				root, install, _ = moduleProject(t, "patrolroute")
				install("module", "add", "monster")
				pkg, target, cases = "./game/modules/monster/service", "TestMonsterPatrolMovesThroughActor", "^TestMonster(PatrolMovesThroughActor|FailedDecisionRollsBackCursorAndMovement|RootedStopsAndResumesPatrol|ChasesPublishedPlayerWithoutRestartingSlowFollow|OutsideSenseRadiusKeepsPatrolling)$"
			}
			m, err := LoadManifest(root)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "demo" {
				for _, name := range []string{"actor", "monster"} {
					if _, installed := m.Modules[name]; installed {
						t.Fatalf("sharing a route unexpectedly installed module %s", name)
					}
				}
			}
			want, err := renderModuleFile(m, "game", ModuleSpec{Name: "monster"}, "files", route, nil)
			if err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, filepath.FromSlash(route))
			got, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(got, want) {
				t.Fatalf("%s route differs from the canonical module template: %v", mode, err)
			}
			t.Log(runGeneratedGo(t, root, "test", "-count=1", "-timeout=60s", "-v", "-run", cases, pkg))

			// Each root is a disposable private copy. Leave the local module source alone.
			const from = "start(points[cursor%len(points)])"
			if strings.Count(string(got), from) != 1 {
				t.Fatal("shared waypoint selection anchor changed")
			}
			mutant := strings.Replace(string(got), from, "start(points[(cursor+1)%len(points)])", 1)
			if err := os.WriteFile(path, []byte(mutant), 0o644); err != nil {
				t.Fatal(err)
			}
			out, err := runModuleTool(context.Background(), root, "go", "test", "-mod=mod", "-count=1", "-timeout=60s", "-v", "-run", "^"+target+"$", pkg)
			if err == nil || !strings.Contains(string(out), "--- FAIL: "+target) || !strings.Contains(string(out), "patrol destination 0 =") {
				t.Fatalf("%s did not observe the shifted route as a wrong first destination: %v\n%s", mode, err, out)
			}
			t.Logf("compiled shifted-waypoint mutation caught by %s", target)
		})
	}
}
