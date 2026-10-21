package alliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	player "example.com/slg-alliance-practice/game/entities/player"
	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync/policy"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/base/spatial"
)

// 位置唯一；覆盖是额外被观察块。两个固定观察视点仅供空间验收，不自动获得敌军侦察权限。
func newSpatialView(t *testing.T) (*entitysync.Manager, *policy.Interest, *viewSink) {
	sink := newViewSink(t)
	m, e := entitysync.NewManager(entitysync.ManagerConfig{Transport: sink})
	check(t, e)
	t.Cleanup(func() { check(t, m.Close(context.Background())) })
	in, e := policy.NewInterest(policy.InterestConfig{Manager: m, AOI: policy.AOIConfig{Bounds: spatial.Rect{Max: spatial.Point{X: 1000, Y: 1000}}, BlockSize: 100, EnterRadius: 20, LeaveRadius: 30}})
	check(t, e)
	t.Cleanup(in.Close)
	check(t, in.DefineRelationView(900, entity.SyncProfile{}))
	check(t, in.AttachView(2, 900))
	return m, in, sink
}

type positionClient struct {
	receiver *frame.Receiver
	objects  map[frame.ObjectRef]map[string]int64
}

func newPositionClient() *positionClient {
	return &positionClient{frame.NewReceiver(frame.DefaultLimits()), map[frame.ObjectRef]map[string]int64{}}
}
func (c *positionClient) consume(t *testing.T, raws [][]byte, id int64) {
	for _, raw := range raws {
		applied, e := c.receiver.Receive(raw, func(f frame.Frame) error {
			if f.Kind == frame.Full {
				clear(c.objects)
			}
			for _, o := range f.Objects {
				if o.Operation == frame.ObjectRemove {
					delete(c.objects, o.Ref)
					continue
				}
				for _, component := range o.Components {
					u, e := entitysync.DecodeSubjectUpdate(component.Data, 0)
					if e != nil {
						return e
					}
					var v map[string]int64
					if e = json.Unmarshal(u.Payload.BytesCopy(), &v); e != nil {
						return e
					}
					if u.SubjectID != id || len(v) != 2 {
						return fmt.Errorf("unexpected or private position payload")
					}
					if _, ok := v["x"]; !ok {
						return fmt.Errorf("missing x")
					}
					if _, ok := v["y"]; !ok {
						return fmt.Errorf("missing y")
					}
					c.objects[o.Ref] = v
				}
			}
			return nil
		})
		check(t, e)
		if !applied {
			t.Fatal("fresh position frame refused")
		}
	}
}

