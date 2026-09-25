package entitysync

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

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
