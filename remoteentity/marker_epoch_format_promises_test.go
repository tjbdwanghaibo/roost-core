package remoteentity

import (
	"context"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// U-0178 · C8 · RR-20260913-11：所有权 lease 的线格式必须始终是精确十进制。
//
// EnterShared / LeaveShared / Transfer 的 Lua 用 `.. ((tonumber(x) or 0) + 1)` 拼新 lease,
// 拼进去的是一个 Lua 数值,而 Lua 5.1 用 "%.14g" 渲染数值:10^14 会变成 "1e+14"。Go 侧的
// parseMarkerLease 只接受十进制 uint64,于是解析失败 —— 而失败发生在 Redis **已经写下坏记录
// 之后**,不是安全地拒绝输入。再次 GetOwnership 也一样失败,这份所有权记录就此不可读。
//
// 值在声明的 uint64 域内,所以要么精确写出,要么在写之前就拒绝。
func TestMarkerLeaseStaysExactDecimalAtLargeEpochs(t *testing.T) {
	ctx := context.Background()
	for _, epoch := range []uint64{1 << 46, 100_000_000_000_000, 1<<53 + 1} {
		stub := newMarkerEvalStub()
		store := newRedisMarkerForEval(stub, "marks")
		// A lease already at a large epoch, in the exact form Go writes.
		seed := entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: epoch, RouteEpoch: 1}
		stub.values["42"] = formatMarkerLease(seed)

		shared, err := store.EnterSharedExpected(ctx, 42, seed)
		if err != nil {
			t.Fatalf("epoch %d: enter shared = %v; the record was rewritten before this failed", epoch, err)
		}
		if shared.MarkerEpoch != epoch+1 {
			t.Fatalf("epoch %d: new epoch = %d, want %d", epoch, shared.MarkerEpoch, epoch+1)
		}
		// And the stored record must still be readable.
		got, ok, err := store.GetOwnership(ctx, 42)
		if err != nil || !ok {
			t.Fatalf("epoch %d: reading back the lease = ok:%v err:%v", epoch, ok, err)
		}
		if got.MarkerEpoch != epoch+1 || !got.Shared {
			t.Fatalf("epoch %d: read back %#v", epoch, got)
		}
	}
}
