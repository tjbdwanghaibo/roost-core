// entitysync 基准：Flush、字节预算恢复、预算耗尽窗口与 profile 需求。

package entitysync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
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

// 字节软预算下的整轮恢复：200 个会话各见 50 个 200 字节实体，Hold/Ready 后持续 Flush 直到快照全部交付。
// 用于对照被挡请求是否在每个窗口被重复捕获编码；包含快照打包与组帧，不包含网络。
func BenchmarkByteBudgetRecoveryDrain(b *testing.B) {
	m, err := NewManager(ManagerConfig{Transport: TransportFunc(func(context.Context, SessionID, []byte) error { return nil }), SnapshotBudget: SnapshotBudget{MaxBytes: 16 << 10}})
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = m.Close(context.Background()) })
	payload := make([]byte, 200)
	for id := int64(1); id <= 2000; id++ {
		state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "recovery", Packer: entity.SubjectSyncPackFunc{Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			return entity.CopyFrozenSyncPayload(1, payload), nil
		}}})
		if err := m.Register(state); err != nil {
			b.Fatal(err)
		}
	}
	for sid := 1; sid <= 200; sid++ {
		if err := m.OpenSession(SessionID(sid)); err != nil {
			b.Fatal(err)
		}
		for offset := range 50 {
			if err := m.Subscribe(SessionID(sid), int64((sid*50+offset)%2000+1), entity.SyncProfile{}); err != nil {
				b.Fatal(err)
			}
		}
	}
	drain := func() int {
		windows := 0
		for ; m.Stats().PendingSnapshots > 0; windows++ {
			if err := m.Flush(context.Background()); err != nil {
				b.Fatal(err)
			}
		}
		return windows
	}
	drain()
	before, windows := m.Stats().SnapshotsCaptured, 0
	b.ReportAllocs()
	for b.Loop() {
		for sid := 1; sid <= 200; sid++ {
			if err := m.HoldSession(SessionID(sid)); err != nil {
				b.Fatal(err)
			}
			if err := m.ReadySession(SessionID(sid)); err != nil {
				b.Fatal(err)
			}
		}
		windows += drain()
	}
	b.ReportMetric(float64(windows)/float64(b.N), "windows/op")
	b.ReportMetric(float64(m.Stats().SnapshotsCaptured-before)/float64(b.N), "captures/op")
}

// 每轮恢复 1000 个会话，每人实际订阅 50 个实体；不包含快照编码和网络。
func BenchmarkSessionRecovery(b *testing.B) {
	for _, entities := range []int{1000, 10000} {
		b.Run(fmt.Sprintf("entities=%d/players=1000/visible=50", entities), func(b *testing.B) {
			m, err := NewManager(ManagerConfig{Transport: TransportFunc(func(context.Context, SessionID, []byte) error { return nil })})
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { _ = m.Close(context.Background()) })
			for id := 1; id <= entities; id++ {
				state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: int64(id), Namespace: "recovery"})
				if err := m.Register(state); err != nil {
					b.Fatal(err)
				}
			}
			for sid := 1; sid <= 1000; sid++ {
				if err := m.OpenSession(SessionID(sid)); err != nil {
					b.Fatal(err)
				}
				for offset := 0; offset < 50; offset++ {
					if err := m.Subscribe(SessionID(sid), int64((sid*50+offset)%entities+1), entity.SyncProfile{}); err != nil {
						b.Fatal(err)
					}
				}
			}
			b.ReportAllocs()
			for b.Loop() {
				for sid := 1; sid <= 1000; sid++ {
					if err := m.HoldSession(SessionID(sid)); err != nil {
						b.Fatal(err)
					}
					if err := m.ReadySession(SessionID(sid)); err != nil {
						b.Fatal(err)
					}
				}
			}
		})
	}
}

// 没有新变化的恢复等待者，不应在同一额度窗口被重复扫描。
func BenchmarkExhaustedSnapshotWindow(b *testing.B) {
	m, err := NewManager(ManagerConfig{Transport: TransportFunc(func(context.Context, SessionID, []byte) error { return nil }), Mode: ModeOnChange, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: 1}})
	if err != nil {
		b.Fatal(err)
	}
	for id := int64(1); id <= 10000; id++ {
		state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Packer: entity.SubjectSyncPackFunc{Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			return entity.CopyFrozenSyncPayload(1, []byte{1}), nil
		}}})
		if err := m.Register(state); err != nil {
			b.Fatal(err)
		}
	}
	for sid := 1; sid <= 1000; sid++ {
		if err := m.OpenSession(SessionID(sid)); err != nil {
			b.Fatal(err)
		}
		for offset := range 50 {
			if err := m.Subscribe(SessionID(sid), int64(((sid-1)*10+offset)%10000+1), entity.SyncProfile{}); err != nil {
				b.Fatal(err)
			}
		}
	}
	if err := m.Flush(context.Background()); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	for b.Loop() {
		if err := m.Flush(context.Background()); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkProfileDemand(b *testing.B) {
	for _, count := range []int{1, 2, 8} {
		b.Run(fmt.Sprintf("profiles=%d", count), func(b *testing.B) {
			s := newSubject(nil)
			for i := range 50 {
				s.subscribers[SessionID(i+1)] = &subscription{kind: kindLive, profile: entity.SyncProfile{Key: fmt.Sprint(i % count)}}
			}
			b.ReportAllocs()
			for b.Loop() {
				s.profilesLocked(nil)
			}
		})
	}
}
