package handler

import (
	"context"
	"math"
	"testing"

	player "example.com/planet/game/entities/player"
	syncsender "example.com/planet/game/handler/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/base/errcode"
)

// 两个正数溢出不能被当作成功经验发放；Player 与 World 必须一起保持原值。
func TestExperienceAdditionRejectsOverflow(t *testing.T) {
	subject, counters, client, committer := newDungeonClaimHarnessWithCommitter(t)
	RegisterAddExpNestHandlers()
	sender := syncsender.NewAddExpSender(client)
	ctx := context.Background()
	if _, err := sender.MultiSync_AddExp(ctx, subject.ID(), counters.ID(), 1); err != nil {
		t.Fatal(err)
	}
	before := len(committer.records)
	level := subject.ProfileComp().Level()
	_, err := sender.MultiSync_AddExp(ctx, subject.ID(), counters.ID(), math.MaxInt64)
	if err == nil {
		t.Fatalf("overflow accepted: player exp=%d world exp=%d", subject.Dao().GetExp(), counters.StatsComp().Snapshot().ExpGranted)
	}
	if code, _ := errcode.ClientError(err); code != 100004 {
		t.Fatalf("code=%d, want exp_amount (100004): %v", code, err)
	}
	if subject.Dao().GetExp() != 1 || subject.ProfileComp().Level() != level || counters.StatsComp().Snapshot().ExpGranted != 1 || len(committer.records) != before {
		t.Fatal("rejected experience grant changed player, world or commit records")
	}
}

// 平坦等级曲线允许直接除余；靠近存储上限的大额合法奖励也无需逐级循环。
func TestExperienceGrantAtStorageLimit(t *testing.T) {
	subject, counters, client, committer := newDungeonClaimHarnessWithCommitter(t)
	RegisterAddExpNestHandlers()
	sender := syncsender.NewAddExpSender(client)
	wantLevels := int64(math.MaxInt32) - int64(subject.ProfileComp().Level())
	amount := wantLevels*player.ExpPerLevel + player.ExpPerLevel - 1
	gained, err := sender.MultiSync_AddExp(context.Background(), subject.ID(), counters.ID(), amount)
	if err != nil || int64(gained) != wantLevels || subject.ProfileComp().Level() != math.MaxInt32 || subject.Dao().GetExp() != player.ExpPerLevel-1 || counters.StatsComp().Snapshot().ExpGranted != amount {
		t.Fatalf("valid boundary grant: gained=%d err=%v level=%d exp=%d", gained, err, subject.ProfileComp().Level(), subject.Dao().GetExp())
	}
	before := len(committer.records)
	if _, err := sender.MultiSync_AddExp(context.Background(), subject.ID(), counters.ID(), 1); err == nil || len(committer.records) != before || subject.Dao().GetExp() != player.ExpPerLevel-1 {
		t.Fatal("grant beyond level storage limit was not refused without changes")
	}
}

