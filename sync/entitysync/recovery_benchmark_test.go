package entitysync

import (
	"context"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// 字节软预算下的整轮恢复：200 个会话各见 50 个 200 字节实体，Hold/Ready 后
// 持续 Flush 直到快照全部交付。用于对照被挡请求是否在每个窗口被重复捕获编码
// （RR-20260926-04）；包含快照打包与组帧，不包含网络。
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
