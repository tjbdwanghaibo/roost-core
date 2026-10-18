package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	deengine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/infra/network/bus"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
)

// NativeStep 是业务写原生步骤体用的形状（D12 方向 A）：框架做预留、回执、completion 与等待投影，业务的 Nest handler
// 只调用 step.Complete。这些用例走真实的 SubscribeDataEngineStep 消费者、mongotest 上真实的原生收件箱与真实的
// DataEngine 投影器（deengine.MongoStore.Project，lease fence 的接受 / 跳过是生产判定）；“Nest 提交”用
// corenest.RunIsolatedTransaction 截下 CommitRecord，投影即生效。body 的调用次数就是业务执行次数。

type nativeStepHarness struct {
	t         *testing.T
	mongo     *mongotest.Client
	inbox     *DataEngineStepInbox
	jetstream *recordingJetStream
	project   func(coredata.CommitRecord)
	runs      atomic.Int32
	// outcome 决定 body 在事务里怎样结束：nil 表示 Complete(success, reason)，否则 body 不提交、返回这个错误。
	success bool
	reason  string
	fail    error
}

func newNativeStepHarness(t *testing.T) *nativeStepHarness {
	t.Helper()
	h := &nativeStepHarness{t: t, mongo: mongotest.NewClient(), jetstream: newRecordingJetStream(), success: true}
	inbox, err := NewDataEngineStepInbox(h.mongo, "game", DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: 2 * time.Minute, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	h.inbox = inbox
	projector, err := deengine.NewMongoStore(h.mongo, deengine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 3, TransactionReceiptTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	h.project = func(record coredata.CommitRecord) {
		t.Helper()
		if err := projector.Project(context.Background(), record); err != nil {
			t.Fatalf("project %s: %v", record.ID, err)
		}
	}
	transport, err := NewJetStreamPublisher(h.jetstream, "roost.saga")
	if err != nil {
		t.Fatal(err)
	}
	config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "game-rally-reserve", Topic: "rally.reserve"}
	if _, err := SubscribeDataEngineStep(context.Background(), h.jetstream, transport, inbox, config, inbox.StepHandler(h.body)); err != nil {
		t.Fatal(err)
	}
	return h
}

// body 是业务的原生步骤体：在“Nest 事务”里调用 step.Complete，提交后立即投影。
func (h *nativeStepHarness) body(ctx context.Context, step NativeStep) error {
	h.runs.Add(1)
	if h.fail != nil {
		return h.fail
	}
	committer := &stepFenceCommitter{}
	if _, err := corenest.RunIsolatedTransaction(ctx, committer, "rally-reserve", func() (any, error) {
		return nil, step.Complete(h.success, h.reason)
	}); err != nil {
		return err
	}
	h.project(committer.record)
	return nil
}

func (h *nativeStepHarness) deliver(command Command) error {
	h.t.Helper()
	raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: command})
	if err != nil {
		h.t.Fatal(err)
	}
	return h.jetstream.deliver(h.t, "roost.saga.command.rally.reserve", raw)
}

func (h *nativeStepHarness) receipt(command Command) Completion {
	h.t.Helper()
	completion, found, err := h.inbox.Replay(context.Background(), command)
	if err != nil || !found {
		h.t.Fatalf("receipt of %s: found=%t err=%v", command.ID, found, err)
	}
	return completion
}

// 同一条命令投递两次（JetStream 至少一次）：步骤体只执行一次，第二次读到回执直接 ack。正向与补偿方向同一条路径。
func TestNativeStepRunsOnceAndAcknowledgesARedelivery(t *testing.T) {
	for _, phase := range []Phase{PhaseForward, PhaseCompensate} {
		t.Run(string(phase), func(t *testing.T) {
			h := newNativeStepHarness(t)
			command := dataEngineCommand("command-"+string(phase), "operation-"+string(phase), "payload")
			command.Phase = phase
			for delivery := 1; delivery <= 2; delivery++ {
				if err := h.deliver(command); err != nil {
					t.Fatalf("delivery %d: %v", delivery, err)
				}
			}
			if got := h.runs.Load(); got != 1 {
				t.Fatalf("the step body ran %d times for one command delivered twice, want 1", got)
			}
			if completion := h.receipt(command); !completion.Success || completion.CommandID != command.ID || completion.IdempotencyKey != command.IdempotencyKey {
				t.Fatalf("receipt completion = %+v", completion)
			}
		})
	}
}

