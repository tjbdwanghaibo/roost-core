package saga

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// 参与方新路径的原生步骤（第 3 批）：回执在实体里，Begin 在实体锁内查，Complete 写，结论在事务提交后交给消费者。这些用例
// 用真实的 Nest 事务（corenest.RunIsolatedTransaction：strict、undo 回滚）与一个按 DAO mutator 同样方式登记逆操作的内存回执
// 存储；body 里的 business 计数就是业务执行次数。

// receiptEntity 是带回执存储的“实体”：写入登记逆操作（与生成 DAO 的 mutator 同一机制），事务被拒时随之回滚。puts 计每笔
// 事务里的写入次数（回放重写回执的断言用）。
type receiptEntity struct {
	receipts map[string][]byte
	puts     int
}

func newReceiptEntity() *receiptEntity { return &receiptEntity{receipts: map[string][]byte{}} }

func (e *receiptEntity) SagaStepReceipts() StepReceipts { return receiptStore{e} }

type receiptStore struct{ e *receiptEntity }

const receiptUndoField = 1

func (s receiptStore) StepReceipt(operation string) ([]byte, bool) {
	raw, ok := s.e.receipts[operation]
	return raw, ok
}

func (s receiptStore) PutStepReceipt(operation string, encoded []byte) {
	s.e.puts++
	// 生成 DAO 的 mutator 会登记一处持久改动，让事务成为一条 WAL 记录；这里用一条唯一的事务回执代替。
	if err := corenest.CurrentRollbackTx().AddReceipt(coredata.Receipt{Namespace: "test-step-receipt", ID: operation + "#" + strconv.Itoa(s.e.puts), Digest: []byte{1}, ExpiresAt: time.Now().Add(time.Hour).UnixNano()}); err != nil {
		panic(err)
	}
	old, had := s.e.receipts[operation]
	corenest.RecordUndoToken(s.e, receiptUndoField, operation, func() error {
		if had {
			s.e.receipts[operation] = old
		} else {
			delete(s.e.receipts, operation)
		}
		return nil
	})
	s.e.receipts[operation] = encoded
}

func (s receiptStore) DeleteStepReceipt(operation string) {
	old, had := s.e.receipts[operation]
	if !had {
		return
	}
	corenest.RecordUndoToken(s.e, receiptUndoField, operation, func() error { s.e.receipts[operation] = old; return nil })
	delete(s.e.receipts, operation)
}

func (s receiptStore) RangeStepReceipts(f func(string, []byte) bool) {
	for operation, raw := range s.e.receipts {
		if !f(operation, raw) {
			return
		}
	}
}

// receiptCommitter 是事务的 committer（空记录 Nest 不交给它）：reject 非空时明确拒绝（事务回滚），否则记下提交次数。
type receiptCommitter struct {
	reject  error
	commits int
}

func (c *receiptCommitter) Commit(context.Context, corenest.CommitRecord) error {
	if c.reject != nil {
		return c.reject
	}
	c.commits++
	return nil
}

// stepCommand 是一次步骤尝试：saga-1 第 0 步的正向操作，incarnation / attempt 决定 CommandID（与协调器同一格式）。
func stepCommand(incarnation, attempt uint32, deadline time.Duration) Command {
	now := time.Now().UTC()
	op := operationKey("saga-1", PhaseForward, 0)
	return Command{
		ID: commandID(op, incarnation, attempt), IdempotencyKey: op, SagaID: "saga-1", SagaType: "rally", DefinitionVersion: 1,
		BusinessKey: "r-1", StepName: "reserve", Phase: PhaseForward, Attempt: attempt, Topic: "rally.reserve", Partition: 3,
		CreatedAt: now, DeadlineAt: now.Add(deadline),
	}
}

// nativeHarness 运行一次步骤：body 开一笔 Nest 事务，事务里是生成包装的形状（Begin → 业务 → Complete）。
type nativeHarness struct {
	entity    *receiptEntity
	committer *receiptCommitter
	business  int
	success   bool
	reason    string
}

