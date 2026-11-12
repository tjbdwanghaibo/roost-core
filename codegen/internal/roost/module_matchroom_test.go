package roost

import (
	"context"
	"testing"
)

func TestModuleMatchroomGenerated(t *testing.T) {
	root, roost, _ := moduleProject(t, "matchroomplay")
	roost("module", "add", "matchroom")
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("matchroom: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"room": 22, "matchroom": 12})
	before := moduleProjectDigest(t, root)
	roost("module", "add", "room", "matchroom")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed project")
	}
	recipeMutationMustFail(t, root, "game/entities/room/matchroom_component.go", `state.GetMatchID() != "" && !slices.Contains`, `false && state.GetMatchID() != "" && !slices.Contains`, "./game/handler", "TestMatchroomRejectsOutsiderWithoutTakingReservedSeat", "outsider admitted:")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestRoom|^TestMatchroom", "./game/handler", "./game/matchroom", "./game/controllers/room")
}
