package saga

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
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
	op := operationKey("saga-1", testLifecycle, PhaseForward, 0)
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

// NativeStep 只由框架构造（RunNativeStep）：零值的 Begin / Complete 返回 ErrNoNativeStep，不执行、不写任何东西。
// 历史：TestNativeStepCannotCompleteWithoutAReservation（旧收件箱路径时零值 Complete 报 ErrNoNativeStep，Begin 是空操作）。
func TestNativeStepCannotCompleteWithoutARun(t *testing.T) {
	if replayed, err := (NativeStep{}).Begin(newReceiptEntity()); replayed || !errors.Is(err, ErrNoNativeStep) {
		t.Fatalf("Begin on a zero NativeStep = %v %v, want ErrNoNativeStep", replayed, err)
	}
	if err := (NativeStep{}).Complete(true, ""); !errors.Is(err, ErrNoNativeStep) {
		t.Fatalf("Complete on a zero NativeStep = %v, want ErrNoNativeStep", err)
	}
}

// 结果未知：Nest 已提交、调用方却拿到错误（回复丢失、超时）。RunNativeStep 返回错误（消费者 nak），同一命令的重投读到回执
// 回放、不再执行业务。历史：TestNativeStepUnknownOutcomeIsNotRunAgainByARedelivery（旧路径靠保留租约 + 等回执）。
func TestNativeStepUnknownOutcomeIsNotRunAgainByARedelivery(t *testing.T) {
	h := newNativeHarness()
	command := stepCommand(0, 1, time.Minute)
	lost := errors.New("reply lost after the commit")
	if _, err := RunNativeStep(context.Background(), command, time.Hour, func(ctx context.Context, step NativeStep) error {
		if err := h.body(ctx, step); err != nil {
			return err
		}
		return lost
	}); !errors.Is(err, lost) {
		t.Fatalf("run whose reply was lost = %v, want the error (outcome unknown, nak)", err)
	}
	outcome := h.run(t, command)
	if outcome.Kind != StepReplayed || !outcome.Completion.Success || outcome.Completion.CommandID != command.ID {
		t.Fatalf("redelivery after an unknown outcome = %+v, want a replay of the committed success", outcome)
	}
	if h.business != 1 {
		t.Fatalf("business ran %d times, want 1", h.business)
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

// 回执保留期满足 Register 的要求（≥ ReceiptRetentionFactor × 步骤重试窗口，CheckReceiptRetention）时，同一操作在窗口内的
// 每一次尝试都读得到第一次生效留下的回执、只回放（RV-20261030-02 的契约面）：同一实体上别的操作 Complete 时会剪掉过了保留期
// 的回执，窗口之内剪不到它；过了保留期才剪（剪枝本身照常工作）。保留期不够的配置由 Register 与配置检查拒绝
// （TestRegisterRefusesStepsWhoseRetryWindowOutlivesTheReceipts）。评审的复现：保留期 60s、窗口 190s，第 61s 别的操作剪掉
// 回执，第 62s 的第四次尝试再执行一次业务。
func TestNativeStepReceiptOutlivesEveryAttemptOfTheRetryWindow(t *testing.T) {
	step := Step{Name: "reserve", ForwardTopic: "rally.reserve", Timeout: 10 * time.Second, MaxAttempts: 10, BackoffMin: 10 * time.Second, BackoffMax: 10 * time.Second}
	retention := ReceiptRetentionFactor * step.RetryWindow()
	if err := CheckReceiptRetention(Definition{Type: "rally", Version: 1, Steps: []Step{step}}, retention); err != nil {
		t.Fatal(err)
	}
	t0 := time.Now().UTC()
	entity, committer := newReceiptEntity(), &receiptCommitter{}
	effects := 0
	run := func(command Command, at time.Time) StepOutcome {
		t.Helper()
		state := newNativeRun(command, retention)
		state.now = func() time.Time { return at }
		native := NativeStep{command: command, run: state}
		if _, err := corenest.RunIsolatedTransaction(context.Background(), committer, "rally-reserve", func() (any, error) {
			if replay, err := native.Begin(entity); replay || err != nil {
				return nil, err
			}
			if command.SagaID == "saga-1" {
				effects++
			}
			return nil, native.Complete(true, "")
		}); err != nil {
			t.Fatal(err)
		}
		return state.result()
	}
	attempt := func(n uint32) Command {
		command := stepCommand(0, n, step.Timeout)
		command.CreatedAt = t0.Add(time.Duration(n-1) * (step.Timeout + step.BackoffMax)) // 计划的派发时刻
		command.DeadlineAt = command.CreatedAt.Add(step.Timeout)
		return command
	}
	other := func(n int) Command {
		command := stepCommand(0, 1, time.Hour)
		command.SagaID = "other-" + strconv.Itoa(n)
		command.IdempotencyKey = operationKey(command.SagaID, testLifecycle, PhaseForward, 0)
		command.ID = commandID(command.IdempotencyKey, 0, 1)
		return command
	}
	first := attempt(1)
	run(first, first.CreatedAt) // 生效，结果没送回协调器
	for n := uint32(2); n <= step.MaxAttempts; n++ {
		retry := attempt(n)
		last := retry.DeadlineAt.Add(-time.Millisecond) // 这次尝试最晚还能执行的时刻
		run(other(int(n)), last)                        // 同一实体上别的操作在此刻 Complete，顺手剪过期回执
		if outcome := run(retry, last); outcome.Kind != StepReplayed || outcome.Completion.CommandID != first.ID {
			t.Fatalf("attempt %d at +%v = %+v, want a replay of %s", n, last.Sub(t0), outcome, first.ID)
		}
	}
	if effects != 1 {
		t.Fatalf("one operation took effect %d times inside its retry window, want 1", effects)
	}
	run(other(0), t0.Add(retention+time.Second))
	if _, kept := entity.receipts[first.IdempotencyKey]; kept {
		t.Fatal("a receipt past the retention was not pruned: the test would not notice an early prune")
	}
}

func TestStepReceiptEncodingRoundTrips(t *testing.T) {
	for _, receipt := range []StepReceipt{
		{CommandID: "a:1:0:1", Success: true, At: 7},
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

// 原生步骤成功的结果带这一步的命令载荷（维护者 2026-10-29 选 A）：执行与回放交出逐字节相同的结果（协调器按 CommandID 的回执摘要
// 去重，不同就是身份冲突）；拒绝不带 Data。补偿迟到那一步的命令载荷因此是参与方解得开的那份（late_success_test.go 的 ④ 用例）。
func TestNativeStepSuccessCarriesItsCommandPayload(t *testing.T) {
	h := newNativeHarness()
	first := stepCommand(0, 1, time.Minute)
	first.Payload = []byte(`{"from":7,"item":3}`)
	executed := h.run(t, first)
	if !bytes.Equal(executed.Completion.Data, first.Payload) {
		t.Fatalf("executed success Data = %q, want the command payload %q", executed.Completion.Data, first.Payload)
	}
	second := stepCommand(0, 2, time.Minute)
	second.Payload = first.Payload // 同一操作实例的各次尝试载荷相同
	replayed := h.run(t, second)
	if replayed.Kind != StepReplayed || receiptDigest(replayed.Completion) != receiptDigest(executed.Completion) {
		t.Fatalf("replay = %+v, want the executed result byte for byte (digest %s vs %s)", replayed, receiptDigest(replayed.Completion), receiptDigest(executed.Completion))
	}
	refusing := newNativeHarness()
	refusing.success, refusing.reason = false, "no"
	if refused := refusing.run(t, first); refused.Completion.Data != nil {
		t.Fatalf("a refusal carries Data %q, want none", refused.Completion.Data)
	}
}

// 原生步骤 handler 的登记（维护者 2026-10-29）：durability 只能 strict / pipelined（运行期与生成器同一条规则），登记同时进
// Nest 的包级注册表；PipelinedNativeStepHandlers 只列 pipelined 的（装配层据此补白名单、开异步完成），Nest 拒绝注册时不登记。
func TestNativeStepHandlerRegistration(t *testing.T) {
	t.Cleanup(func() {
		corenest.ResetHandlersForTest()
		ResetNativeStepHandlersForTest()
	})
	noop := func([]entity.IThreadSafeEntity, []any, ...corenest.HandlerOption) (any, error) { return nil, nil }
	for _, durability := range []corenest.DurabilityPolicy{corenest.DurabilityAsync, corenest.DurabilityMemory} {
		name := corenest.NewHandlerName("test_native_" + durability.String())
		if err := RegisterNativeStepHandler(name, noop, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: durability}); err == nil || !strings.Contains(err.Error(), "strict or pipelined") {
			t.Fatalf("durability %s: RegisterNativeStepHandler = %v, want a refusal", durability, err)
		}
		if corenest.GetHandler(name) != nil {
			t.Fatalf("durability %s: a refused native step reached the Nest registry", durability)
		}
	}
	MustRegisterNativeStepHandler(corenest.NewHandlerName("test_native_b"), noop, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined})
	MustRegisterNativeStepHandler(corenest.NewHandlerName("test_native_a"), noop, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityPipelined})
	MustRegisterNativeStepHandler(corenest.NewHandlerName("test_native_strict"), noop, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict})
	if corenest.GetHandler(corenest.NewHandlerName("test_native_a")) == nil {
		t.Fatal("a native step handler must be registered with Nest")
	}
	if err := RegisterNativeStepHandler(corenest.NewHandlerName("test_native_a"), noop, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict}); err == nil {
		t.Fatal("a duplicate native step handler was accepted")
	}
	if got := PipelinedNativeStepHandlers(); !slices.Equal(got, []string{"test_native_a", "test_native_b"}) {
		t.Fatalf("PipelinedNativeStepHandlers = %v, want the two pipelined steps, sorted (the duplicate must not overwrite a's durability)", got)
	}
}
