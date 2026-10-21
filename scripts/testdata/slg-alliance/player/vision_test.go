package alliance

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"testing"

	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"example.com/slg-alliance-practice/game/scout"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 敌方目标使用单独的授权发布 Interest，不进入公共空间订阅源。
// 每次通过生成 Sender 在 Nest 锁内同时读已提交的位置和侦察位图，避免从两份缓存拼接权限。
// 目标位置与命令回执归独立 ScoutMap，联合快照只在本服 Nest 内成立。
type durableVision struct {
	refresh  func(context.Context) error
	expect   func(...int64)
	load     func(context.Context) (string, error)
	noFrames func()
}

func newDurableVision(t *testing.T, n *rewardRuntime, business clock.Business) *durableVision {
	t.Helper()
	m, in, sink := newSpatialView(t)
	positions := map[int64]spatial.Point{}
	for _, id := range []int64{98, 501, 502, 503, 504} {
		id := id
		raw := []byte(fmt.Sprintf(`{"target":%d}`, id))
		if id == 98 {
			raw = []byte(`{"view":true}`)
		}
		check(t, m.Register(entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "authorized-target", Packer: entity.SubjectSyncPackFunc{Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			if id != 98 {
				at, ok := positions[id]
				if !ok {
					return entity.FrozenSyncPayload{}, fmt.Errorf("missing committed target")
				}
				raw = []byte(fmt.Sprintf(`{"target":%d,"x":%d,"y":%d}`, id, at.X, at.Y))
			}
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
	v := &durableVision{}
	v.load = func(ctx context.Context) (string, error) {
		return sender.NewAllianceRewardsSender(n.nest).MultiSync_AllianceVision(ctx, n.id, n.mapID)
	}
	v.noFrames = func() {
		for _, id := range []entitysync.SessionID{1, 2} {
			if len(sink.take(id)) != 0 {
				t.Fatal("failed authority read published a frame")
			}
		}
	}
	v.refresh = func(ctx context.Context) error {
		for _, sid := range []entitysync.SessionID{1, 2} {
			if err := m.HoldSession(sid); err != nil {
				return err
			}
		}
		raw, e := v.load(ctx)
		if e != nil {
			return e
		}
		if e = ctx.Err(); e != nil {
			return e
		}
		var state struct {
			X, Y, VisionMask, ScoutUntil int64
			Revision                     uint64
			Targets                      map[int64]spatial.Point
			Routes                       map[int64]scout.Route
		}
		if e = json.Unmarshal([]byte(raw), &state); e != nil {
			return e
		}
		if state.X < 0 || state.Y < 0 || state.X >= 1000 || state.Y >= 1000 {
			return fmt.Errorf("invalid durable viewpoint")
		}
		if state.Revision == 0 || len(state.Targets) != 4 {
			return fmt.Errorf("invalid target snapshot")
		}
		for id, at := range state.Targets {
			if id < 501 || id > 504 || at.X < 0 || at.Y < 0 || at.X >= 1000 || at.Y >= 1000 {
				return fmt.Errorf("invalid target coordinate")
			}
		}
		positions = state.Targets
		// 期限是业务事实，过期时不延长、不删档；读不到合法期限即不授予临时视野。
		now := business.Now().UnixMilli()
		for id, route := range state.Routes {
			if _, ok := positions[id]; !ok {
				return fmt.Errorf("route for unknown target")
			}
			at, err := route.At(now)
			if err != nil {
				return err
			}
			positions[id] = at
		}
		var visible []int64
		for id, at := range positions {
			x, y := state.X-at.X, state.Y-at.Y
			if id == 504 && (state.ScoutUntil <= 0 || now >= state.ScoutUntil) {
				continue
			}
			if state.VisionMask&(int64(1)<<uint(id-501)) != 0 && x*x+y*y <= 50*50 {
				visible = append(visible, id)
			}
		}
		slices.Sort(visible)
		if e = in.SetViewMembers(901, visible); e != nil {
			return e
		}
		if refused := in.Apply(); len(refused) > 0 {
			return fmt.Errorf("view application refused: %v", refused)
		}
		for _, sid := range []entitysync.SessionID{1, 2} {
			if e = m.ReadySession(sid); e != nil {
				return e
			}
		}
		return m.Flush(ctx)
	}
	v.expect = func(want ...int64) {
		t.Helper()
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
							if _, ok := positions[u.SubjectID]; !ok || string(u.Payload.BytesCopy()) != fmt.Sprintf(`{"target":%d,"x":%d,"y":%d}`, u.SubjectID, positions[u.SubjectID].X, positions[u.SubjectID].Y) {
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
				t.Fatalf("client %d targets=%v want=%v", i+1, got, expected)
			}
		}
	}
	return v
}