// 业务拒绝也提交：回执与一份不可重试的失败 completion，协调器据此补偿；reason 为空时填一条点名步骤的说明。
func TestNativeStepRefusalCommitsAFailedCompletion(t *testing.T) {
	for reason, want := range map[string]string{"item_short": "item_short", "": "native step reserve refused"} {
		h := newNativeStepHarness(t)
		h.success, h.reason = false, reason
		command := dataEngineCommand("command-refused-"+want, "operation-refused-"+want, "payload")
		if err := h.deliver(command); err != nil {
			t.Fatal(err)
		}
		completion := h.receipt(command)
		if completion.Success || completion.Retryable || !strings.Contains(completion.Error, want) {
			t.Fatalf("refusal with reason %q committed %+v, want a non-retryable failure mentioning %q", reason, completion, want)
		}
	}
}

// 结果未知：步骤体返回错误（Nest 提交可能已经发生），消费者保留这次尝试的租约。同一命令的重投不再执行步骤体，
// 等回执到命令截止后返回（nak）；下一次尝试由协调器按步骤超时发出。
func TestNativeStepUnknownOutcomeIsNotRunAgainByARedelivery(t *testing.T) {
	h := newNativeStepHarness(t)
	h.fail = errors.New("nest: commit outcome unknown")
	command := dataEngineCommand("command-unknown", "operation-unknown", "payload")
	command.DeadlineAt = time.Now().Add(300 * time.Millisecond)
	if err := h.deliver(command); !errors.Is(err, h.fail) {
		t.Fatalf("first delivery = %v, want the body's error (nak)", err)
	}
	if err := h.deliver(command); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("redelivery while the first attempt holds its lease = %v, want it to wait for the receipt until the command deadline", err)
	}
	if got := h.runs.Load(); got != 1 {
		t.Fatalf("the step body ran %d times; an attempt whose outcome is unknown must not be run again by a redelivery", got)
	}
}

// 唯一例外：ErrFencedEntityPending（实体上还有一笔结果未定的原生步骤，本次事务整体回滚）交还租约，重投立刻重试。
func TestNativeStepFencedEntityPendingHandsTheLeaseBack(t *testing.T) {
	h := newNativeStepHarness(t)
	h.fail = coredata.ErrFencedEntityPending
	command := dataEngineCommand("command-fenced-pending", "operation-fenced-pending", "payload")
	if err := h.deliver(command); !errors.Is(err, coredata.ErrFencedEntityPending) {
		t.Fatalf("first delivery = %v", err)
	}
	h.fail = nil
	if err := h.deliver(command); err != nil {
		t.Fatalf("redelivery after the barrier cleared: %v", err)
	}
	if got := h.runs.Load(); got != 2 || !h.receipt(command).Success {
		t.Fatalf("runs=%d; the redelivery should have taken the handed-back lease and committed", got)
	}
}

// NativeStep 只由框架构造：零值不能提交，步骤体不在原生消费者里（ctx 上没有执行权）时不调用。
func TestNativeStepCannotCompleteWithoutAReservation(t *testing.T) {
	committer := &stepFenceCommitter{}
	_, err := corenest.RunIsolatedTransaction(context.Background(), committer, "zero-step", func() (any, error) {
		return nil, NativeStep{}.Complete(true, "")
	})
	if !errors.Is(err, ErrNoNativeStep) {
		t.Fatalf("zero NativeStep.Complete = %v, want ErrNoNativeStep", err)
	}
	h := newNativeStepHarness(t)
	if _, err := h.inbox.StepHandler(h.body)(context.Background(), dataEngineCommand("command-bare", "operation-bare", "payload")); !errors.Is(err, ErrNoNativeStep) {
		t.Fatalf("StepHandler without a reservation on the context = %v, want ErrNoNativeStep", err)
	}
	if got := h.runs.Load(); got != 0 {
		t.Fatalf("the body ran %d times without a reservation", got)
	}
}

