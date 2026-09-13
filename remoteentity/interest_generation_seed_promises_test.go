package remoteentity

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0184 复核补修 · C8 · RR-20260913-02 观察:同一进程里相继创建的两个 Manager(重启 harness、
// Windows 的粗时钟)若落进同一个 UnixNano 刻度,第二个的首个代际会低于第一个已发出的代际,
// owner 端按"落后代际忽略"的规则会丢掉新实例的 renewal。审查在 Windows 上两次实测
// `TestManagerStampsInterestMessagesWithAdvancingGenerations` 失败(新 …501 < 旧 …503)。
// 承诺:播种不只看时钟,还要高于本进程已发出的任何代际。这里把时钟冻住,让顺序不再依赖纳秒分辨率。
func TestInterestGenerationSeedAdvancesPastIssuedGenerationsUnderAFrozenClock(t *testing.T) {
	key := interestKeyFor(t, 250, 9343)
	previous := interestGenerationNow
	interestGenerationNow = func() uint64 { return 1_700_000_000_000_000_000 }
	t.Cleanup(func() { interestGenerationNow = previous })

	captured := make([]entity.RemoteSnapshotInterest, 0, 4)
	publisher := &capturingInterestPublisher{onPublish: func(i entity.RemoteSnapshotInterest, _ bool) {
		captured = append(captured, i)
	}}
	ctx := context.Background()
	first := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	first.syncer = publisher
	// renew → release → renew:三条消息、三个递增代际(同 key 的连续 renew 会被去重)。
	if err := first.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := first.ReleaseRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if err := first.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	restarted := NewManager(newMockVersionedLockFactory(), DefaultConfig(), 1000)
	restarted.syncer = publisher
	if err := restarted.RenewRemoteSnapshotInterest(ctx, key); err != nil {
		t.Fatal(err)
	}
	if len(captured) != 4 {
		t.Fatalf("captured %d messages, want 4", len(captured))
	}
	for i := 1; i < len(captured); i++ {
		if captured[i].Generation <= captured[i-1].Generation {
			t.Fatalf("a restarted manager issued generation %d, not above the previous %d (same clock tick)", captured[i].Generation, captured[i-1].Generation)
		}
	}
}
