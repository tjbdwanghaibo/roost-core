package rewards

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/hotcode"
)

// 热补丁点的三条承诺：没注册时用原函数、替换之后立刻生效、revert 回到原样。
//
// 第一条是最容易漏的：一个测试、一个工具、一个没装补丁点的进程，调用方仍然
// 必须拿到正确的行为。`hotcode.Resolve` 的 fallback 就是为这个存在的——
// 如果它返回零值或 panic，那么"能热补丁"这件事的代价就是"平时也可能坏"。

func TestTheRewardWorksWithoutAnyPatchPointRegistered(t *testing.T) {
	// No Register call anywhere in this test binary's startup.
	if reward := LevelUpReward(3); reward.ItemID != 1002 || reward.Count != 1 {
		t.Fatalf("with no patch point registered the reward is %+v, want the original", reward)
	}
}

func TestAPatchTakesEffectAndRevertUndoesIt(t *testing.T) {
	registry := hotcode.NewRegistry()
	if err := registry.Register(LevelUpPatchPoint, OriginalLevelUpReward); err != nil {
		t.Fatalf("register: %v", err)
	}
	// Resolve off this registry rather than the package default, so the test
	// does not depend on what else in the binary registered what.
	resolve := func() Reward {
		fn, _ := registry.Resolve(LevelUpPatchPoint, OriginalLevelUpReward).(func(int32) Reward)
		return fn(3)
	}
	if got := resolve(); got.ItemID != 1002 {
		t.Fatalf("before the patch the reward is %+v", got)
	}

	patched := func(level int32) Reward { return Reward{ItemID: 1001, Count: int32(level)} }
	if err := registry.Replace(LevelUpPatchPoint, patched, hotcode.Meta{Reason: "reward table was wrong"}); err != nil {
		t.Fatalf("replace: %v", err)
	}
	if got := resolve(); got.ItemID != 1001 || got.Count != 3 {
		t.Fatalf("after the patch the reward is %+v, want the patched one", got)
	}

	if err := registry.Revert(LevelUpPatchPoint); err != nil {
		t.Fatalf("revert: %v", err)
	}
	if got := resolve(); got.ItemID != 1002 || got.Count != 1 {
		t.Fatalf("after the revert the reward is %+v, want the original back", got)
	}
}

// A replacement with a different signature is refused, and the point keeps
// working: a patch that does not fit is an operator error, not an outage.
func TestAPatchWithTheWrongSignatureIsRefused(t *testing.T) {
	registry := hotcode.NewRegistry()
	if err := registry.Register(LevelUpPatchPoint, OriginalLevelUpReward); err != nil {
		t.Fatal(err)
	}
	if err := registry.Replace(LevelUpPatchPoint, func(string) Reward { return Reward{} }, hotcode.Meta{}); err == nil {
		t.Fatal("a replacement with the wrong signature was accepted")
	}
	fn, _ := registry.Resolve(LevelUpPatchPoint, OriginalLevelUpReward).(func(int32) Reward)
	if got := fn(3); got.ItemID != 1002 {
		t.Fatalf("the refused patch disturbed the point: %+v", got)
	}
}