// RunHandedOver 是转交接收方：自己取执行权运行步骤体，与消费者互斥（同一操作实例的状态文档）。
func TestRunHandedOverSharesTheClaimWithTheConsumer(t *testing.T) {
	t.Run("runs, then the consumer finds the receipt", func(t *testing.T) {
		h := newNativeStepHarness(t)
		command := dataEngineCommand("command-handoff", "operation-handoff", "payload")
		if err := h.inbox.RunHandedOver(context.Background(), command, h.body); err != nil {
			t.Fatal(err)
		}
		if err := h.deliver(command); err != nil {
			t.Fatalf("the consumer's delivery after the handed-over run: %v", err)
		}
		if got := h.runs.Load(); got != 1 || !h.receipt(command).Success {
			t.Fatalf("runs=%d, want the handed-over run only", got)
		}
		// 再转交一次同一条命令：已有结果，不执行。
		if err := h.inbox.RunHandedOver(context.Background(), command, h.body); err != nil || h.runs.Load() != 1 {
			t.Fatalf("second handoff of a finished command: err=%v runs=%d", err, h.runs.Load())
		}
	})
	t.Run("the consumer holds the claim", func(t *testing.T) {
		h := newNativeStepHarness(t)
		h.fail = errors.New("nest: commit outcome unknown")
		command := dataEngineCommand("command-handoff-busy", "operation-handoff-busy", "payload")
		if err := h.deliver(command); err == nil {
			t.Fatal("first delivery should fail with the body's error")
		}
		if err := h.inbox.RunHandedOver(context.Background(), command, h.body); err != nil {
			t.Fatalf("handoff while the consumer holds the lease = %v, want nil (the holder finishes it)", err)
		}
		if got := h.runs.Load(); got != 1 {
			t.Fatalf("runs=%d; the handoff ran a command another delivery holds", got)
		}
	})
	// 转交走业务自己的通道（demo 是 bus，MessagePack 编码）：命令的时间字段回来时是同一时刻、换了时区（MessagePack 解出
	// 本地时区，协调器写的是 UTC）。命令身份按时刻判定，不能因为时区不同就成了“同一命令 ID、不同内容”：修前两条路径
	// 互相 ErrIdentityConflict，消费者那条消息读不到自己的回执，nak 到 MaxDeliver。
	t.Run("a handoff whose times came back in another zone", func(t *testing.T) {
		zone := time.FixedZone("UTC+8", 8*3600)
		shifted := func(command Command) Command {
			command.CreatedAt, command.DeadlineAt = command.CreatedAt.In(zone), command.DeadlineAt.In(zone)
			return command
		}
		h := newNativeStepHarness(t)
		command := dataEngineCommand("command-handoff-zone", "operation-handoff-zone", "payload")
		if err := h.inbox.RunHandedOver(context.Background(), shifted(command), h.body); err != nil {
			t.Fatal(err)
		}
		if err := h.deliver(command); err != nil {
			t.Fatalf("the consumer's delivery after a handoff that crossed the bus: %v", err)
		}
		if got := h.runs.Load(); got != 1 || !h.receipt(command).Success {
			t.Fatalf("runs=%d, want the handed-over run only", got)
		}

		h = newNativeStepHarness(t)
		command = dataEngineCommand("command-zone-consumer-first", "operation-zone-consumer-first", "payload")
		if err := h.deliver(command); err != nil {
			t.Fatal(err)
		}
		if err := h.inbox.RunHandedOver(context.Background(), shifted(command), h.body); err != nil || h.runs.Load() != 1 {
			t.Fatalf("handoff of a finished command whose times came back in another zone: err=%v runs=%d, want nil and no second run", err, h.runs.Load())
		}

		// 真实的 bus 编码：同一条命令过一次 MessagePack 后身份不变。
		var decoded Command
		raw, err := bus.MessagePackCodec{}.Marshal(command)
		if err == nil {
			err = bus.MessagePackCodec{}.Unmarshal(raw, &decoded)
		}
		original, _ := commandDigest(command)
		roundTripped, digestErr := commandDigest(decoded)
		if err != nil || digestErr != nil || string(original) != string(roundTripped) {
			t.Fatalf("command identity changed across the bus codec (err=%v %v): created_at %s -> %s", err, digestErr, command.CreatedAt, decoded.CreatedAt)
		}
	})
	t.Run("past its deadline", func(t *testing.T) {
		h := newNativeStepHarness(t)
		command := dataEngineCommand("command-handoff-late", "operation-handoff-late", "payload")
		command.DeadlineAt = time.Now().Add(-time.Second)
		if err := h.inbox.RunHandedOver(context.Background(), command, h.body); err != nil || h.runs.Load() != 0 {
			t.Fatalf("expired handoff: err=%v runs=%d, want nil and nothing run", err, h.runs.Load())
		}
	})
	t.Run("no inbox", func(t *testing.T) {
		var inbox *DataEngineStepInbox
		command := dataEngineCommand("command-handoff-nil", "operation-handoff-nil", "payload")
		if err := inbox.RunHandedOver(context.Background(), command, func(context.Context, NativeStep) error { return nil }); !errors.Is(err, ErrInvalidRecord) {
			t.Fatalf("nil inbox = %v, want ErrInvalidRecord", err)
		}
	})
}

