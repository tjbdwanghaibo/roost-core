package action_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/cube-core/robot"
	"github.com/tjbdwanghaibo/cube-core/robot/action"
	"github.com/tjbdwanghaibo/cube-core/robot/protocol"
	"github.com/tjbdwanghaibo/cube-core/robot/transport"
)

const msgBuy = 21

type buyReq struct {
	ItemID int32  `json:"item_id"`
	Count  int32  `json:"count"`
	Note   string `json:"note"`
}

type buyResp struct {
	Code   int32 `json:"code"`
	GoldID int64 `json:"gold_id"`
}

func (r *buyResp) GetCode() int32 { return r.Code }

// startServer answers msgBuy: code 0 unless item_id == 666, echoing a gold
// id derived from the request.
func startServer(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			wg.Add(1)
			go func() {
				defer wg.Done()
				defer conn.Close()
				for {
					packet, err := transport.ReadPacketFrom(conn, 0)
					if err != nil {
						return
					}
					var req buyReq
					_ = json.Unmarshal(packet.Payload, &req)
					resp := buyResp{GoldID: int64(req.ItemID) * 10}
					if req.ItemID == 666 {
						resp.Code = 5
					}
					payload, _ := json.Marshal(&resp)
					_ = transport.WritePacketsTo(conn, []*transport.Packet{{MsgID: packet.MsgID, Seq: packet.Seq, Payload: payload}})
				}
			}()
		}
	}()
	t.Cleanup(func() {
		_ = listener.Close()
		wg.Wait()
	})
	return listener.Addr().String()
}

var goldID = robot.Key[int64]("gold_id")

func newCallFixture(t *testing.T) (*action.Registry, *robot.Context) {
	t.Helper()
	endpoint := startServer(t)
	protocols := protocol.NewRegistry(protocol.JSONCodec{})
	actions := action.NewRegistry()
	action.MustRegisterCall[buyReq, buyResp](actions, protocols, "buy", msgBuy,
		action.OnResp(func(rb *robot.Context, resp *buyResp) error {
			goldID.Set(rb, resp.GoldID)
			return nil
		}),
	)
	rb := robot.NewContext(robot.Config{
		Transport: transport.Config{Endpoint: endpoint},
		Protocols: protocols,
	})
	rb.RunAction = func(ctx context.Context, rb *robot.Context, name string, param any) error {
		return actions.Run(ctx, rb, name, param)
	}
	t.Cleanup(func() { _ = rb.Close() })
	if err := rb.Do(context.Background(), action.NameConnect, nil); err != nil {
		t.Fatal(err)
	}
	return actions, rb
}

func TestRegisterCallConventions(t *testing.T) {
	_, rb := newCallFixture(t)

	// Fields fill by json name from the param map; OnResp remembers.
	if err := rb.Do(context.Background(), "buy", map[string]any{"item_id": 7, "count": 2}); err != nil {
		t.Fatal(err)
	}
	if v, _ := goldID.Get(rb); v != 70 {
		t.Fatalf("gold_id = %d (OnResp not applied)", v)
	}

	// Blackboard fallback: item_id comes from the blackboard when the param
	// omits it.
	rb.Blackboard.Set("item_id", int32(9))
	if err := rb.Do(context.Background(), "buy", nil); err != nil {
		t.Fatal(err)
	}
	if v, _ := goldID.Get(rb); v != 90 {
		t.Fatalf("gold_id = %d (blackboard fallback broken)", v)
	}

	// Non-zero GetCode fails the action with the code in the error.
	err := rb.Do(context.Background(), "buy", map[string]any{"item_id": 666})
	if err == nil || !strings.Contains(err.Error(), "response code 5") {
		t.Fatalf("code check missed: %v", err)
	}

	// A typed *Req param is used verbatim.
	if err := rb.Do(context.Background(), "buy", &buyReq{ItemID: 11}); err != nil {
		t.Fatal(err)
	}
	if v, _ := goldID.Get(rb); v != 110 {
		t.Fatalf("gold_id = %d (typed param ignored)", v)
	}
}

func TestRegisterCallMapFieldAndValidation(t *testing.T) {
	protocols := protocol.NewRegistry(protocol.JSONCodec{})
	actions := action.NewRegistry()
	// MapField renames the lookup key.
	if err := action.RegisterCall[buyReq, buyResp](actions, protocols, "buy_alias", 31,
		action.MapField[buyResp]("ItemID", "boss_id")); err != nil {
		t.Fatal(err)
	}
	// MapField for a nonexistent field fails at registration.
	if err := action.RegisterCall[buyReq, buyResp](actions, protocols, "buy_bad", 32,
		action.MapField[buyResp]("NoSuchField", "x")); err == nil {
		t.Fatal("bad MapField accepted")
	}
	// Registry without a codec is rejected up front.
	bare := protocol.NewRegistry(nil)
	if err := action.RegisterCall[buyReq, buyResp](actions, bare, "buy_nocodec", 33); err == nil || !errors.Is(err, protocol.ErrCodecRequired) {
		t.Fatalf("codec-less registry accepted: %v", err)
	}
}

func TestParamsChain(t *testing.T) {
	rb := robot.NewContext(robot.Config{})
	rb.Blackboard.Set("scene_id", int64(7))
	params := action.ParamsOf(rb, map[string]any{"count": 3, "flag": true, "wait": "150ms"})
	if v := params.Int64("count", -1); v != 3 {
		t.Fatalf("param int = %d", v)
	}
	if v := params.Int64("scene_id", -1); v != 7 {
		t.Fatalf("blackboard fallback = %d", v)
	}
	if v := params.Int64("missing", 42); v != 42 {
		t.Fatalf("default = %d", v)
	}
	if !params.Bool("flag", false) {
		t.Fatal("bool param lost")
	}
	if d := params.Duration("wait", 0); d.Milliseconds() != 150 {
		t.Fatalf("duration = %v", d)
	}
}
