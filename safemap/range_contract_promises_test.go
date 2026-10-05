package safemap

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/internal/rangecontract"
)

// C7（维护者 2026-10-05）：遍历回调契约——回调里可以读写同一个 map，返回 false 立即停止。
// SmallSafeMap / ShardedSafeMap 按快照遍历（回调期间删掉的未到达键仍可能交出）；FastMap 活遍历
// （删掉的不交出，回调里扩容 / Clear 之后到当前表查剩余键，NC-182）。生成 DAO 的 RangeX 直接委托
// 给这三种 map，其组合在 codegen/internal/dao/testdata/runtime/range_contract_test.go 里再跑一遍。

func mapOps(m IMap[int64, int64]) rangecontract.Ops {
	return rangecontract.Ops{
		Set:    m.Set,
		Get:    m.Get,
		Delete: func(k int64) { m.Delete(k) },
		Clear:  m.Clear,
		Range:  m.Range,
	}
}

func TestSafemapRangeContract(t *testing.T) {
	subjects := map[string]rangecontract.Subject{
		"FastMap": {Live: true, New: func(testing.TB) rangecontract.Ops {
			return mapOps(NewInt64FastMap[int64](0))
		}},
		"SmallSafeMap": {New: func(testing.TB) rangecontract.Ops {
			return mapOps(NewSmallSafeMap[int64, int64](0))
		}},
		"ShardedSafeMap": {New: func(testing.TB) rangecontract.Ops {
			return mapOps(NewInt64ShardedSafeMap[int64](4))
		}},
	}
	for name, subject := range subjects {
		t.Run(name, func(t *testing.T) { rangecontract.Check(t, subject) })
	}
}
