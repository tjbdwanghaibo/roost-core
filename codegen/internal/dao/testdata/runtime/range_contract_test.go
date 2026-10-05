//go:build daoruntime

// C7（维护者 2026-10-05）：遍历回调契约——回调里可以读写同一个容器，返回 false 立即停止。
// 生成 DAO 的 RangeX 直接委托给字段的 map，三种 map 选项（默认 small、map=fast、map=sharded）
// 各跑一遍共用辅助 internal/rangecontract；回调里的读写走生成的 GetX / SetX / DelX（含标脏、
// undo 登记），而不是底层 map。N13 观察 O7：同一个 RangeX 在 small / sharded 下是快照遍历、
// 在 fast 下是活遍历——两者都满足契约，差别只在“回调里删掉的未到达键是否仍交出”。
//
// 和 roundtrip_test.go 一样，这个文件不由 `go test ./...` 编译：scripts/dao-golden-runtime.sh 把它、
// golden 与 internal/rangecontract/rangecontract.go 放进一次性模块里跑。
package testdata

import (
	"context"
	"strconv"
	"testing"

	"daogoldenruntime/rangecontract"

	"github.com/tjbdwanghaibo/roost-core/nest"
)

// inTransaction 把填充和每次 RangeX（连同回调里的 SetX / DelX）放进一个真实的隔离事务：
// 生成的 mutator 在事务里标脏、登记 undo，与业务 handler 里的用法相同。
func inTransaction(t testing.TB) func(func()) {
	return func(body func()) {
		if _, err := nest.RunIsolatedTransaction(context.Background(), &ownershipCommitter{}, "range_contract",
			func() (any, error) { body(); return nil, nil }); err != nil {
			t.Errorf("transaction: %v", err)
		}
	}
}

func TestGeneratedRangeXContract(t *testing.T) {
	t.Run("map default (SmallSafeMap)", func(t *testing.T) {
		rangecontract.Check(t, rangecontract.Subject{Tx: inTransaction(t), New: func(testing.TB) rangecontract.Ops {
			hero := NewHeroDao()
			hero.SetId(1)
			return rangecontract.Ops{
				Set: func(k, v int64) { hero.SetItems(k, int32(v)) },
				Get: func(k int64) (int64, bool) {
					v, ok := hero.GetItems(k)
					return int64(v), ok
				},
				Delete: hero.DelItems,
				Range: func(f func(int64, int64) bool) {
					hero.RangeItems(func(k int64, v int32) bool { return f(k, int64(v)) })
				},
			}
		}})
	})
	t.Run("map=fast (FastMap)", func(t *testing.T) {
		rangecontract.Check(t, rangecontract.Subject{Tx: inTransaction(t), Live: true, New: func(testing.TB) rangecontract.Ops {
			variety := NewVarietyDao()
			variety.SetId(1)
			return rangecontract.Ops{
				Set: func(k, v int64) { variety.SetFastItems(k, int32(v)) },
				Get: func(k int64) (int64, bool) {
					v, ok := variety.GetFastItems(k)
					return int64(v), ok
				},
				Delete: variety.DelFastItems,
				Range: func(f func(int64, int64) bool) {
					variety.RangeFastItems(func(k int64, v int32) bool { return f(k, int64(v)) })
				},
			}
		}})
	})
	t.Run("map=sharded (ShardedSafeMap)", func(t *testing.T) {
		rangecontract.Check(t, rangecontract.Subject{Tx: inTransaction(t), New: func(testing.TB) rangecontract.Ops {
			variety := NewVarietyDao()
			variety.SetId(1)
			return rangecontract.Ops{
				Set: func(k, v int64) { variety.SetShardedTags(int32(k), strconv.FormatInt(v, 10)) },
				Get: func(k int64) (int64, bool) {
					v, ok := variety.GetShardedTags(int32(k))
					if !ok {
						return 0, false
					}
					n, _ := strconv.ParseInt(v, 10, 64)
					return n, true
				},
				Delete: func(k int64) { variety.DelShardedTags(int32(k)) },
				Range: func(f func(int64, int64) bool) {
					variety.RangeShardedTags(func(k int32, v string) bool {
						n, _ := strconv.ParseInt(v, 10, 64)
						return f(int64(k), n)
					})
				},
			}
		}})
	})
}