// deferringJetStream 是支持推迟结算的单 durable 替身：deliver 像真实传输一样给消息挂上 DeferSettle，结算结果（含
// handler 同步返回的）送进 settled。
type deferringJetStream struct {
	*recordingJetStream
	settled chan error
}

func (j *deferringJetStream) deliverDeferrable(subject string, data []byte) {
	j.mu.Lock()
	handler := j.handlers[subject]
	j.mu.Unlock()
	var deferred atomic.Bool
	message := &fnats.JetStreamMsg{Subject: subject, Data: data}
	message.DeferSettle = func() func(error) {
		if !deferred.CompareAndSwap(false, true) {
			return nil
		}
		var once atomic.Bool
		return func(err error) {
			if once.CompareAndSwap(false, true) {
				j.settled <- err
			}
		}
	}
	if err := handler(context.Background(), message); err != nil || !deferred.Load() {
		j.settled <- err
	}
}

// 后台等回执有上限（StepConsumerConfig.ReceiptWaiters）：投影停顿时，前两条命令推迟结算后回调立即返回，第三条的回调
// 在空位上等（回压，不起第三个后台等待）；第一条的回执可见并 ack 之后第三条才进入后台。每条最终 ack（nil）一次。
func TestNativeStepReceiptWaitersAreBounded(t *testing.T) {
	mongo := mongotest.NewClient()
	inbox, err := NewDataEngineStepInbox(mongo, "game", DataEngineStepInboxOptions{Owner: "worker-1", LeaseDuration: 2 * time.Minute, PollInterval: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	projector, err := deengine.NewMongoStore(mongo, deengine.MongoStoreConfig{DefaultDatabase: "game", ServerID: 3, TransactionReceiptTTL: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	js := &deferringJetStream{recordingJetStream: newRecordingJetStream(), settled: make(chan error, 8)}
	transport, err := NewJetStreamPublisher(js, "roost.saga")
	if err != nil {
		t.Fatal(err)
	}
	records := make(chan coredata.CommitRecord, 4)
	body := func(ctx context.Context, step NativeStep) error {
		committer := &stepFenceCommitter{}
		if _, err := corenest.RunIsolatedTransaction(ctx, committer, "bounded", func() (any, error) {
			return nil, step.Complete(true, "")
		}); err != nil {
			return err
		}
		records <- committer.record // 投影停顿：由用例决定何时投影
		return nil
	}
	config := StepConsumerConfig{Stream: "ROOST_SAGA", Durable: "bounded", Topic: "rally.reserve", ReceiptWaiters: 2}
	if _, err := SubscribeDataEngineStep(context.Background(), js, transport, inbox, config, inbox.StepHandler(body)); err != nil {
		t.Fatal(err)
	}
	deliver := func(id string) <-chan struct{} {
		returned := make(chan struct{})
		raw, err := json.Marshal(commandEnvelope{Version: WireVersion, Command: dataEngineCommand(id, "op-"+id, id)})
		if err != nil {
			t.Fatal(err)
		}
		go func() {
			defer close(returned)
			js.deliverDeferrable("roost.saga.command.rally.reserve", raw)
		}()
		return returned
	}
	project := func() {
		t.Helper()
		if err := projector.Project(context.Background(), <-records); err != nil {
			t.Fatal(err)
		}
	}
	for _, id := range []string{"bounded-1", "bounded-2"} {
		select {
		case <-deliver(id):
		case <-time.After(5 * time.Second):
			t.Fatalf("callback for %s did not return while a waiter slot was free", id)
		}
	}
	third := deliver("bounded-3")
	select {
	case <-third:
		t.Fatal("third callback returned with both receipt waiters busy: the background wait is not bounded")
	case <-time.After(100 * time.Millisecond):
	}
	project() // bounded-1 的回执可见 → 它的后台等待 ack、让出空位
	select {
	case err := <-js.settled:
		if err != nil {
			t.Fatalf("first settle = %v, want ack", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first command was not acked after its receipt became visible")
	}
	select {
	case <-third:
	case <-time.After(5 * time.Second):
		t.Fatal("third callback still blocked after a waiter slot was freed")
	}
	project()
	project()
	for range 2 {
		select {
		case err := <-js.settled:
			if err != nil {
				t.Fatalf("settle = %v, want ack", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("a command was not acked after its receipt became visible")
		}
	}
}
