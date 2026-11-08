//go:build terrainexp

package terrain

import (
	"bytes"
	"compress/flate"
	"context"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

type discardCommitter struct{ setBytes int }

func (c *discardCommitter) Commit(_ context.Context, rec nest.CommitRecord) error {
	c.setBytes = len(rec.Mutations[0].Patch.SetBSON)
	return nil
}

func benchCases() []struct {
	name string
	idx  []int
} {
	out := []struct {
		name string
		idx  []int
	}{}
	for _, r := range []int{1, 5, 20, 32} {
		out = append(out, struct {
			name string
			idx  []int
		}{fmt.Sprintf("r%d", r), disk(32, 32, r)})
	}
	return append(out, struct {
		name string
		idx  []int
	}{"flatten", allPoints()})
}

// 每次迭代 = 一次编辑：Nest 隔离事务（setter + undo 登记 + 持久化 BSON 冻结，提交器为内存替身）
// + 同步编码 + 确认同步变化。不含 WAL / Mongo / 网络 / Nest 队列。
func BenchmarkTerrainEdit(b *testing.B) {
	base := baseHeights(0)
	for _, c := range benchCases() {
		b.Run("A/"+c.name, func(b *testing.B) {
			d := newBytesDao(encodeLE(base))
			committer := &discardCommitter{}
			var raw []byte
			sign := 1
			b.ReportAllocs()
			for b.Loop() {
				sign = -sign
				if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "terrain", func() (any, error) {
					brushBytes(d, c.idx, sign)
					return nil, nil
				}); err != nil {
					b.Fatal(err)
				}
				raw = d.MarshalSyncChanges(maskSyncA)
				d.DirtyTracker().AcknowledgeSyncChanges(d.DirtyTracker().SyncChangeVersion())
			}
			b.ReportMetric(float64(len(raw)), "sync-B/op")
			b.ReportMetric(float64(committer.setBytes), "bson-B/op")
		})
		b.Run("B/"+c.name, func(b *testing.B) {
			d := newListDao(base)
			committer := &discardCommitter{}
			var raw []byte
			sign := 1
			b.ReportAllocs()
			for b.Loop() {
				sign = -sign
				if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "terrain", func() (any, error) {
					brushList(d, c.idx, sign)
					return nil, nil
				}); err != nil {
					b.Fatal(err)
				}
				raw = d.MarshalSyncChanges(maskSyncB)
				d.DirtyTracker().AcknowledgeSyncChanges(d.DirtyTracker().SyncChangeVersion())
			}
			b.ReportMetric(float64(len(raw)), "sync-B/op")
			b.ReportMetric(float64(committer.setBytes), "bson-B/op")
		})
		// B 的大笔刷守卫：取副本改完后一次 SetHeightsAll（整字段同步 + 整字段持久）。
		b.Run("Bsetall/"+c.name, func(b *testing.B) {
			d := newListDao(base)
			committer := &discardCommitter{}
			var raw []byte
			sign := 1
			b.ReportAllocs()
			for b.Loop() {
				sign = -sign
				if _, err := nest.RunIsolatedTransaction(context.Background(), committer, "terrain", func() (any, error) {
					h := make([]uint16, 0, points)
					d.RangeHeights(func(_ int, v uint16) bool { h = append(h, v); return true })
					for _, i := range c.idx {
						h[i] = uint16(int(h[i]) + sign)
					}
					d.SetHeightsAll(h)
					d.SetVersion(d.GetVersion() + 1)
					return nil, nil
				}); err != nil {
					b.Fatal(err)
				}
				raw = d.MarshalSyncChanges(maskSyncB)
				d.DirtyTracker().AcknowledgeSyncChanges(d.DirtyTracker().SyncChangeVersion())
			}
			b.ReportMetric(float64(len(raw)), "sync-B/op")
			b.ReportMetric(float64(committer.setBytes), "bson-B/op")
		})
	}
}

// A 选择业务层压缩时每次编辑额外付出的 CPU：解压整块 + 压缩整块（8192 字节 LE）。
func BenchmarkTerrainFlate(b *testing.B) {
	le := encodeLE(baseHeights(0))
	for _, level := range []struct {
		name  string
		value int
	}{{"default", flate.DefaultCompression}, {"bestspeed", flate.BestSpeed}} {
		b.Run("compress/"+level.name, func(b *testing.B) {
			var out []byte
			b.ReportAllocs()
			for b.Loop() {
				out = deflate(b, le, level.value)
			}
			b.ReportMetric(float64(len(out)), "out-B/op")
		})
	}
	compressed := deflate(b, le, flate.DefaultCompression)
	b.Run("decompress", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if out := inflate(b, compressed); !bytes.Equal(out[:2], le[:2]) {
				b.Fatal("inflate")
			}
		}
	})
}

// 客户端（Go）应用一份 B 补丁的耗时：以上一份全量视图为基线。
func BenchmarkTerrainClientApplyB(b *testing.B) {
	base := baseHeights(0)
	for _, c := range benchCases() {
		b.Run(c.name, func(b *testing.B) {
			d := newListDao(base)
			previous, _ := UnmarshalSyncTerrainListDao(d.MarshalSync(^uint64(0)))
			commit(b, func() { brushList(d, c.idx, 1) })
			raw := d.MarshalSyncChanges(maskSyncB)
			b.ReportAllocs()
			for b.Loop() {
				p, err := UnmarshalSyncTerrainListDao(raw)
				if err != nil {
					b.Fatal(err)
				}
				if _, err := ApplySyncTerrainListDao(p, previous); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