func newNativeHarness() *nativeHarness {
	return &nativeHarness{entity: newReceiptEntity(), committer: &receiptCommitter{}, success: true}
}

func (h *nativeHarness) body(ctx context.Context, step NativeStep) error {
	_, err := corenest.RunIsolatedTransaction(ctx, h.committer, "rally-reserve", func() (any, error) {
		if replayed, err := step.Begin(h.entity); replayed || err != nil {
			return nil, err
		}
		h.business++
		return nil, step.Complete(h.success, h.reason)
	})
	return err
}

func (h *nativeHarness) run(t *testing.T, command Command) StepOutcome {
	t.Helper()
	outcome, err := RunNativeStep(context.Background(), command, time.Hour, h.body)
	if err != nil {
		t.Fatalf("run %s: %v", command.ID, err)
	}
	return outcome
}

func (h *nativeHarness) stored(t *testing.T, command Command) StepReceipt {
	t.Helper()
	raw, ok := h.entity.receipts[command.IdempotencyKey]
	if !ok {
		t.Fatalf("no receipt for %s", command.IdempotencyKey)
	}
	receipt, err := decodeStepReceipt(raw)
	if err != nil {
		t.Fatal(err)
	}
	return receipt
}

// 同一操作的重投与下一次尝试：业务只执行一次，之后都回放第一次的结论（CommandID 是生效的那次）；回放也写一次回执，
// 让回放事务成为一条 WAL 记录（pipelined 下它的提交后回调晚于写回执那笔的落盘）。
func TestNativeStepReceiptRunsOnceAndReplaysTheEffectiveAttempt(t *testing.T) {
	h := newNativeHarness()
	first := stepCommand(0, 1, time.Minute)
	outcome := h.run(t, first)
	if outcome.Kind != StepExecuted || !outcome.Completion.Success || outcome.Completion.CommandID != first.ID || outcome.Completion.SagaID != "saga-1" {
		t.Fatalf("first run = %+v, want executed success for %s", outcome, first.ID)
	}
	if receipt := h.stored(t, first); receipt.CommandID != first.ID || !receipt.Success || receipt.Incarnation != 0 || receipt.At == 0 {
		t.Fatalf("receipt = %+v", receipt)
	}
	for _, command := range []Command{first, stepCommand(0, 2, time.Minute)} {
		putsBefore := h.entity.puts
		outcome = h.run(t, command)
		if outcome.Kind != StepReplayed || outcome.Completion.CommandID != first.ID || !outcome.Completion.Success || outcome.Completion.IdempotencyKey != first.IdempotencyKey {
			t.Fatalf("redelivery of %s = %+v, want a replay of %s", command.ID, outcome, first.ID)
		}
		if h.entity.puts != putsBefore+1 {
			t.Fatalf("a replay wrote the receipt %d times, want once (it makes the replay a WAL record)", h.entity.puts-putsBefore)
		}
	}
	if h.business != 1 {
		t.Fatalf("business ran %d times for one operation, want 1", h.business)
	}
	if h.committer.commits != 3 {
		t.Fatalf("commits = %d, want 3 (one execution, two replays)", h.committer.commits)
	}
}

