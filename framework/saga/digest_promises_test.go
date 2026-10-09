package saga

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// U-0107 · C5（静默吞错）· B-14。
//
// 收件箱用命令的 JSON 摘要判断"同一个 ID 再来一次"是重投递还是身份冲突。
// json.Marshal 对 Command 并非不可失败：time.Time 的年份超出 [0,9999] 时
// MarshalJSON 报错，而 Command.Validate 只要求 DeadlineAt / CreatedAt 非零，
// 这样的命令能通过校验。此前摘要函数丢弃了这个错误——所有此类命令的摘要都
// 退化成 sha256(nil)，同一个 ID 带不同载荷再来时被当成重投递、返回别人的
// 完成结果。这条测试在修复前对着 mongotest 替身确实变红：第二次 Handle 得到
// duplicate=true 而不是任何错误。

func farFutureCommand(id string, payload string) Command {
	now := time.Now()
	return Command{
		ID: id, IdempotencyKey: "op-" + id, SagaID: "saga", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "r-" + id, Step: 0, StepName: "reserve", Phase: PhaseForward, Attempt: 1, Topic: "reserve",
		Payload: []byte(payload), CreatedAt: now,
		// 年份 10000：Validate 放行（非零），json.Marshal 拒绝。
		DeadlineAt: time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC),
	}
}

func TestCommandDigestReportsUnmarshalableCommands(t *testing.T) {
	command := farFutureCommand("digest-1", "a")
	if err := command.Validate(); err != nil {
		t.Fatalf("fixture must pass Validate, got %v", err)
	}
	digest, err := commandDigest(command)
	if err == nil {
		t.Fatalf("commandDigest accepted an unmarshalable command, digest=%x", digest)
	}
	if !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("commandDigest error=%v, want ErrInvalidRecord", err)
	}
	other := farFutureCommand("digest-2", "b")
	if otherDigest, otherErr := commandDigest(other); otherErr == nil && string(otherDigest) == string(digest) {
		t.Fatal("two different unmarshalable commands shared one digest")
	}

	// 对照：正常命令的摘要稳定且随内容变化。
	normal := farFutureCommand("digest-3", "a")
	normal.DeadlineAt = time.Now().Add(time.Second)
	first, err := commandDigest(normal)
	if err != nil {
		t.Fatal(err)
	}
	again, _ := commandDigest(normal)
	if string(first) != string(again) {
		t.Fatal("digest of the same command is not stable")
	}
	normal.Payload = []byte("b")
	changed, _ := commandDigest(normal)
	if string(first) == string(changed) {
		t.Fatal("payload change did not change the digest")
	}
}

func TestMongoCommandInboxRefusesCommandsWhoseIdentityCannotBeDigested(t *testing.T) {
	inbox, err := NewMongoCommandInbox(newInboxMongoFake(), "game", "")
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := func(context.Context, Command) (Completion, error) {
		calls.Add(1)
		return Completion{Success: true, Data: []byte("reserved")}, nil
	}
	first := farFutureCommand("reuse", "a")
	if _, duplicate, err := inbox.Handle(context.Background(), first, handler); !errors.Is(err, ErrInvalidRecord) || duplicate {
		t.Fatalf("first Handle = (duplicate=%v, err=%v), want ErrInvalidRecord", duplicate, err)
	}
	// 同一个 ID、不同载荷：修复前被当成重投递（duplicate=true，返回第一次的结果）。
	second := farFutureCommand("reuse", "b")
	if _, duplicate, err := inbox.Handle(context.Background(), second, handler); err == nil || duplicate {
		t.Fatalf("second Handle = (duplicate=%v, err=%v), want an error and no duplicate verdict", duplicate, err)
	}
	if calls.Load() != 0 {
		t.Fatalf("handler ran %d times for commands whose identity could not be established", calls.Load())
	}
	if _, found, err := inbox.Replay(context.Background(), first); found || !errors.Is(err, ErrInvalidRecord) {
		t.Fatalf("Replay = (found=%v, err=%v), want ErrInvalidRecord", found, err)
	}
}

func TestCompletionDigestIsStableForMarshalableReceipts(t *testing.T) {
	one := Completion{CommandID: "c", IdempotencyKey: "op", SagaID: "saga", Success: true, Data: []byte("x"), CompletedAt: time.Now()}
	first, err := completionDigest(one)
	if err != nil {
		t.Fatal(err)
	}
	again, err := completionDigest(one)
	if err != nil || string(first) != string(again) {
		t.Fatalf("completion digest unstable: %v", err)
	}
}
