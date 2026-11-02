//go:build daoruntime

package testdata

import (
	"context"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 包含正式 setter、事务/undo 登记、持久化准备与 BSON 冻结；提交器为内存替身。
// 同一基准可复制到改动前的生成物上运行，不调用本轮新增 API。
func BenchmarkPersistPaths(b *testing.B) {
	for _, size := range []int{100, 1000} {
		for _, kind := range []string{"sparse", "nested", "dense"} {
			b.Run(fmt.Sprintf("n%d/%s", size, kind), func(b *testing.B) {
				d := collectionHero(size)
				d.DirtyTracker().SetVersion(1)
				seq := int64(2000)
				c := &persistBenchmarkCommitter{}
				b.ReportAllocs()
				for b.Loop() {
					seq++
					c.record = nest.CommitRecord{}
					_, err := nest.RunIsolatedTransaction(context.Background(), c, "persist_bench", func() (any, error) {
						switch kind {
						case "sparse":
							d.SetItems(5, int32(seq))
							d.SetFriendsAt(50, seq)
						case "nested":
							e, _ := d.GetSquad(50)
							e.SetStar(int32(seq))
							item, _ := d.GetEquips(2)
							gem, _ := item.GetGems(3)
							gem.SetLevel(int32(seq))
						case "dense":
							for i := range size {
								d.SetItems(int64(i), int32(seq))
								d.SetFriendsAt(i, seq)
							}
						}
						return nil, nil
					})
					if err != nil {
						b.Fatal(err)
					}
					d.DirtyTracker().AcknowledgeSyncChanges(d.DirtyTracker().SyncChangeVersion())
				}
				raw := c.record.Mutations[0].Patch.SetBSON
				b.ReportMetric(float64(len(raw)), "bson-B/op")
			})
		}
	}
}

type persistBenchmarkCommitter struct{ record nest.CommitRecord }

func (c *persistBenchmarkCommitter) Commit(_ context.Context, record nest.CommitRecord) error {
	if err := dataengine.ValidateCommitRecord(record); err != nil {
		return err
	}
	c.record = record
	return nil
}