// 业务拒绝：同一代际的重投与下一次尝试回放拒绝；补偿方向重开的新一生（CommandID 带 r1）重新执行并覆盖回执。reason 为空时填
// 一条点名步骤的说明。
func TestNativeStepReceiptReplaysARefusalOnlyWithinItsIncarnation(t *testing.T) {
	h := newNativeHarness()
	h.success, h.reason = false, ""
	first := stepCommand(0, 1, time.Minute)
	outcome := h.run(t, first)
	if outcome.Kind != StepExecuted || outcome.Completion.Success || outcome.Completion.Error != "saga: native step reserve refused" {
		t.Fatalf("refusal = %+v", outcome)
	}
	if outcome = h.run(t, stepCommand(0, 2, time.Minute)); outcome.Kind != StepReplayed || outcome.Completion.CommandID != first.ID || outcome.Completion.Success {
		t.Fatalf("same incarnation = %+v, want the refusal replayed", outcome)
	}
	h.success = true
	reopened := stepCommand(1, 1, time.Minute)
	if outcome = h.run(t, reopened); outcome.Kind != StepExecuted || !outcome.Completion.Success || outcome.Completion.CommandID != reopened.ID {
		t.Fatalf("new incarnation = %+v, want a fresh execution", outcome)
	}
	if receipt := h.stored(t, reopened); receipt.CommandID != reopened.ID || receipt.Incarnation != 1 || !receipt.Success {
		t.Fatalf("receipt after the new incarnation = %+v", receipt)
	}
	if h.business != 2 {
		t.Fatalf("business ran %d times, want 2 (once per incarnation)", h.business)
	}
}

// 过了截止：没有回执就不执行（过期未执行，不写任何东西）；有成功回执照样回放（过期投递重发成功）。
func TestNativeStepReceiptPastTheDeadline(t *testing.T) {
	h := newNativeHarness()
	expired := stepCommand(0, 1, -time.Second)
	if outcome := h.run(t, expired); outcome.Kind != StepExpired {
		t.Fatalf("expired without a receipt = %+v", outcome)
	}
	if h.business != 0 || len(h.entity.receipts) != 0 || h.committer.commits != 0 {
		t.Fatalf("expired attempt ran business=%d receipts=%d commits=%d, want nothing written", h.business, len(h.entity.receipts), h.committer.commits)
	}
	done := stepCommand(0, 2, time.Minute)
	h.run(t, done)
	late := stepCommand(0, 3, -time.Second)
	if outcome := h.run(t, late); outcome.Kind != StepReplayed || outcome.Completion.CommandID != done.ID || !outcome.Completion.Success {
		t.Fatalf("expired with a success receipt = %+v, want the success replayed", outcome)
	}
}

// 事务被明确拒绝：回执随业务回滚，没有结论（结果未知 → 调用方 nak）；下一次投递正常执行。
func TestNativeStepReceiptRollsBackWithARejectedTransaction(t *testing.T) {
	h := newNativeHarness()
	h.committer.reject = errors.New("wal full")
	command := stepCommand(0, 1, time.Minute)
	if _, err := RunNativeStep(context.Background(), command, time.Hour, h.body); err == nil {
		t.Fatal("a rejected transaction returned no error")
	}
	if len(h.entity.receipts) != 0 {
		t.Fatalf("receipt survived the rollback: %v", h.entity.receipts)
	}
	h.committer.reject = nil
	if outcome := h.run(t, command); outcome.Kind != StepExecuted {
		t.Fatalf("after the rollback = %+v, want an execution", outcome)
	}
}

