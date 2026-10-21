package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// 验证测量客户端真的拒绝坏版本、坏字段与错误可见集合，而不是只数收到了几帧。
func TestClientValidatesContentVersionAndVisibility(t *testing.T) {
	for _, fault := range []string{"none", "version", "field", "visibility"} {
		t.Run(fault, func(t *testing.T) {
			c := config{Players: 1, Entities: 10, Padding: 8, TraceCapacity: 16}
			id := int64(1)
			at := position(id, 0, c)
			value := &subject{data: component{ID: id, X: at.X, Y: at.Y, HP: 100, Extra: []byte{1, 1, 1, 1, 1, 1, 1, 1}}}
			state := entity.NewSubjectSyncState(entity.SubjectSyncCreateParam{Enabled: true, SubjectID: id, Namespace: "test", Packer: value})
			server, client := net.Pipe()
			defer server.Close()
			done := make(chan clientResult, 1)
			go func() { defer client.Close(); done <- consume(client, c, func() {}) }()
			tick := 0
			manager, err := entitysync.NewManager(entitysync.ManagerConfig{Transport: entitysync.TransportFunc(func(_ context.Context, _ entitysync.SessionID, raw []byte) error {
				if tick == 1 && fault == "version" {
					decoded, e := entitysync.DecodeFrame(raw, frame.DefaultLimits())
					if e != nil {
						return e
					}
					update, e := entitysync.DecodeSubjectUpdate(decoded.Objects[0].Components[0].Data, 0)
					if e != nil {
						return e
					}
					update.BaseVersion = 999
					data, e := entitysync.EncodeSubjectUpdate(update, 0)
					if e != nil {
						return e
					}
					decoded.Objects[0].Components[0].Data = data
					raw, e = frame.Encode(decoded, frame.DefaultLimits())
					if e != nil {
						return e
					}
				}
				_ = server.SetWriteDeadline(time.Now().Add(time.Second))
				return writePacket(server, packetFrame, tick, time.Now().UnixNano(), raw)
			})})
			if err != nil {
				t.Fatal(err)
			}
			if err := manager.Register(state); err != nil {
				t.Fatal(err)
			}
			if err := manager.OpenSession(1); err != nil {
				t.Fatal(err)
			}
			if err := manager.Subscribe(1, id, entity.SyncProfile{}); err != nil {
				t.Fatal(err)
			}
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			tick = 1
			at = position(id, 1, c)
			value.data.Step = 1
			value.data.X, value.data.Y = at.X, at.Y
			value.data.HP = 100 - 10001%97
			value.data.Planned = time.Now().UnixNano()
			value.data.Changed = value.data.Planned
			if fault == "field" {
				value.data.HP = -999
			}
			state.MarkDirty(1)
			if err := manager.Flush(context.Background()); err != nil {
				t.Fatal(err)
			}
			if fault == "none" {
				if err := manager.HoldSession(1); err != nil {
					t.Fatal(err)
				}
				if err := manager.ReadySession(1); err != nil {
					t.Fatal(err)
				}
				if err := manager.Flush(context.Background()); err != nil {
					t.Fatal(err)
				}
				checkpoint, _ := json.Marshal([]int64{id})
				if err := writePacket(server, packetRecovery, tick, time.Now().Add(-time.Second).UnixNano(), checkpoint); err != nil {
					t.Fatal(err)
				}
			}
			visible := []int64{id}
			if fault == "visibility" {
				visible = []int64{2}
			}
			raw, _ := json.Marshal(visible)
			_ = server.SetWriteDeadline(time.Now().Add(time.Second))
			_ = writePacket(server, packetFinish, 0, 0, raw)
			select {
			case result := <-done:
				if fault == "none" {
					if result.err != nil || result.frames != 2 || result.final != 1 || len(result.changed) != 1 {
						t.Fatalf("valid stream rejected: %+v", result)
					}
					if result.recoveryVerifiedMS == nil || *result.recoveryVerifiedMS < 1000 || len(result.baselineReceipts) != 1 || result.baselineReceipts[0].Epoch != 2 {
						t.Fatalf("missing actual client recovery evidence: %+v", result)
					}
				} else if result.err == nil {
					t.Fatalf("%s corruption accepted", fault)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("client did not finish")
			}
		})
	}
}

// 新订阅全量的计时口径：全量更新（新建对象，或已有对象的全量重建）里可能是本会话没计过时的旧改值，按改值时刻计时
// 量到的是旧值的年龄而不是下发延迟。所以全量只计“本 tick 改的值”（Step 等于帧的 tick），增量照常计时。
// 确定的小场景（手工编帧，不经 Manager）：
//
//	帧 1（tick 5）新建对象，Step 3（两秒前的旧改值）     → 不计时（Step ≠ tick）
//	帧 2（tick 6）同一对象全量重建，仍是 Step 3          → 不计时（修前按 ObjectUpdate 计时，量到约 2000ms）
//	帧 3（tick 7）全量重建，Step 7（本 tick 改的值）     → 计入全量
//	帧 4（tick 9）增量，Step 8（晚一个 tick 到的新改值） → 计入增量
func TestFullUpdateTimesOnlyThisTicksChange(t *testing.T) {
	c := config{Players: 1, Entities: 10, Padding: 8}
	const id = int64(1)
	old := time.Now().Add(-2 * time.Second).UnixNano()
	payload := func(step int64, changed int64) entity.FrozenSyncPayload {
		at := position(id, int(step), c)
		value := component{ID: id, Step: step, X: at.X, Y: at.Y, HP: 100 - (step+10000)%97, Planned: changed, Changed: changed, Extra: []byte{1, 1, 1, 1, 1, 1, 1, 1}}
		raw, err := bson.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		return entity.TakeFrozenSyncPayload(1, raw)
	}
	encode := func(clock uint32, kind frame.Kind, op frame.ObjectOperation, version uint64, full bool, body entity.FrozenSyncPayload) []byte {
		data, err := entitysync.EncodeSubjectUpdate(entity.SubjectSyncUpdate{SubjectID: id, Namespace: "test", Version: version, BaseVersion: version - 1, Full: full, Payload: body}, 0)
		if err != nil {
			t.Fatal(err)
		}
		base := clock - 1
		object := frame.ObjectDelta{Operation: op, Ref: frame.ObjectRef{ID: 1, Generation: 1}, Components: []frame.ComponentDelta{{Operation: frame.ComponentSet, TypeID: 1, SchemaVersion: 1, Data: data}}}
		if op == frame.ObjectCreate {
			object.Archetype = 1
		}
		raw, err := frame.Encode(frame.Frame{SnapshotMeta: frame.SnapshotMeta{StreamID: 1, Epoch: 1, Tick: clock, SchemaVersion: 1}, Kind: kind, BaseTick: base, Objects: []frame.ObjectDelta{object}}, frame.DefaultLimits())
		if err != nil {
			t.Fatal(err)
		}
		return raw
	}
	server, client := net.Pipe()
	defer server.Close()
	done := make(chan clientResult, 1)
	go func() { defer client.Close(); done <- consume(client, c, func() {}) }()
	now := time.Now().UnixNano()
	frames := []struct {
		tick int
		raw  []byte
	}{
		{5, encode(1, frame.Full, frame.ObjectCreate, 1, true, payload(3, old))},
		{6, encode(2, frame.Delta, frame.ObjectUpdate, 2, true, payload(3, old))},
		{7, encode(3, frame.Delta, frame.ObjectUpdate, 3, true, payload(7, now))},
		{9, encode(4, frame.Delta, frame.ObjectUpdate, 4, false, payload(8, now))},
	}
	for _, f := range frames {
		_ = server.SetWriteDeadline(time.Now().Add(time.Second))
		if err := writePacket(server, packetFrame, f.tick, now, f.raw); err != nil {
			t.Fatal(err)
		}
	}
	visible, _ := json.Marshal([]int64{id})
	_ = server.SetWriteDeadline(time.Now().Add(time.Second))
	if err := writePacket(server, packetFinish, 0, 0, visible); err != nil {
		t.Fatal(err)
	}
	var result clientResult
	select {
	case result = <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("client did not finish")
	}
	if result.err != nil {
		t.Fatal(result.err)
	}
	t.Logf("full samples (ms) = %.1f, delta samples (ms) = %.1f", result.full, result.live)
	if len(result.full) != 1 || result.full[0] > 1000 {
		t.Fatalf("full updates timed an old value's age: full samples %.1f ms, want exactly this tick's change (one sample, well under 1000 ms)", result.full)
	}
	if len(result.live) != 1 {
		t.Fatalf("delta samples %.1f ms, want the late delta counted once", result.live)
	}
}
