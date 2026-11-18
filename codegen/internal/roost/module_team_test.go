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
	if !bytes.Contains(restored, []byte("facebook:")) {
		t.Fatal("team did not bring the facebook module it requires")
	}

	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("team: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"facebook": 9, "team": 32})
	roost("module", "add", "rehome")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("team/rehome: %v %+v", err, report)
	}
	checkTaskModuleRows(t, report, map[string]int{"facebook": 9, "team": 32, "rehome": 62, "team+rehome": 2})
	before := moduleProjectDigest(t, root)
	roost("module", "add", "rehome", "team")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat team install changed project")
	}
	recipeMutationMustFail(t, root, "game/components/teamroster/teamroster.go", "!invited || now >= expiry", "!invited || now > expiry", "./game/handler", "TestTeamInviteExpiresAtExactDeadline", "deadline accepted:")
	recipeMutationMustFail(t, root, "game/components/teamroster/teamroster.go", "const MaxMembers = 8", "const MaxMembers = 9", "./game/handler", "TestTeamCapacityIsCheckedWhenAccepting", "ninth member accepted:")
	recipeMutationMustFail(t, root, "game/components/teamroster/teamroster.go", "const MaxInvites = 64", "const MaxInvites = 65", "./game/handler", "TestTeamInvitationsAreBoundedAndCanBeRenewed", "65th invitation accepted:")
	recipeMutationMustFail(t, root, "game/components/teamroster/teamroster.go", "member < next", "member > next", "./game/handler", "TestTeamLeaderPassesToSmallestMember", "leader handoff=")
	recipeMutationMustFail(t, root, "game/components/teamroster/teamroster.go", "d.GetRevision() != 0", "d.GetLeader() != 0", "./game/handler", "TestTeamLastLeaveKeepsNonReusableTombstone", "empty team ID reused:")
	recipeMutationMustFail(t, root, "game/components/team/team.go", "current != 0 && current != id", "false && current != 0 && current != id", "./game/handler", "TestTeamMemberCannotJoinASecondTeam", "joined second team:")
	recipeMutationMustFail(t, root, "game/components/team/team_rehome.go", "c.TeamID() != 0", "false && c.TeamID() != 0", "./game/handler", "TestWithTeamRehomeRejectsAPlayerInTeam", "team allowed rehome:")
	recipeMutationMustFail(t, root, "game/components/team/team_rehome.go", `c.owner.Dao().GetRehome().GetPendingID() != ""`, `false && c.owner.Dao().GetRehome().GetPendingID() != ""`, "./game/handler", "TestWithTeamRehomeRejectsJoiningDuringMove", "moving player joined:")
	// 邀请的跨 game 投递（facebook）：Facebook 是共享 kind（生成的登记声明它，首写一条插入就是共享记录）、没打开的收件箱
	// 不经 Nest 读、收件箱有界、邀请提交后经 facebook 投递。真实依赖的跨进程用例是 scripts/test-alliance-e2e.sh 的
	// TestAllianceE2ESocialTeamInvite。
	recipeMutationMustFail(t, root, "game/entities/facebook/facebook_gen_wire.go", "entity.DeclareRemoteSharedKind(EntityKindFacebook)", "_ = EntityKindFacebook", "./game/modules/facebook", "TestFacebookIsASharedKind", "is not declared shared")
	recipeMutationMustFail(t, root, "game/modules/facebook/facebook.go", "if !found || !lease.Shared {", "if false && (!found || !lease.Shared) {", "./game/modules/facebook", "TestFacebookUnopenedInboxIsEmptyWithoutNest", "unopened facebook")
	recipeMutationMustFail(t, root, "game/components/facebook/facebook.go", "!renewing && dao.InvitesLen() >= MaxInvites", "!renewing && dao.InvitesLen() > MaxInvites", "./game/handler", "TestFacebookInvitesAreBoundedAndPruned", "invite beyond the bound accepted")
	// Team 也是共享 kind：生成的登记声明它，建队认领一条插入就是共享记录，不再切共享；认领答出独占记录时建队报错。
	recipeMutationMustFail(t, root, "game/entities/team/team_gen_wire.go", "entity.DeclareRemoteSharedKind(EntityKindTeam)", "_ = EntityKindTeam", "./game/controllers", "TestTeamCreateClaimsSharedInOneStep", "kind declared shared=false")
	recipeMutationMustFail(t, root, "game/controllers/team.go", "	if !lease.Shared {", "	if false && !lease.Shared {", "./game/controllers", "TestTeamCreateClaimsSharedInOneStep", "an exclusive claim was accepted")
	recipeMutationMustFail(t, root, "game/controllers/team.go", "if invitation.ExpireAt == 0 {", "if invitation.ExpireAt != 0 {", "./game/controllers", "TestTeamEndpointInvitationNoticeFailureDoesNotUndoAuthority", "online invitee not told")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestTeam|^TestWithTeamRehome|^TestFacebook", "./game/handler", "./game/controllers", "./game/modules/facebook")

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
	runGeneratedGo(t, root, "test", "-count=1", "-run", "^TestTeam|^TestWithTeam|^TestCellGroup|^TestRehome|^TestPartition", "./game/handler", "./game/entities/player", "./game/controllers", "./game/settings", "./internal/service/game")
	runGeneratedGo(t, root, "build", "./...")
}
