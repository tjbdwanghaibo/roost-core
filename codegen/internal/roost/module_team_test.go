package roost

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

func TestModuleTeamGenerated(t *testing.T) {
	root, roost, _ := moduleProject(t, "teamplay")
	manifestPath := filepath.Join(root, ManifestName)
	raw, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	reduced := regexp.MustCompile(`(?m)^\s*- remote_entity\n`).ReplaceAll(raw, nil)
	if bytes.Equal(reduced, raw) {
		t.Fatal("game fixture no longer contains remote_entity")
	}
	if err := os.WriteFile(manifestPath, reduced, 0o644); err != nil {
		t.Fatal(err)
	}
	roost("project", "sync")
	roost("module", "add", "team")
	restored, err := os.ReadFile(manifestPath)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(restored, []byte("- remote_entity")) {
		t.Fatal("team did not install its remote runtime")
	}

	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("team: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"team": 35})
	roost("module", "add", "rehome")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("team/rehome: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"team": 35, "rehome": 40, "team+rehome": 2})
	before := moduleProjectDigest(t, root)
	roost("module", "add", "rehome", "team")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat team install changed project")
	}
	recipeMutationMustFail(t, root, "game/entities/team/roster.go", "!invited || now >= expiry", "!invited || now > expiry", "./game/handler", "TestTeamInviteExpiresAtExactDeadline", "deadline accepted:")
	recipeMutationMustFail(t, root, "game/entities/team/roster.go", "const MaxMembers = 8", "const MaxMembers = 9", "./game/handler", "TestTeamCapacityIsCheckedWhenAccepting", "ninth member accepted:")
	recipeMutationMustFail(t, root, "game/entities/team/roster.go", "const MaxInvites = 64", "const MaxInvites = 65", "./game/handler", "TestTeamInvitationsAreBoundedAndCanBeRenewed", "65th invitation accepted:")
	recipeMutationMustFail(t, root, "game/entities/team/roster.go", "member < next", "member > next", "./game/handler", "TestTeamLeaderPassesToSmallestMember", "leader handoff=")
	recipeMutationMustFail(t, root, "game/entities/team/roster.go", "d.GetRevision() != 0", "d.GetLeader() != 0", "./game/handler", "TestTeamLastLeaveKeepsNonReusableTombstone", "empty team ID reused:")
	recipeMutationMustFail(t, root, "game/entities/player/team_component.go", "current != 0 && current != id", "false && current != 0 && current != id", "./game/handler", "TestTeamMemberCannotJoinASecondTeam", "joined second team:")
	recipeMutationMustFail(t, root, "game/entities/player/team_rehome.go", "owner.TeamComp().TeamID() != 0", "false && owner.TeamComp().TeamID() != 0", "./game/handler", "TestWithTeamRehomeRejectsAPlayerInTeam", "team allowed rehome:")
	recipeMutationMustFail(t, root, "game/entities/player/team_rehome.go", `owner.Dao().GetRehome().GetPendingID() != ""`, `false && owner.Dao().GetRehome().GetPendingID() != ""`, "./game/handler", "TestWithTeamRehomeRejectsJoiningDuringMove", "moving player joined:")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestTeam|^TestWithTeamRehome", "./game/handler", "./game/controllers/team")

}

// 只验证本次 demo 迁移的队伍、分区投影、换服接线；不提前跑全 demo 验收。
func TestDemoTeamModuleIntegration(t *testing.T) {
	root := copyOfNewProject(t, "game-demo")
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "go.mod")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(raw, nil)
	raw = append(raw, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err := os.WriteFile(path, raw, 0644); err != nil {
		t.Fatal(err)
	}
	runGeneratedGo(t, root, "test", "-count=1", "-run", "^TestTeam|^TestWithTeam|^TestCellGroup|^TestRehome|^TestPartition", "./game/handler", "./game/entities/player", "./game/controllers/team", "./game/settings", "./internal/service/game")
	runGeneratedGo(t, root, "build", "./...")
}
