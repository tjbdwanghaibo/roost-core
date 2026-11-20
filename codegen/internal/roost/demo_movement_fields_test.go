package roost

import (
	"strings"
	"testing"
)

// This is a consumer test: compile the generated DAOs, both ordinary-map
// components and the scene's client decoders against this checkout.
func TestDemoOrdinaryMovementFieldsWireContract(t *testing.T) {
	if testing.Short() {
		t.Skip("generates a demo project")
	}
	root := copyOfNewProject(t, "game-demo")
	useLocalRoostCore(t, root)
	const test = "TestOrdinaryMovementFieldsReachWatcherOnlyOnCommit"
	out := runGeneratedGo(t, root, "test", "-count=1", "-v", "-timeout=60s", "-run", "^"+test+"$", "./internal/service/game")
	for _, kind := range []string{"player", "monster"} {
		if !strings.Contains(out, "--- PASS: "+test+"/"+kind+" (") {
			t.Fatalf("generated %s consumer test did not run:\n%s", kind, out)
		}
	}
	t.Log(out)
}
