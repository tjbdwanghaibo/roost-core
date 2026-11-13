package roost

import (
	"context"
	"testing"
)

// 新 game 先装纯登记点，再增量装五组命令；身份、参数、事务与重放边界都在生成工程中验收。
func TestModuleGMScenariosAndMutations(t *testing.T) {
	root, roost, _ := moduleProject(t, "gmops")
	roost("module", "add", "gm")
	report, err := VerifyModules(context.Background(), root, []string{"gm"}, nil)
	if err != nil || len(report.Modules) != 1 || report.Modules[0].Passed != 9 || report.Modules[0].Skipped != 0 {
		t.Fatalf("gm alone: %v %+v", err, report)
	}
	roost("module", "add", "wallet", "bag", "profile", "mailbox", "rehome")
	report, err = VerifyModules(context.Background(), root, nil, nil)
	if err != nil {
		t.Fatalf("combined: %v %+v", err, report)
	}
	want := map[string]int{"gm": 9, "wallet": 17, "bag": 20, "profile": 20, "mailbox": 33, "rehome": 44, "mailbox+bag": 9, "mailbox+profile": 10, "wallet+gm": 4, "bag+gm": 4, "profile+gm": 4, "mailbox+gm": 4, "rehome+gm": 4, "enter": 29, "enter+profile": 3, "profile+enter": 4, "mailbox+enter": 5, "rehome+enter": 5}
	if len(report.Modules) != len(want) {
		t.Fatalf("rows: %+v", report.Modules)
	}
	for _, row := range report.Modules {
		if row.Passed != want[row.Name] || row.Skipped != 0 || !row.OK {
			t.Fatalf("row: %+v", row)
		}
	}
	before := moduleProjectDigest(t, root)
	roost("module", "add", "rehome", "mailbox", "profile", "bag", "wallet", "gm")
	if moduleProjectDigest(t, root) != before {
		t.Fatal("repeat install changed project")
	}
	recipeMutationMustFail(t, root, "game/gm/gm.go", "!auditText(command.Operator, 64)", "false", "./game/gm", "TestGMRequiresOperator", "calls=1")
	recipeMutationMustFail(t, root, "game/gm/gm.go", "!auditText(meta.Note, 256)", "false", "./game/gm", "TestGMRequiresNote", "calls=1")
	recipeMutationMustFail(t, root, "game/gm/gm.go", "if r.stopped.Load() {", "if false && r.stopped.Load() {", "./game/gm", "TestGMStopRejectsNewCalls", "calls=1")
	recipeMutationMustFail(t, root, "game/gm/wallet.go", "full, payload.Delta)", "full, payload.Delta*2)", "./game/gm", "TestWithWalletGMSuccess", "value=")
	recipeMutationMustFail(t, root, "game/gm/bag.go", "payload.ItemID, payload.Count)", "payload.ItemID, payload.Count+1)", "./game/gm", "TestWithBagGMSuccess", "value=")
	recipeMutationMustFail(t, root, "game/gm/profile.go", "full, payload.Amount)", "full, payload.Amount+1)", "./game/gm", "TestWithProfileGMSuccess", "value=")
	recipeMutationMustFail(t, root, "game/gm/mailbox.go", "if err := syncsender.NewGmMailTargetSender(client).Sync_GMMailTarget(ctx, full); err != nil {", "if err := syncsender.NewGmMailTargetSender(client).Sync_GMMailTarget(ctx, full); false && err != nil {", "./game/gm", "TestWithMailboxGMMissingTargetWritesNothing", "result=")
	recipeMutationMustFail(t, root, "game/gm/rehome.go", `"gm "+command.Operator+": "+payload.Note`, `payload.Note`, "./game/gm", "TestWithRehomeGMSuccess", "note=")
	runGeneratedGo(t, root, "test", "-race", "-count=2", "./game/gm")
}
