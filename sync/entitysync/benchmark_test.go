package entitysync

import (
	"context"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// BenchmarkManagerFlush 测量稳定订阅下的一次脏更新 tick，包含 MarkDirty、
// 捕获、组件编码、会话组帧和内存传输准入；不计连接建立，也不代表网络吞吐。
func BenchmarkManagerFlush(b *testing.B) {
	for _, sessions := range []int{1, 64, 256} {
		for _, profiles := range []int{1, 4} {
			if profiles > sessions {
				continue
			}
			b.Run(fmt.Sprintf("sessions=%d/subjects=32/profiles=%d/payload=256", sessions, profiles), func(b *testing.B) {
				ctx := context.Background()
				var bytes, frames, packs int64
				manager, err := NewManager(ManagerConfig{Transport: TransportFunc(func(_ context.Context, _ SessionID, data []byte) error {
					bytes += int64(len(data))
					frames++
					return nil
				})})
				if err != nil {
					b.Fatal(err)
				}
				b.Cleanup(func() {
					if err := manager.Close(ctx); err != nil {
						b.Error(err)
					}
				})
				payload := entity.CopyFrozenSyncPayload(1, make([]byte, 256))
				states := make([]*entity.SubjectSyncState, 32)
				for i := range states {
					states[i] = entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: int64(i + 1), Namespace: "bench", Packer: entity.SubjectSyncPackFunc{
						Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) { packs++; return payload, nil },
						Delta:    func(entity.SyncProfile, uint64) (entity.FrozenSyncPayload, error) { packs++; return payload, nil },
					}})
					if err := manager.Register(states[i]); err != nil {
						b.Fatal(err)
					}
				}
				for sid := 1; sid <= sessions; sid++ {
					id := SessionID(sid)
					if err := manager.OpenSession(id); err != nil {
						b.Fatal(err)
					}
					for _, state := range states {
						if err := manager.Subscribe(id, state.SubjectID(), entity.SyncProfile{Key: fmt.Sprintf("p%d", sid%profiles)}); err != nil {
							b.Fatal(err)
						}
					}
				}
				if err := manager.Flush(ctx); err != nil {
					b.Fatal(err)
				}
				bytes, frames, packs = 0, 0, 0
				b.ReportAllocs()
				for b.Loop() {
					for _, state := range states {
						state.MarkDirty(1)
					}
					if err := manager.Flush(ctx); err != nil {
						b.Fatal(err)
					}
				}
				b.ReportMetric(float64(frames)/float64(b.N), "frames/op")
				b.ReportMetric(float64(packs)/float64(b.N), "packs/op")
				b.ReportMetric(float64(bytes)/float64(b.N), "wire-B/op")
			})
		}
	}
}
