package main

import (
	"context"
	"encoding/json"
	"net"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/entity"
	"github.com/tjbdwanghaibo/roost-core/sync/entitysync"
	"github.com/tjbdwanghaibo/roost-core/sync/frame"
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
