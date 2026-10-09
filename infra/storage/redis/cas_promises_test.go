package redis

import (
	"context"
	"errors"
	"testing"
)

// U-0106 · C2（空洞测试）· B-19（契约包部分）。
//
// CompareAndSet 是 versionstore / service 的乐观并发原语。命令不完整时必须在
// 发出脚本之前拒绝（nil 客户端不能被解引用、空 key 不能变成对 "" 的 CAS、
// nil Next 不能写入空串）；脚本回复不是 {applied, current} 二元组时必须报错
// 而不是越界读取。

type recordingScriptRunner struct {
	calls int
	reply any
}

func (r *recordingScriptRunner) Eval(context.Context, string, []string, ...any) (any, error) {
	r.calls++
	return r.reply, nil
}

func TestCompareAndSetRejectsIncompleteCommandsBeforeIO(t *testing.T) {
	ctx := context.Background()
	runner := &recordingScriptRunner{reply: []any{int64(1), "v2"}}
	valid := CompareAndSetCommand{Key: "k", Expected: []byte("v1"), Next: []byte("v2")}
	if _, err := CompareAndSet(ctx, runner, valid); err != nil {
		t.Fatalf("baseline CompareAndSet = %v", err)
	}
	if runner.calls != 1 {
		t.Fatalf("baseline issued %d scripts, want 1", runner.calls)
	}

	cases := []struct {
		name   string
		runner ScriptRunner
		cmd    CompareAndSetCommand
	}{
		{name: "nil client", runner: nil, cmd: valid},
		{name: "empty key", runner: runner, cmd: CompareAndSetCommand{Expected: []byte("v1"), Next: []byte("v2")}},
		{name: "nil next", runner: runner, cmd: CompareAndSetCommand{Key: "k", Expected: []byte("v1")}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			before := runner.calls
			result, err := CompareAndSet(ctx, tc.runner, tc.cmd)
			if !errors.Is(err, ErrCASInvalidCommand) {
				t.Fatalf("err=%v, want ErrCASInvalidCommand", err)
			}
			if result.Applied || result.Current != nil {
				t.Fatalf("rejected command produced a result: %+v", result)
			}
			if runner.calls != before {
				t.Fatal("rejected command still reached the script runner")
			}
		})
	}
}

func TestCompareAndSetRejectsMalformedScriptReply(t *testing.T) {
	ctx := context.Background()
	cmd := CompareAndSetCommand{Key: "k", Expected: []byte("v1"), Next: []byte("v2")}
	for _, reply := range []any{[]any{int64(1)}, []any{}, []any{int64(1), "v2", "extra"}} {
		runner := &recordingScriptRunner{reply: reply}
		result, err := CompareAndSet(ctx, runner, cmd)
		if err == nil {
			t.Fatalf("reply %v (len %d) was accepted: %+v", reply, len(reply.([]any)), result)
		}
	}
}