// 只在本次 Runtime 尚未启动移动驱动时装配；冷事实已经经 Nest 读取并与崩溃前字节比较。
// 重建是已提交事实的派生索引恢复，不重新发移动命令，不增加 WAL 或覆盖冷位置。
func verifyPlayerSpatial(t *testing.T, n *rewardRuntime, write bool) {
	t.Helper()
	ctx := context.Background()
	value, e := n.access.Get(ctx, n.id, entity.EntityCategoryOther)
	check(t, e)
	p := value.(*player.Player)
	pack := func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
		raw, e := json.Marshal(map[string]int64{"x": p.Dao().GetX(), "y": p.Dao().GetY()})
		return entity.TakeFrozenSyncPayload(1, raw), e
	}
	p.Base().EnableSync(entity.EntitySyncCreateParam{Enabled: true, EntityID: n.id, Namespace: "slg-position", Packer: entity.SubjectSyncPackFunc{Snapshot: pack, Delta: func(profile entity.SyncProfile, _ uint64) (entity.FrozenSyncPayload, error) { return pack(profile) }}})
	check(t, n.syncer.Register(p.Base().Sync()))
	check(t, n.interest.Show(n.id, spatial.Point{X: p.Dao().GetX(), Y: p.Dao().GetY()}))
	var coverage []int64
	check(t, json.Unmarshal([]byte(p.Dao().GetCoverage()), &coverage))
	check(t, n.interest.QueueObservedBlocks(p, coverage))
	clients := []*positionClient{newPositionClient(), newPositionClient()}
	for i, id := range []entitysync.SessionID{1, 2} {
		check(t, n.syncer.OpenHeldSession(id))
		check(t, n.interest.Watch(int64(id), spatial.Point{X: 10 + int64(i)*200, Y: 10}, policy.ObserverView{}))
		check(t, n.syncer.ReadySession(id))
	}
	expect := func(first, second bool) {
		check(t, n.syncer.Flush(ctx))
		for i, want := range []bool{first, second} {
			clients[i].consume(t, n.sink.take(entitysync.SessionID(i+1)), n.id)
			if (len(clients[i].objects) == 1) != want {
				t.Fatalf("view %d objects=%v want visible=%v", i+1, clients[i].objects, want)
			}
			if want {
				for _, at := range clients[i].objects {
					if at["x"] != 700 || at["y"] != 700 {
						t.Fatal("wrong committed position", at)
					}
				}
			}
		}
	}
	// 测试时钟只注入服务端视野刷新，不从客户端接收时间，不修改全局时钟。
	deadline := p.Dao().GetScoutUntil()
	if deadline <= 0 {
		t.Fatal("missing durable scout deadline")
	}
	now := deadline - 1
	if !write {
		now = deadline + 1
	} // 离线越过期限；先前完整冷事实比较包含此期限。
	view := newDurableVision(t, n, clock.BusinessFunc(func() time.Time { return time.UnixMilli(now) }))
	vision := func(want ...int64) {
		check(t, view.refresh(ctx))
		view.expect(want...)
	}
	replayInit := func() {
		s := sender.NewAllianceRewardsSender(n.nest)
		before, err := s.Sync_AllianceWallet(ctx, n.id)
		check(t, err)
		_, err = s.Sync_AllianceHomeInit(ctx, n.id, p.Dao().GetHomeSID())
		check(t, err)
		after, err := s.Sync_AllianceWallet(ctx, n.id)
		check(t, err)
		if before != after {
			t.Fatal("duplicate init renewed scouting or changed wallet")
		}
	}
	if !write {
		replayInit()
		vision()
		expect(true, false)
		return
	}
	expect(false, false)
	vision(503)
	s := sender.NewAllianceRewardsSender(n.nest)
	blocks := []int64{n.interest.BlockAt(spatial.Point{X: 10, Y: 10}), n.interest.BlockAt(spatial.Point{X: 210, Y: 10})}
	before, e := s.Sync_AllianceWallet(ctx, n.id)
	check(t, e)
	for _, bad := range []struct {
		blocks []int64
		reject bool
	}{{blocks, true}, {[]int64{-1}, false}} {
		if _, e = s.Sync_AllianceMove(ctx, n.id, n.interest, n.syncer, 700, 700, bad.blocks, bad.reject); e == nil {
			t.Fatal("invalid/rolled back movement succeeded")
		}
		after, e := s.Sync_AllianceWallet(ctx, n.id)
		check(t, e)
		if before != after || len(n.interest.ObservedBlocks(n.id)) != 0 {
			t.Fatal("position or coverage rollback diverged")
		}
		expect(false, false)
		vision(503)
	}
	_, e = s.Sync_AllianceMove(ctx, n.id, n.interest, n.syncer, 700, 700, blocks, false)
	check(t, e)
	expect(true, true)
	vision(501, 504)
	// 独立侦察授权仍有效时，清除覆盖块不能误删对象；最后授权来源撤销才删除。
	check(t, n.interest.SetViewMembers(900, []int64{n.id}))
	expect(true, true)
	// 清除一个覆盖块只移走该视点来源；位置仍在 700,700，不复制两个实体。
	_, e = s.Sync_AllianceMove(ctx, n.id, n.interest, n.syncer, 700, 700, blocks[:1], false)
	check(t, e)
	expect(true, true)
	check(t, n.interest.SetViewMembers(900, nil))
	expect(true, false)
	if got := n.interest.ObservedBlocks(n.id); len(got) != 1 || got[0] != blocks[0] {
		t.Fatal("committed coverage mismatch", got)
	}
	before, e = s.Sync_AllianceWallet(ctx, n.id)
	check(t, e)
	if _, e = s.Sync_AllianceRevokeVision(ctx, n.id, p.Dao().GetHomeSID(), 501, true); e == nil {
		t.Fatal("vision rollback accepted")
	}
	after, e := s.Sync_AllianceWallet(ctx, n.id)
	check(t, e)
	if after != before {
		t.Fatal("vision rollback changed durable permission")
	}
	vision(501, 504)
	if _, e = s.Sync_AllianceRevokeVision(ctx, n.id, p.Dao().GetHomeSID()+1, 501, false); e == nil {
		t.Fatal("wrong home changed vision")
	}
	remaining, e := s.Sync_AllianceRevokeVision(ctx, n.id, p.Dao().GetHomeSID(), 501, false)
	check(t, e)
	if remaining != 12 {
		t.Fatal("revoking 501 changed unrelated permission", remaining)
	}
	vision(504) // 最后 1ms 内仍可见；501 的撤权不影响独立临时授权。
	replayInit()
	vision(504)
	// 由真实 ticker 推动到期，不在 observe 内手动 refresh；所有发布串行归此循环。
	loop, err := newPermissionLoop(20*time.Millisecond, 50*time.Millisecond)
	check(t, err)
	loopCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	originalLoad := view.load
	cycles := 0
	view.load = func(readCtx context.Context) (string, error) {
		switch cycles {
		case 1:
			<-readCtx.Done()
			return "", readCtx.Err()
		case 2:
			// 模拟期限之后才返回的有效快照；即使 loader 漏报超时，也不能 Ready/Flush。
			raw, e := originalLoad(readCtx)
			if e != nil {
				return "", e
			}
			<-readCtx.Done()
			return raw, nil
		case 3:
			return "{", nil // 损坏的内部快照同样保持 held。
		}
		return originalLoad(readCtx)
	}
	err = loop.run(loopCtx, func(readCtx context.Context, _ uint64) error {
		return view.refresh(readCtx)
	}, func(refreshErr error) {
		cycles++
		switch cycles {
		case 1:
			check(t, refreshErr)
			view.expect(504)
			now = deadline // 不发提示；下次 ticker 必须发现到期。
		case 2, 3:
			if !errors.Is(refreshErr, context.DeadlineExceeded) {
				t.Fatalf("want read deadline, got %v", refreshErr)
			}
			view.noFrames()
		case 4:
			if refreshErr == nil {
				t.Fatal("invalid authority snapshot was accepted")
			}
			view.noFrames()
		case 5:
			check(t, refreshErr)
			view.expect() // 超时后重读期限，Full 清掉最后目标。
			cancel()
		default:
			t.Fatal("loop continued after cancellation")
		}
	})
	if !errors.Is(err, context.Canceled) || cycles != 5 {
		t.Fatalf("loop exit=%v cycles=%d", err, cycles)
	}
	view.load = originalLoad
	replayInit()
	vision()
	now = deadline + 1
	vision()
	t.Log("scouting: deadline persisted, before/exact/after expiry checked over TCP, duplicate init never renews")
	t.Log("strict spatial movement: precommit hidden, rollback/invalid coverage unchanged, two blocks one subject, remove one block, TCP public position only")
}
