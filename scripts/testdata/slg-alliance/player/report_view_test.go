package alliance

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	sender "example.com/slg-alliance-practice/game/handler/syncsender"
	"example.com/slg-alliance-practice/game/scout"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
)

// 在恢复进程比完冷事实之后调用；模拟通知缺席，首次连接从永久 Inbox 重建。
// 票据 1 的服务端绑定是 n.id；票据 2 没有该玩家权限。这里不提供客户端选玩家接口。
func verifyReportClient(t *testing.T, n *rewardRuntime, home int32, original string, report scout.Report) {
	t.Helper()
	ctx := context.Background()
	s := sender.NewAllianceRewardsSender(n.nest)
	sink := newViewSink(t)
	m, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: sink})
	check(t, err)
	t.Cleanup(func() { check(t, m.Close(ctx)) })
	var payload []byte
	check(t, m.Register(entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{
		Enabled: true, SubjectID: n.id, Namespace: "private-inbox",
		Packer: entity.SubjectSyncPackFunc{Snapshot: func(entity.SyncProfile) (entity.FrozenSyncPayload, error) {
			return entity.CopyFrozenSyncPayload(1, payload), nil
		}},
	})))
	for _, id := range []entitysync.SessionID{1, 2} {
		check(t, m.OpenHeldSession(id))
	}
	// 只由服务端绑定订阅，永远不把私人报告 Show 到公共 AOI。
	check(t, m.Subscribe(1, n.id, entity.SyncProfile{}))
	load := func(ctx context.Context) (string, error) { return s.Sync_AllianceInbox(ctx, n.id) }
	refresh := func(ctx context.Context) error {
		for _, id := range []entitysync.SessionID{1, 2} {
			if err := m.HoldSession(id); err != nil {
				return err
			}
		}
		raw, err := load(ctx)
		if err != nil {
			return err
		}
		if err = ctx.Err(); err != nil {
			return err
		}
		views, err := scout.ProjectInbox(raw, n.id, home)
		if err != nil {
			return err
		}
		payload, err = json.Marshal(views)
		if err != nil {
			return err
		}
		for _, id := range []entitysync.SessionID{1, 2} {
			if err := m.ReadySession(id); err != nil {
				return err
			}
		}
		return m.Flush(ctx)
	}
	// 预期由原始报告独立拼出，逐字比较 wire 上的白名单 DTO，防止内部字段泄漏。
	want := fmt.Sprintf(`[{"id":%q,"target":%d,"at":%d,"destination":{"X":%d,"Y":%d}}]`, report.Identity(), report.Target, report.At, report.Destination.X, report.Destination.Y)
	receivers := []*frame.Receiver{frame.NewReceiver(frame.DefaultLimits()), frame.NewReceiver(frame.DefaultLimits())}
	var old []byte
	consume := func() {
		for i, r := range receivers {
			raws := sink.take(entitysync.SessionID(i + 1))
			if i == 0 && len(raws) == 0 {
				t.Fatal("missing reconnect/reset Full")
			}
			objects := 0
			for _, raw := range raws {
				applied, err := r.Receive(raw, func(f frame.Frame) error {
					if f.Kind != frame.Full {
						return errors.New("expected Full after held refresh")
					}
					for _, o := range f.Objects {
						objects++
						for _, c := range o.Components {
							u, err := entitysync.DecodeSubjectUpdate(c.Data, 0)
							if err != nil {
								return err
							}
							if i != 0 || u.SubjectID != n.id || string(u.Payload.BytesCopy()) != want {
								return fmt.Errorf("private report payload mismatch: %s", u.Payload.BytesCopy())
							}
						}
					}
					return nil
				})
				check(t, err)
				if !applied {
					t.Fatal("fresh report frame refused")
				}
				if i == 0 && old == nil {
					old = append([]byte(nil), raw...)
				}
			}
			if (i == 0 && objects != 1) || (i == 1 && objects != 0) {
				t.Fatal("report visible to wrong session", i, objects)
			}
		}
	}
	// 没有收件通知时也由周期重读恢复；服务循环串行拥有 payload 与 Sync 发布。
	loopCtx, stop := context.WithTimeout(ctx, 2*time.Second)
	defer stop()
	cycles := 0
	service := &VisionService{Interval: time.Millisecond, ReadTimeout: time.Second,
		Refresh: func(readCtx context.Context, _ uint64) error { return refresh(readCtx) },
		Observe: func(err error) { check(t, err); consume(); cycles++; stop() },
	}
	check(t, service.Init(nil))
	check(t, service.Serve(loopCtx))
	check(t, service.Shutdown(ctx))
	if cycles != 1 {
		t.Fatal("missed notification was not recovered by periodic read", cycles)
	}
	goodLoad := load
	injected := errors.New("inbox read unavailable")
	for _, fault := range []string{"read", "late", "corrupt", "foreign"} {
		readCtx, cancel := context.WithCancel(ctx)
		load = func(context.Context) (string, error) {
			switch fault {
			case "read":
				return "", injected
			case "late":
				cancel()
				return original, nil
			case "corrupt":
				return "{", nil
			default:
				other := report
				other.Recipient++
				raw, err := json.Marshal(map[string]scout.Report{other.Identity(): other})
				return string(raw), err
			}
		}
		if err := refresh(readCtx); err == nil {
			t.Fatal("failed/late/foreign inbox published", fault)
		}
		cancel()
		check(t, m.Flush(ctx))
		if len(sink.take(1))+len(sink.take(2)) != 0 {
			t.Fatal("held report session published")
		}
		load = goodLoad
		check(t, refresh(ctx))
		consume()
	}
	// 刷新不是消费：重复读取不新增回执；关闭真实 socket 后仍重新加载同一 Inbox。
	for range 2 {
		check(t, refresh(ctx))
		consume()
	}
	for _, id := range []entitysync.SessionID{1, 2} {
		m.CloseSession(id)
		sink.connect(id)
		check(t, m.OpenHeldSession(id))
	}
	check(t, m.Subscribe(1, n.id, entity.SyncProfile{}))
	check(t, refresh(ctx))
	consume()
	applied, err := receivers[0].Receive(old, func(frame.Frame) error { return errors.New("old epoch reached report UI") })
	check(t, err)
	if applied {
		t.Fatal("old report epoch applied")
	}
	after, err := s.Sync_AllianceInbox(ctx, n.id)
	check(t, err)
	if after != original {
		t.Fatal("client refresh mutated permanent inbox")
	}
	t.Log("report client: cold Inbox -> private TCP Full, failed/late/foreign read held, reconnect rebuilt, old epoch refused, receipt unchanged")
}
