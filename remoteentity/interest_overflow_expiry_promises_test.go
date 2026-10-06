package remoteentity

import (
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20261006-11 复核：表满时记下的溢出水位（interestOverflowFence）在最后一次 release + 兴趣 TTL 时失效。
// 修复时兴趣表直接读 time.Now，到期只按代码推理；这里经 registry.now 注入可控时钟验证：
//   - 到期前一刻，迟到的旧续租（代际不新于水位、key 在位图里）仍被挡住；
//   - 到期那一刻起水位清掉、不再挡：同一条旧续租照常建立租约；
//   - 到期时刻随同一 consumer 的后续 release 后移（最后一次 release + TTL）；
//   - 每个 consumer 至多一个水位：大量 release 之后 overflow 条目数等于 consumer 数；到期后由任何一次
//     表满时的清理回收，不需要那个 consumer 再出现。

// overflowTestRegistry 建一个容量为 2、已被 filler 占满的兴趣表，时钟由返回的指针控制。
func overflowTestRegistry(t *testing.T, fence time.Duration) (*remoteInterestRegistry, *int64, entity.RemoteSnapshotKey) {
	t.Helper()
	registry := newRemoteInterestRegistry(remoteInterestLimits{PerConsumer: 4, Total: 2, ReleaseFence: fence})
	clock := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC).UnixNano()
	registry.now = func() int64 { return clock }
	const filler int32 = 1
	keyA := interestKeyFor(t, 245, 9701)
	keyB := interestKeyFor(t, 245, 9702)
	for _, key := range []entity.RemoteSnapshotKey{keyA, keyB} {
		if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: filler, Key: key, ExpiresAt: clock + time.Hour.Nanoseconds(), Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	return registry, &clock, keyA
}

func TestInterestOverflowFenceExpiresOneTTLAfterTheLastRelease(t *testing.T) {
	const fence = 30 * time.Second
	registry, clock, fillerKey := overflowTestRegistry(t, fence)
	const consumer int32 = 7
	keyC := interestKeyFor(t, 245, 9703)
	keyD := interestKeyFor(t, 245, 9704)
	start := *clock

	// 表满时撤销 C（代际 21），10 秒后再撤销 D（代际 23）：同一个 consumer 的水位，到期时刻后移到第二次 + TTL。
	registry.release(keyC, consumer, 21)
	*clock = start + (10 * time.Second).Nanoseconds()
	registry.release(keyD, consumer, 23)
	if got := len(registry.overflow); got != 1 {
		t.Fatalf("overflow entries = %d after two releases by one consumer, want 1", got)
	}
	lastRelease := *clock
	registry.drop(fillerKey, 1, 1) // 表里空出一格：之后能否建立只由水位决定

	late := func() entity.RemoteSnapshotInterest {
		return entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: keyC, ExpiresAt: *clock + time.Minute.Nanoseconds(), Generation: 20}
	}

	// 第一次 release 的 TTL 已过、最后一次的还没过：仍然挡住。
	*clock = start + fence.Nanoseconds() + 1
	if err := registry.renew(late()); err != nil {
		t.Fatalf("late renewal: %v", err)
	}
	if registry.interested(keyC) {
		t.Fatal("the overflow fence expired one TTL after the first release; it must last until one TTL after the last")
	}
	// 到期前一刻：仍然挡住。
	*clock = lastRelease + fence.Nanoseconds() - 1
	if err := registry.renew(late()); err != nil {
		t.Fatalf("late renewal: %v", err)
	}
	if registry.interested(keyC) {
		t.Fatal("a renewal issued before the release revived the withdrawn lease before the overflow fence expired")
	}
	if got := len(registry.overflow); got != 1 {
		t.Fatalf("overflow entries = %d before expiry, want 1", got)
	}

	// 到期那一刻：水位清掉，同一条旧续租照常建立（此时任何撤销之前发出的真实续租都已过期，见 interestLease）。
	*clock = lastRelease + fence.Nanoseconds()
	if err := registry.renew(late()); err != nil {
		t.Fatalf("renewal after the fence expired: %v", err)
	}
	if !registry.interested(keyC) {
		t.Fatal("the overflow fence still refused a renewal after it expired")
	}
	if got := len(registry.overflow); got != 0 {
		t.Fatalf("overflow entries = %d after expiry, want 0", got)
	}
}

func TestInterestOverflowFencesAreOnePerConsumerAndReclaimedOnExpiry(t *testing.T) {
	const fence = 30 * time.Second
	registry, clock, _ := overflowTestRegistry(t, fence)
	start := *clock

	// 两个 consumer 在表满时各撤销大量 key：每个 consumer 只占一个溢出水位，代际取最大。
	for i := range 1000 {
		registry.release(interestKeyFor(t, 245, 10_000+int64(i)), 7, uint64(100+i))
	}
	for i := range 500 {
		registry.release(interestKeyFor(t, 245, 20_000+int64(i)), 8, uint64(50+i))
	}
	if got := len(registry.overflow); got != 2 {
		t.Fatalf("overflow entries = %d after 1500 releases by two consumers, want 2", got)
	}
	if got := registry.overflow[7].generation; got != 1099 {
		t.Fatalf("consumer 7 fence generation = %d, want the largest released (1099)", got)
	}
	if registry.total != 2 {
		t.Fatalf("registry entries = %d, want the two filler leases only (overflow fences take no table capacity)", registry.total)
	}

	// 两个水位都到期之后，另一个 consumer 在表满时的一次 release 触发清理：过期水位被回收，不需要
	// consumer 7 / 8 再出现；只剩新的那一个。
	*clock = start + fence.Nanoseconds()
	registry.release(interestKeyFor(t, 245, 30_000), 9, 1)
	if got := len(registry.overflow); got != 1 || registry.overflow[9] == nil {
		t.Fatalf("overflow after expiry = %d entries (consumer 9 present: %v), want only consumer 9's", got, registry.overflow[9] != nil)
	}
}
