package action_test

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/action"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
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

// RegisterCall 缺协议注册表、请求类型不是结构体时注册即拒绝；未连接时调用在发包前以
// "session not connected" 拒绝；参数无法转换成目标标量、响应解码成别的类型各自报错——不 panic，
// 也不带着零值发包。
// 历史：U-0111

type convReq struct {
	I int32   `json:"i"`
	U uint32  `json:"u"`
	F float64 `json:"f"`
	B bool    `json:"b"`
}

type otherResp struct {
	Code int32 `json:"code"`
	Note string
}

func TestRegisterCallGuards(t *testing.T) {
	ctx := context.Background()
	actions := action.NewRegistry()
	protocols := protocol.NewRegistry(protocol.JSONCodec{})

	if err := action.RegisterCall[buyReq, buyResp](actions, nil, "buy", msgBuy); err == nil || !strings.Contains(err.Error(), "needs a protocol registry") {
		t.Fatalf("RegisterCall without protocols = %v", err)
	}
	if err := action.RegisterCall[int, buyResp](actions, protocols, "scalar_req", 23); err == nil || !strings.Contains(err.Error(), "must be a struct") {
		t.Fatalf("RegisterCall with a scalar request type = %v", err)
	}

	// 未连接：调用动作在发包前以 "session not connected" 拒绝。
	if err := action.RegisterCall[buyReq, buyResp](actions, protocols, "buy", msgBuy); err != nil {
		t.Fatal(err)
	}
	offline := robot.NewContext(robot.Config{Protocols: protocols})
	if err := actions.Run(ctx, offline, "buy", map[string]any{"item_id": 1}); err == nil || !strings.Contains(err.Error(), "session not connected") {
		t.Fatalf("call without session = %v", err)
	}

	// 已连接的会话：参数转换失败与响应类型不符各自报错。
	endpoint := startServer(t)
	action.MustRegisterCall[convReq, buyResp](actions, protocols, "conv", 24)
	// 与 "buy" 共用 msg id 但声明了别的响应类型：解码器以先注册的 buyResp 为准，
	// 这个动作拿到的响应就不是它声明的类型。
	action.MustRegisterCall[buyReq, otherResp](actions, protocols, "buy_other_resp", msgBuy)
	rb := robot.NewContext(robot.Config{Transport: transport.Config{Endpoint: endpoint}, Protocols: protocols})
	rb.RunAction = func(ctx context.Context, rb *robot.Context, name string, param any) error {
		return actions.Run(ctx, rb, name, param)
	}
	t.Cleanup(func() { _ = rb.Close() })
	if err := rb.Do(ctx, action.NameConnect, nil); err != nil {
		t.Fatal(err)
	}
	if err := actions.Run(ctx, rb, "conv", map[string]any{"i": 1, "u": 2, "f": 3, "b": true}); err != nil {
		t.Fatalf("baseline conv call = %v", err)
	}
	for _, tc := range []struct {
		name   string
		params map[string]any
		text   string
	}{
		{"string into int", map[string]any{"i": "one"}, "cannot convert string to integer"},
		{"negative into uint", map[string]any{"u": -1}, "cannot convert int to unsigned integer"},
		{"string into float", map[string]any{"f": "pi"}, "cannot convert string to float"},
		{"string into bool", map[string]any{"b": "yes"}, "cannot convert string to bool"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := actions.Run(ctx, rb, "conv", tc.params); err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("conv %v = %v, want %q", tc.params, err, tc.text)
			}
		})
	}
	if err := actions.Run(ctx, rb, "buy_other_resp", map[string]any{"item_id": 1}); err == nil || !strings.Contains(err.Error(), "unexpected response type") {
		t.Fatalf("response decoded as another type = %v", err)
	}
}
