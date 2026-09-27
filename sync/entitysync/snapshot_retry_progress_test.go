package entitysync

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// OPEN-ITEMS B37：RR-20260926-05 修复记录的边界“持续 RetryLater（传输长时间不可用）时会话间公平性只按规则推理”。
// 验证：冷预算按窗口受限、传输在每个窗口都只接下第一帧就回 RetryLater、最低 ID 的会话每个窗口还持续新增一个小的冷对象
// 时，其余每个会话的冷创建仍在有界窗口数内交付——游标跟随真实尝试、不越过未尝试者，失败的那次尝试照常计费、
// 下一窗口从它之后继续，不会让同一批会话一直排在 RetryLater 之后。periodic（每次 Flush 一个窗口）与 on_change
// （按 Interval 轮转窗口，这里把窗口起点前移一个 Interval 来确定性轮转，不 sleep）各跑一遍。
func TestEverySessionIsDeliveredUnderPersistentRetryLater(t *testing.T) {
	const (
		sessions   = 6
		maxObjects = 2
		// 每个窗口只成功一帧；最低 ID 会话每窗口新增一个对象，与其余会话竞争同一份额度。界取会话数的 4 倍。
		windowBound = 4 * sessions
	)
	for _, mode := range []SyncMode{ModePeriodic, ModeOnChange} {
		t.Run(mode.String(), func(t *testing.T) {
			recorder := newRecordingTransport()
			pushes := 0
			transport := TransportFunc(func(ctx context.Context, sid SessionID, raw []byte) error {
				pushes++
				if pushes > 1 {
					return ErrRetryLater // 本窗口的第二帧起传输不可用
				}
				return recorder.Push(ctx, sid, raw)
			})
			m := newTestManager(t, transport, ManagerConfig{Mode: mode, Interval: time.Hour, SnapshotBudget: SnapshotBudget{MaxObjects: maxObjects}})
			ids := make([]SessionID, 0, sessions)
			for sid := SessionID(1); sid <= sessions; sid++ {
				ids = append(ids, sid)
			}
			open(t, m, ids...)
			packs := 0
			for _, sid := range ids {
				if err := m.Register(testSubject(t, int64(sid)*100, &packs)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, sid, int64(sid)*100, entity.SyncProfile{})
			}

			deliveredAt := make(map[SessionID]int)
			nextArrival := int64(10_000)
			retries := 0
			for window := 1; window <= windowBound && len(deliveredAt) < sessions; window++ {
				// 最低 ID 的会话持续有新的小对象入场。
				nextArrival++
				if err := m.Register(testSubject(t, nextArrival, &packs)); err != nil {
					t.Fatal(err)
				}
				mustSubscribe(t, m, 1, nextArrival, entity.SyncProfile{})
				if mode == ModeOnChange && !m.budgetWindow.IsZero() {
					m.budgetWindow = m.budgetWindow.Add(-m.config.Interval) // 进入下一个窗口
				}
				pushes = 0
				if err := m.Flush(t.Context()); err != nil {
					if !errors.Is(err, ErrRetryLater) {
						t.Fatalf("window %d: %v", window, err)
					}
					retries++
				}
				for _, sid := range ids {
					recorder.take(sid)
					if _, done := deliveredAt[sid]; !done && m.sessionHoldsSubject(sid, int64(sid)*100) {
						deliveredAt[sid] = window
					}
				}
			}
			t.Logf("%s: first delivery window per session=%v, windows ending in RetryLater=%d", mode, deliveredAt, retries)
			if retries == 0 {
				t.Fatal("premise: no window ended in RetryLater")
			}
			var missing []string
			for _, sid := range ids {
				if _, done := deliveredAt[sid]; !done {
					missing = append(missing, fmt.Sprint(sid))
				}
			}
			if len(missing) > 0 {
				t.Fatalf("sessions %v never received their cold create within %d windows of persistent RetryLater (delivered=%v)", missing, windowBound, deliveredAt)
			}
		})
	}
}
