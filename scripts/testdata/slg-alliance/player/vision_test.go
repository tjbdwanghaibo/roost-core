package alliance

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 敌方目标使用单独的授权发布 Interest，不进入公共空间订阅源。
// 每次通过生成 Sender 在 Nest 锁内同时读已提交的位置和侦察位图，避免从两份缓存拼接权限。
// 目标位置是固定教学地图配置；只复用已持久的玩家视点，未实现敌方目标 DAO、行军速度或碰撞。
func newDurableVision(t *testing.T, n *rewardRuntime) func(...int64) {
	t.Helper()
	m, in, sink := newSpatialView(t)
	for _, id := range []int64{98, 501, 502, 503} {
		id := id
		raw := []byte(fmt.Sprintf(`{"target":%d}`, id))
		if id == 98 {
			raw = []byte(`{"view":true}`)
		}
		check(t, m.Register(entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "authorized-target", Packer: entity.SubjectSyncPackFunc{Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			return entity.TakeFrozenSyncPayload(1, raw), nil
		}}})))
	}
	check(t, in.DefineRelationView(901, entity.SyncProfile{}))
	check(t, in.AttachView(1, 901))
	receivers := []*frame.Receiver{frame.NewReceiver(frame.DefaultLimits()), frame.NewReceiver(frame.DefaultLimits())}
	objects := []map[frame.ObjectRef]int64{{}, {}}
	for _, sid := range []entitysync.SessionID{1, 2} {
		check(t, m.OpenHeldSession(sid))
		check(t, m.Subscribe(sid, 98, entity.SyncProfile{}))
	}
	positions := map[int64]spatial.Point{501: {X: 700, Y: 700}, 502: {X: 710, Y: 700}, 503: {X: 800, Y: 800}}
	return func(want ...int64) {
		t.Helper()
		for _, sid := range []entitysync.SessionID{1, 2} {
			check(t, m.HoldSession(sid))
		}
		raw, e := sender.NewAllianceRewardsSender(n.nest).Sync_AllianceWallet(context.Background(), n.id)
		check(t, e)
		var state struct{ X, Y, VisionMask int64 }
		check(t, json.Unmarshal([]byte(raw), &state))
		if state.X < 0 || state.Y < 0 || state.X >= 1000 || state.Y >= 1000 {
			t.Fatal("invalid durable viewpoint")
		}
		var visible []int64
		for id, at := range positions {
			x, y := state.X-at.X, state.Y-at.Y
			if state.VisionMask&(int64(1)<<uint(id-501)) != 0 && x*x+y*y <= 50*50 {
				visible = append(visible, id)
			}
		}
		slices.Sort(visible)
		check(t, in.SetViewMembers(901, visible))
		if refused := in.Apply(); len(refused) > 0 {
			t.Fatal(refused)
		}
		for _, sid := range []entitysync.SessionID{1, 2} {
			check(t, m.ReadySession(sid))
		}
		check(t, m.Flush(context.Background()))
		for i := range receivers {
			raws := sink.take(entitysync.SessionID(i + 1))
			if len(raws) == 0 {
				t.Fatal("empty world did not send a reset frame")
			}
			for _, raw := range raws {
				applied, e := receivers[i].Receive(raw, func(f frame.Frame) error {
					if f.Kind == frame.Full {
						clear(objects[i])
					}
					for _, o := range f.Objects {
						if o.Operation == frame.ObjectRemove {
							delete(objects[i], o.Ref)
							continue
						}
						for _, component := range o.Components {
							u, e := entitysync.DecodeSubjectUpdate(component.Data, 0)
							if e != nil {
								return e
							}
							if u.SubjectID == 98 && string(u.Payload.BytesCopy()) == `{"view":true}` {
								continue
							}
							if _, ok := positions[u.SubjectID]; !ok || string(u.Payload.BytesCopy()) != fmt.Sprintf(`{"target":%d}`, u.SubjectID) {
								return fmt.Errorf("private or unexpected target payload")
							}
							objects[i][o.Ref] = u.SubjectID
						}
					}
					return nil
				})
				check(t, e)
				if !applied {
					t.Fatal("fresh target frame rejected")
				}
			}
			got := []int64{}
			for _, id := range objects[i] {
				got = append(got, id)
			}
			slices.Sort(got)
			expected := want
			if i == 1 {
				expected = nil
			} // 第二个客户端没有该玩家的侦察授权。
			if !slices.Equal(got, expected) {
				t.Fatalf("client %d targets=%v want=%v at=%+v", i+1, got, expected, state)
			}
		}
	}
}
