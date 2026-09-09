package action_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/action"
	"github.com/tjbdwanghaibo/roost-core/robot/protocol"
	"github.com/tjbdwanghaibo/roost-core/robot/session"
	"github.com/tjbdwanghaibo/roost-core/robot/transport"
)

// U-0111 · C2（空洞测试）· nightly gap map `robot/action` 15/20。
//
// 机器人动作是场景脚本与协议之间的桥：注册表为 nil、动作不存在、内层错误
// 已带 "robot action " 前缀（不能再包一层，否则场景报告里同一个错误出现两个
// 前缀）、wait_push 缺 msg 参数、没有会话、RegisterCall 缺协议注册表、请求类
// 型不是结构体、响应解码成了别的类型、参数无法转换成目标标量——每一条都必
// 须以清楚的错误拒绝，而不是 panic 或带着零值发包。

func TestActionRegistryNilAndLookupGuards(t *testing.T) {
	ctx := context.Background()
	var none *action.Registry
	if err := none.Register(namedAction("x")); err == nil || !strings.Contains(err.Error(), "registry is nil") {
		t.Fatalf("nil Register = %v", err)
	}
	if err := none.Run(ctx, robot.NewContext(robot.Config{}), "x", nil); err == nil || !strings.Contains(err.Error(), "registry is nil") {
		t.Fatalf("nil Run = %v", err)
	}

	registry := action.NewRegistry()
	rb := robot.NewContext(robot.Config{})
	if err := registry.Run(ctx, rb, "no_such_action", nil); err == nil || !strings.Contains(err.Error(), `"no_such_action" not found (registered:`) {
		t.Fatalf("unknown action = %v", err)
	}

	// 已带前缀的错误原样返回；其他错误包上动作名。
	inner := errors.New("robot action login: boom")
	registry.MustRegister(action.Func{ActionName: "prefixed", Handle: func(context.Context, *robot.Context, any) error { return inner }})
	registry.MustRegister(action.Func{ActionName: "plain", Handle: func(context.Context, *robot.Context, any) error { return errors.New("boom") }})
	if err := registry.Run(ctx, rb, "prefixed", nil); err != inner {
		t.Fatalf("prefixed error was rewrapped: %v", err)
	}
	if err := registry.Run(ctx, rb, "plain", nil); err == nil || err.Error() != "robot action plain: boom" {
		t.Fatalf("plain error = %v, want it wrapped once with the action name", err)
	}
}

func TestWaitPushRequiresMsgParamAndSession(t *testing.T) {
	ctx := context.Background()
	registry := action.NewRegistry()
	rb := robot.NewContext(robot.Config{}) // never connected: no session
	if err := registry.Run(ctx, rb, "wait_push", map[string]any{}); err == nil || !strings.Contains(err.Error(), "msg param is required") {
		t.Fatalf("wait_push without msg = %v", err)
	}
	if err := registry.Run(ctx, rb, "wait_push", map[string]any{"msg": 7}); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("wait_push without session = %v, want session.ErrClosed", err)
	}
}

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
