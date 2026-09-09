package robot

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/robot/session"
)

// U-0147 · C2 · nightly gap map core `robot` 5/5：nil 上下文 / 无动作执行器不能 Do；
// 捕获登记要求 key 与 handler，且没有会话时报 session.ErrClosed；步进 bot 缺 Sink 不能建。
func TestContextRefusesMissingRunnerSessionAndCaptureArguments(t *testing.T) {
	ctx := context.Background()
	var none *Context
	if err := none.Do(ctx, "login", nil); err == nil || !strings.Contains(err.Error(), "context is nil") {
		t.Fatalf("Do on a nil context = %v", err)
	}
	c := NewContext(Config{})
	if err := c.Do(ctx, "login", nil); err == nil || !strings.Contains(err.Error(), "action runner is nil") {
		t.Fatalf("Do without a runner = %v", err)
	}
	ran := ""
	c.RunAction = func(_ context.Context, _ *Context, name string, _ any) error { ran = name; return nil }
	if err := c.Do(ctx, "login", nil); err != nil || ran != "login" {
		t.Fatalf("Do with a runner = %v, ran=%q", err, ran)
	}
	handler := func(*session.Message) {}
	for name, call := range map[string]func() error{
		"nil context": func() error { return none.EnsurePushCapture("k", 1, handler) },
		"empty key":   func() error { return c.EnsurePushCapture("", 1, handler) },
		"nil handler": func() error { return c.EnsurePushCapture("k", 1, nil) },
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "capture key and handler are required") {
			t.Fatalf("EnsurePushCapture(%s) = %v", name, err)
		}
	}
	if err := c.EnsurePushCapture("k", 1, handler); !errors.Is(err, session.ErrClosed) {
		t.Fatalf("EnsurePushCapture without a session = %v, want session.ErrClosed", err)
	}
	if _, err := NewLockstepBot(LockstepBotConfig{Player: 1}); err == nil || !strings.Contains(err.Error(), "sink is required") {
		t.Fatalf("NewLockstepBot without a sink = %v", err)
	}
}
