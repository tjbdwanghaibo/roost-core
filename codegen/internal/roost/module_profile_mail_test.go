package roost

import (
	"context"
	"testing"
)

func TestModuleProfileMailScenariosAndMutations(t *testing.T) {
	root, roost, _ := moduleProject(t, "rewardmail")
	roost("module", "add", "profile", "mailbox")
	report, err := VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("verification: %v %+v", err, report)
	}
	want := map[string]int{"mailbox": 32, "profile": 20, "wallet": 17, "mailbox+profile": 10, "enter": 29, "enter+profile": 3, "profile+enter": 4, "mailbox+enter": 5}
	if len(report.Modules) != len(want) {
		t.Fatalf("rows=%+v", report.Modules)
	}
	for _, row := range report.Modules {
		if row.Passed != want[row.Name] || row.Skipped != 0 || !row.OK {
			t.Fatalf("row=%+v", row)
		}
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "mailbox", "profile")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed project")
	}
	recipeMutationMustFail(t, root, "game/mailbox/mailbox_profile.go", "if reward == (Attachment{}) {", "if false && reward == (Attachment{}) {", "./game/handler", "TestWithMailboxProfileDefaultDoesNotReward", "not one this version can grant")
	recipeMutationMustFail(t, root, "game/mailbox/mailbox_profile.go", "Level: to,", "Level: to+1,", "./game/handler", "TestWithMailboxProfileOneUpgradeOneEffect", "events=")
	recipeMutationMustFail(t, root, "game/mailbox/mailbox_profile.go", "From: from,", "From: to-1,", "./game/handler", "TestWithMailboxProfileSeveralLevelsOneCombinedMail", "combined events=")
	recipeMutationMustFail(t, root, "game/mailbox/mailbox_profile.go", "return Validate(event.Reward)", "return nil", "./game/handler", "TestWithMailboxProfileInvalidRewardRollsBackUpgrade", "invalid reward accepted")
	recipeMutationMustFail(t, root, "game/mailbox/mailbox_profile.go", "RequestID: envelope.EffectID,", "RequestID: envelope.EffectID+fmt.Sprint(event.Reward.Amount),", "./game/mailbox", "TestWithMailboxProfileReplayKeepsFirstReward", "envelopes=")
	recipeMutationMustFail(t, root, "game/mailbox/mailbox_profile.go", "RequestID: envelope.EffectID,", "RequestID: \"constant\",", "./game/mailbox", "TestWithMailboxProfileTransportFailureCanRetry", "requests=")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "-run", "^TestWithMailboxProfile", "./game/handler", "./game/mailbox")
}
