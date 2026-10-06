package remoteentity

import (
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// RR-20261006-11：兴趣表满时 release 不留撤销水位，迟到的旧续租复活已撤销的租约。
//
// 承诺（interestLease 注释，Mirror 第 4 步）：consumer 以代际 g 撤销后，在撤销之前发出（代际 ≤ g）、
// 撤销之后才到的续租不能把租约带回来。release 落到一个不存在的条目（续租还在路上，或续租曾因表满被拒、
// 等待重投）且表已满时，旧实现只撤销、不留水位；之后表里空出一格，迟到的旧续租就建起一条 consumer
// 已经撤销的租约：owner 继续给它推送这个 key，租约占着 consumer 的配额和节点容量，直到续租自己的到期时刻。
func TestInterestReleaseOnAFullRegistryStillFencesTheLateRenewal(t *testing.T) {
	registry := newRemoteInterestRegistry(remoteInterestLimits{PerConsumer: 4, Total: 2})
	keyA := interestKeyFor(t, 244, 9601)
	keyB := interestKeyFor(t, 244, 9602)
	keyC := interestKeyFor(t, 244, 9603)
	keyD := interestKeyFor(t, 244, 9604)
	expires := time.Now().Add(time.Minute).UnixNano()
	const filler, consumer int32 = 1, 7

	// 表满：两个 filler 租约。
	for _, key := range []entity.RemoteSnapshotKey{keyA, keyB} {
		if err := registry.renew(entity.RemoteSnapshotInterest{ConsumerSID: filler, Key: key, ExpiresAt: expires, Generation: 1}); err != nil {
			t.Fatal(err)
		}
	}
	// consumer 先续租 C（代际 20），再撤销 C（代际 21）；线上 release 先到，C 的条目还不存在。
	registry.release(keyC, consumer, 21)
	// 表里空出一格（A 的租约被清掉，与到期清理同样不留水位）。
	registry.drop(keyA, filler, 1)

	late := entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: keyC, ExpiresAt: expires, Generation: 20}
	if err := registry.renew(late); err != nil {
		t.Fatalf("late renewal: %v", err)
	}
	if registry.interested(keyC) {
		t.Fatal("a renewal issued before the release brought the withdrawn lease back after the full registry dropped the watermark")
	}
	if got := registry.consumerLeases(consumer); got != 0 {
		t.Fatalf("consumer holds %d leases after withdrawing its only interest, want 0", got)
	}

	// 撤销之后发出的续租（代际更新）照常建立租约，不被这次撤销挡住。
	fresh := entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: keyD, ExpiresAt: expires, Generation: 22}
	if err := registry.renew(fresh); err != nil {
		t.Fatalf("a renewal issued after the release: %v", err)
	}
	if !registry.interested(keyD) {
		t.Fatal("a renewal issued after the release was refused")
	}

	// 退路只挡被撤销的 key（指纹位图）：同一窗口里另一个 key 的迟到旧续租，指纹不碰撞时照常建立。
	cWord, cMask := interestOverflowBit(keyC, consumer)
	var keyE entity.RemoteSnapshotKey
	for unique := int64(9605); ; unique++ {
		keyE = interestKeyFor(t, 244, unique)
		if word, mask := interestOverflowBit(keyE, consumer); word != cWord || mask != cMask {
			break
		}
	}
	registry.drop(keyD, consumer, 22) // 腾出一格
	other := entity.RemoteSnapshotInterest{ConsumerSID: consumer, Key: keyE, ExpiresAt: expires, Generation: 19}
	if err := registry.renew(other); err != nil {
		t.Fatalf("late renewal of another key: %v", err)
	}
	if !registry.interested(keyE) {
		t.Fatal("the overflow fence refused a renewal of a key that was never released")
	}
}
