package entitysync

import (
	"context"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

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