// Complete 只在 Begin 判定执行的那笔事务里有效；锁目标没有回执存储时 Begin 报错点名类型；步骤体没跑出结论是错误。
func TestNativeStepReceiptMisuse(t *testing.T) {
	command := stepCommand(0, 1, time.Minute)
	committer := &receiptCommitter{}
	withoutBegin := func(ctx context.Context, step NativeStep) error {
		_, err := corenest.RunIsolatedTransaction(ctx, committer, "x", func() (any, error) { return nil, step.Complete(true, "") })
		return err
	}
	if _, err := RunNativeStep(context.Background(), command, time.Hour, withoutBegin); !errors.Is(err, ErrNativeStepNotBegun) {
		t.Fatalf("Complete without Begin = %v", err)
	}
	entity := newReceiptEntity()
	inAnotherTransaction := func(ctx context.Context, step NativeStep) error {
		if _, err := corenest.RunIsolatedTransaction(ctx, committer, "x", func() (any, error) { _, err := step.Begin(entity); return nil, err }); err != nil {
			return err
		}
		_, err := corenest.RunIsolatedTransaction(ctx, committer, "y", func() (any, error) { return nil, step.Complete(true, "") })
		return err
	}
	if _, err := RunNativeStep(context.Background(), command, time.Hour, inAnotherTransaction); !errors.Is(err, ErrNativeStepNotBegun) {
		t.Fatalf("Complete in another transaction = %v", err)
	}
	noHost := func(ctx context.Context, step NativeStep) error {
		_, err := corenest.RunIsolatedTransaction(ctx, committer, "x", func() (any, error) { _, err := step.Begin(struct{}{}); return nil, err })
		return err
	}
	if _, err := RunNativeStep(context.Background(), command, time.Hour, noHost); !errors.Is(err, ErrNoStepReceipts) {
		t.Fatalf("Begin on a target without receipts = %v", err)
	}
	if _, err := RunNativeStep(context.Background(), command, time.Hour, func(context.Context, NativeStep) error { return nil }); !errors.Is(err, ErrNativeStepNoOutcome) {
		t.Fatalf("a body without a Nest transaction = %v", err)
	}
	// 坏回执不当作“没有回执”（那会让操作再执行一次）。
	entity.receipts[command.IdempotencyKey] = []byte(`{"v":99}`)
	h := &nativeHarness{entity: entity, committer: committer, success: true}
	if _, err := RunNativeStep(context.Background(), command, time.Hour, h.body); !errors.Is(err, ErrInvalidRecord) || h.business != 0 {
		t.Fatalf("undecodable receipt = %v (business ran %d times)", err, h.business)
	}
}

// 旧路径（收件箱构造的 NativeStep，run 为空）上 Begin 不做任何事：业务照常执行，至多一次由收件箱租约保证。
func TestNativeStepBeginIsANoOpOnTheInboxPath(t *testing.T) {
	replayed, err := NativeStep{}.Begin(struct{}{})
	if replayed || err != nil {
		t.Fatalf("Begin on the inbox path = %v %v", replayed, err)
	}
}

// Complete 顺手剪掉过了保留期的旧回执，一次至多 16 条，不剪刚写的与没过期的。
func TestNativeStepReceiptPrunesExpiredReceiptsBoundedly(t *testing.T) {
	h := newNativeHarness()
	old := time.Now().Add(-2 * time.Hour).UnixMilli()
	for i := range 20 {
		op := "other:" + strconv.Itoa(i)
		h.entity.receipts[op] = encodeStepReceipt(StepReceipt{CommandID: op + ":1", Success: true, At: old})
	}
	h.entity.receipts["fresh"] = encodeStepReceipt(StepReceipt{CommandID: "fresh:1", Success: true, At: time.Now().UnixMilli()})
	command := stepCommand(0, 1, time.Minute)
	h.run(t, command)
	if got := len(h.entity.receipts); got != 22-16 {
		t.Fatalf("receipts after one Complete = %d, want %d (16 of 20 expired pruned)", got, 22-16)
	}
	if _, ok := h.entity.receipts[command.IdempotencyKey]; !ok {
		t.Fatal("the new receipt was pruned")
	}
	if _, ok := h.entity.receipts["fresh"]; !ok {
		t.Fatal("a receipt inside the retention was pruned")
	}
}

func TestStepReceiptEncodingRoundTrips(t *testing.T) {
	for _, receipt := range []StepReceipt{
		{CommandID: "a:1:0:1", Success: true, Data: []byte("x"), At: 7},
		{CommandID: "a:1:0:r2:1", Incarnation: 2, Error: "no", At: 8},
	} {
		got, err := decodeStepReceipt(encodeStepReceipt(receipt))
		if err != nil || fmt.Sprint(got) != fmt.Sprint(receipt) {
			t.Fatalf("round trip %+v = %+v %v", receipt, got, err)
		}
	}
	for _, raw := range []string{`{}`, `{"v":1}`, `{"v":1,"command_id":"x"}`, `not json`} {
		if _, err := decodeStepReceipt([]byte(raw)); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("decode %s = %v, want ErrInvalidRecord", raw, err)
		}
	}
}