// World 累计超限时 Player 的经验、等级、属性与升级效果都不变，也没有新的提交记录。
func TestWorldExperienceOverflowLeavesThePlayerUnchanged(t *testing.T) {
	subject, counters, client, committer := newDungeonClaimHarnessWithCommitter(t)
	RegisterAddExpNestHandlers()
	if _, err := nest.RunIsolatedTransaction(context.Background(), &recordingCommitter{}, "seed-world-exp", func() (any, error) {
		counters.Dao().SetExpGranted(math.MaxInt64)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	level := subject.ProfileComp().Level()
	beforeAttrs, ok := subject.AttributeComp().Final()
	if !ok {
		t.Fatal("missing final attributes")
	}
	hp, attack := beforeAttrs.HP, beforeAttrs.Attack
	_, err := syncsender.NewAddExpSender(client).MultiSync_AddExp(context.Background(), subject.ID(), counters.ID(), player.ExpPerLevel)
	afterAttrs, ok := subject.AttributeComp().Final()
	if err == nil || !ok || subject.ProfileComp().Level() != level || subject.Dao().GetExp() != 0 || counters.StatsComp().Snapshot().ExpGranted != math.MaxInt64 || len(committer.records) != 0 || afterAttrs.HP != hp || afterAttrs.Attack != attack {
		t.Fatalf("world overflow changed player, attributes or effect: err=%v", err)
	}
}

// 副本领奖走同一道校验，超限时不得留下“已领取”账本或奖励效果。
func TestWorldExperienceOverflowLeavesTheDungeonClaimUnpaid(t *testing.T) {
	const now int64 = 1_800_000_000
	pinBusinessClock(t, now)
	subject, counters, client, committer := newDungeonClaimHarnessWithCommitter(t)
	if _, err := nest.RunIsolatedTransaction(context.Background(), &recordingCommitter{}, "seed-world-exp-claim", func() (any, error) {
		counters.Dao().SetExpGranted(math.MaxInt64)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	level := subject.ProfileComp().Level()
	_, err := syncsender.NewClaimDungeonSender(client).MultiSync_ClaimDungeon(context.Background(), subject.ID(), counters.ID(), "overflow-run", player.ExpPerLevel, now, now, clearsSeason)
	if err == nil || subject.ProfileComp().DungeonRunClaimed("overflow-run") || subject.ProfileComp().DungeonClears(clearsSeason) != 0 || subject.ProfileComp().Level() != level || subject.Dao().GetExp() != 0 || counters.StatsComp().Snapshot().ExpGranted != math.MaxInt64 || len(committer.records) != 0 {
		t.Fatalf("world overflow left a claim or reward committed: err=%v", err)
	}
}

// 等级的存储类型也是边界；即使只加一级也不能绕回负数。
func TestExperienceLevelRejectsOverflow(t *testing.T) {
	subject, counters, client, committer := newDungeonClaimHarnessWithCommitter(t)
	RegisterAddExpNestHandlers()
	// 仅在测试初始化时写入边界状态；实际发放仍经正式 Sender/Nest 路径。
	if _, err := nest.RunIsolatedTransaction(context.Background(), &recordingCommitter{}, "seed-max-level", func() (any, error) {
		subject.Dao().SetLevel(math.MaxInt32)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := syncsender.NewAddExpSender(client).MultiSync_AddExp(context.Background(), subject.ID(), counters.ID(), 100)
	if err == nil || subject.ProfileComp().Level() != math.MaxInt32 || subject.Dao().GetExp() != 0 || counters.StatsComp().Snapshot().ExpGranted != 0 || len(committer.records) != 0 {
		t.Fatalf("level overflow was not rejected atomically: err=%v level=%d", err, subject.ProfileComp().Level())
	}
}

// 超限的发放必须在任何 DAO 写入之前被拒绝，而不是写完再靠 rollback=undo 撤回：这里在一笔照常提交的事务里
// 直接调用 handler 并吞掉它的错误，提交下来的 Player 经验、等级、副本账本与 World 累计必须与调用前相同。
// （只靠回滚兜底的写法在这里会把已写的 Player 与“已领取”账本提交出去。）
func TestExperienceOverflowIsRefusedBeforeAnyWrite(t *testing.T) {
	const now int64 = 1_800_000_000
	pinBusinessClock(t, now)
	subject, counters, _, _ := newDungeonClaimHarnessWithCommitter(t)
	ctx := context.Background()
	committer := &recordingCommitter{}
	if _, err := nest.RunIsolatedTransaction(ctx, committer, "seed-world-exp", func() (any, error) {
		counters.Dao().SetExpGranted(math.MaxInt64)
		return nil, nil
	}); err != nil {
		t.Fatal(err)
	}
	level := subject.ProfileComp().Level()
	calls := map[string]func() error{
		"add_exp": func() error {
			_, err := handlerAddExp(subject, counters, player.ExpPerLevel)
			return err
		},
		"claim_dungeon": func() error {
			_, err := handlerClaimDungeon(subject, counters, "refused-run", player.ExpPerLevel, now, now, clearsSeason)
			return err
		},
	}
	for name, call := range calls {
		var handlerErr error
		if _, err := nest.RunIsolatedTransaction(ctx, committer, "swallow-"+name, func() (any, error) {
			handlerErr = call()
			return nil, nil
		}); err != nil {
			t.Fatalf("%s: commit the swallowed call: %v", name, err)
		}
		if code, _ := errcode.ClientError(handlerErr); code != 100004 {
			t.Fatalf("%s: code=%d, want exp_amount (100004): %v", name, code, handlerErr)
		}
		if subject.Dao().GetExp() != 0 || subject.ProfileComp().Level() != level || subject.ProfileComp().DungeonRunClaimed("refused-run") || counters.StatsComp().Snapshot().ExpGranted != math.MaxInt64 {
			t.Fatalf("%s wrote before refusing: want level %d, exp=%d level=%d claimed=%v world=%d", name, level, subject.Dao().GetExp(), subject.ProfileComp().Level(), subject.ProfileComp().DungeonRunClaimed("refused-run"), counters.StatsComp().Snapshot().ExpGranted)
		}
	}
}
