package roost

// RR-20260927-05（OPEN-ITEMS C05，维护者 2026-09-27 按推荐执行）：生成的 player TCP Mod（accessplayertcp）
// 声明 StopBudget() = player_access.tcp.shutdown_timeout，生成的 shutdown.total_timeout 与 doctor 同步计入它。
//
// 之前它不声明预算，App 只给固定 3s 保底：低于它自己的 shutdown_timeout 10s，与 nest.request_timeout 3s
// 也没有余量（在途请求用满 3s 时连接排空没有时间）。生成公式按“声明预算之和 + 3s × 未声明数 + 5s”计算，
// 所以 game 服务的 total 与宽限期各多 10s − 3s = 7s（-mods configdata,mongo,nats,dataengine,nest 时
// 101s / 106s → 108s / 113s；App 单实例锁的 Release 预留再加 3s，现为 111s / 116s；RR-20261006-59 后 game 多一个 etcd Mod，为 114s / 119s）；doctor 按配置里的 player_access.tcp.shutdown_timeout 判定并给建议值。

import (
	"strings"
	"testing"
	"time"
)

// setPlayerTCPShutdownTimeout rewrites player_access.tcp.shutdown_timeout.
func setPlayerTCPShutdownTimeout(t *testing.T, root, rel, value string) {
	t.Helper()
	body := readProjectFile(t, root, rel)
	block := strings.Index(body, "player_access:\n")
	if block < 0 {
		t.Fatalf("%s has no player_access: block", rel)
	}
	key := strings.Index(body[block:], "shutdown_timeout: ")
	if key < 0 {
		t.Fatalf("%s: player_access: block has no shutdown_timeout", rel)
	}
	at := block + key + len("shutdown_timeout: ")
	end := strings.IndexByte(body[at:], '\n')
	writeProjectFile(t, root, rel, body[:at]+value+body[at+end:])
}

// The generated game service's window counts the player TCP Mod's 10s.
func TestGeneratedShutdownCountsThePlayerTCPStopBudget(t *testing.T) {
	root := newGameDemo(t)
	plan := gamePlan(t, root)
	if plan.playerTCP != 1 || plan.dataEngine != 1 || plan.declared != generatedDataEngineShutdownTimeout+generatedPlayerTCPShutdownTimeout {
		t.Fatalf("game plan declares %+v, want dataengine 30s + player tcp 10s", plan)
	}
	for _, rel := range gameConfigs {
		body := readProjectFile(t, root, rel)
		wants := []string{"40s declared + 3s x 22\n", "3s for the singleton release = 114s", "total_timeout: 114s\n"}
		if rel != gameConfigs[2] { // the k8s secret example carries no player_access block: the Mod's default 10s
			wants = append(wants, "shutdown_timeout: 10s\n")
		}
		for _, want := range wants {
			if !strings.Contains(body, want) {
				t.Errorf("%s does not contain %q", rel, want)
			}
		}
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK || !strings.Contains(item.Detail, "total_timeout 114s, grace period 119s") {
		t.Errorf("fresh game-demo: %s %s", item.Status, item.Detail)
	}
}

// playerTCPConfigs are the game configs that carry a player_access block (the
// k8s secret example does not: the Mod runs on its default there).
var playerTCPConfigs = gameConfigs[:2]

// doctor reads the listener's budget from the config: raised to 30s, the
// generated total no longer covers it, and the advice counts the 30s.
func TestDoctorCountsTheConfiguredPlayerTCPStopBudget(t *testing.T) {
	root := newGameDemo(t)
	plan := gamePlan(t, root)
	for _, rel := range playerTCPConfigs {
		setPlayerTCPShutdownTimeout(t, root, rel, "30s")
	}
	item := shutdownStatus(t, root)["shutdown:game"]
	want := plan.total + 20*time.Second
	if item.Status != StatusWarn || !strings.Contains(item.Detail, "(60s declared + 3s x 22 + 3s singleton release = ") || !strings.Contains(item.Detail, "Set it to "+seconds(want)) {
		t.Fatalf("player_access.tcp.shutdown_timeout 30s with total_timeout %s: %s %s, want a WARN with 60s declared and advice %s",
			seconds(plan.total), item.Status, item.Detail, seconds(want))
	}
	// 0s is the Mod's 10s (configFromViper keeps the default for 0).
	root = newGameDemo(t)
	for _, rel := range playerTCPConfigs {
		setPlayerTCPShutdownTimeout(t, root, rel, "0s")
	}
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusOK {
		t.Errorf("player_access.tcp.shutdown_timeout 0s (the Mod uses 10s): %s %s, want OK", item.Status, item.Detail)
	}
}

// A negative value stops the Mod from starting: a FAIL in the dev config,
// a WARN naming the file and key in an example (like any value the doctor
// cannot use), never a budget.
func TestDoctorRejectsANegativePlayerTCPShutdownTimeout(t *testing.T) {
	root := newGameDemo(t)
	dev, prod := gameConfigs[0], gameConfigs[1]
	setPlayerTCPShutdownTimeout(t, root, prod, "-1s")
	item := shutdownStatus(t, root)["shutdown:game"]
	if item.Status != StatusWarn || !strings.Contains(item.Detail, prod+": player_access.tcp.shutdown_timeout: -1s is negative") || strings.Contains(item.Detail, "Set it to") {
		t.Errorf("prod example with a negative player tcp shutdown_timeout: %s %s", item.Status, item.Detail)
	}
	setPlayerTCPShutdownTimeout(t, root, dev, "-1s")
	if item := shutdownStatus(t, root)["shutdown:game"]; item.Status != StatusFail || !strings.Contains(item.Detail, dev+": player_access.tcp.shutdown_timeout") {
		t.Errorf("dev config with a negative player tcp shutdown_timeout: %s %s, want a FAIL", item.Status, item.Detail)
	}
}

// A project generated before the Mod declared its budget carries unedited
// shutdown: blocks written for "30s declared + 3s x 22 = 101s". One sync
// recognises them (their summary still matches what that generator wrote)
// and moves them, and every template, to the new plan.
func TestSyncMovesAnUneditedBlockWrittenBeforeThePlayerTCPBudget(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	plan := gamePlan(t, root)
	before := plan
	before.declaring, before.dataEngine, before.playerTCP = 1, 1, 0
	before.declared = generatedDataEngineShutdownTimeout
	before.total = before.declared + time.Duration(before.undeclared())*generatedModStopFloor + before.margin
	for _, rel := range gameConfigs {
		replaceShutdownBlock(t, root, rel, renderShutdownConfig(before))
		if body := readProjectFile(t, root, rel); !strings.Contains(body, "total_timeout: "+seconds(before.total)+"\n") {
			t.Fatalf("setup: %s does not carry the old %s block", rel, seconds(before.total))
		}
	}
	syncProject(t, root)
	for _, rel := range gameConfigs {
		if body := readProjectFile(t, root, rel); !strings.Contains(body, "total_timeout: "+seconds(plan.total)+"\n") {
			t.Errorf("%s was not moved from %s to %s", rel, seconds(before.total), seconds(plan.total))
		}
	}
	assertDeployedGrace(t, root, "game", int((plan.total+generatedGraceOverTotal)/time.Second))
	assertProjectDiffEmpty(t, root)
}
