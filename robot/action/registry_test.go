package action_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/robot"
	"github.com/tjbdwanghaibo/roost-core/robot/action"
	"github.com/tjbdwanghaibo/roost-core/robot/session"
)

type namedAction string

func (n namedAction) Name() string                                   { return string(n) }
func (n namedAction) Run(context.Context, *robot.Context, any) error { return nil }

// 动作按归一化后的名字被场景 spec 寻址：无名或重名的动作会让 spec 绑到错误的代码，注册时拒绝。
func TestActionRegistryRefusesNamelessAndDuplicateActions(t *testing.T) {
	r := action.NewRegistry()
	for _, tc := range []struct {
		name string
		a    action.Action
		text string
	}{
		{"nil action", nil, "action is nil"},
		{"blank name", namedAction("   "), "name is required"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := r.Register(tc.a); err == nil || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Register = %v, want %q", err, tc.text)
			}
		})
	}
	if err := r.Register(namedAction("Login")); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(namedAction("login")); err == nil || !strings.Contains(err.Error(), `duplicate "login"`) {
		t.Fatalf("duplicate = %v", err)
	}
}

// nil 注册表不能注册或执行；未知动作报出已注册列表；内层错误已带 "robot action " 前缀时原样返回，
// 否则只包一层动作名（场景报告里同一个错误不出现两个前缀）。
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

// wait_push 缺 msg 参数、没有会话时以清楚的错误拒绝。
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
