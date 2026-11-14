package roost

import (
	"context"
	"testing"
)

func TestModuleMatchmakingGenerated(t *testing.T) {
	root, roost, _ := moduleProject(t, "matchmakingplay")
	roost("module", "add", "matchmaking")
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("matchmaking: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"room": 22, "team": 35, "matchroom": 12, "matchmaking": 17})
	roost("module", "add", "rehome")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("matchmaking/rehome: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"room": 22, "team": 35, "matchroom": 12, "matchmaking": 17, "rehome": 44, "team+rehome": 2, "matchmaking+rehome": 2})
	before := moduleProjectDigest(t, root)
	roost("module", "add", "team", "matchroom", "matchmaking")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed project")
	}

	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestMatchmaking|^TestMatchroom|^TestTeam|^TestWithMatchmakingRehome", "./game/handler", "./game/matchmaking", "./game/controllers/matchmaking")
}
