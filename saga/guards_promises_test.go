package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
)

// U-0148 · C2 · gap map core `saga` 6/20：Assemble 缺 Mongo / JetStream 拒绝；步骤消费者对 nil 消息、
// 超大信封、异版本信封以永久错误（不重投）拒绝。`command_consumer.go:124`（UpdateOne 在同一事务里
// 匹配数不为 1）只在刚插入的回执于同一事务内消失时可达，防御性守卫，保留。
func TestAssembleAndStepConsumerRefuseMissingPartsAndForeignEnvelopes(t *testing.T) {
	ctx := context.Background()
	js := &startJetStream{}
	if _, err := Assemble(nil, js, AssemblyConfig{}); err == nil || !strings.Contains(err.Error(), "mongo client is required") {
		t.Fatalf("Assemble without mongo = %v", err)
	}
	if _, err := Assemble(mongotest.NewClient(), nil, AssemblyConfig{}); err == nil || !strings.Contains(err.Error(), "jetstream client is required") {
		t.Fatalf("Assemble without jetstream = %v", err)
	}

	transport, err := NewJetStreamPublisher(js, "saga")
	if err != nil {
		t.Fatal(err)
	}
	inbox, err := NewMongoCommandInbox(mongotest.NewClient(), "saga", "step_inbox")
	if err != nil {
		t.Fatal(err)
	}
	handled := 0
	handler := func(context.Context, Command) (Completion, error) { handled++; return Completion{}, nil }
	if _, err := SubscribeMongoStep(ctx, js, transport, inbox, StepConsumerConfig{Stream: "SAGA", Durable: "step-charge", Topic: "charge"}, handler); err != nil {
		t.Fatal(err)
	}
	if js.handler == nil {
		t.Fatal("subscription did not install a handler")
	}
	// 异版本信封里放一条完全合法的命令：没有版本守卫它会被当作正常命令执行。
	now := time.Now().UTC()
	valid := Command{ID: "cmd-1", IdempotencyKey: "idem-1", SagaID: "saga1", SagaType: "order", DefinitionVersion: 1, BusinessKey: "bk-1", StepName: "charge", Phase: PhaseForward, Attempt: 1, Topic: "charge", DeadlineAt: now.Add(time.Hour), CreatedAt: now}
	if err := valid.Validate(); err != nil {
		t.Fatalf("fixture command invalid: %v", err)
	}
	foreign, err := json.Marshal(commandEnvelope{Version: WireVersion + 1, Command: valid})
	if err != nil {
		t.Fatal(err)
	}
	cases := map[string]*fnats.JetStreamMsg{
		"nil message":     nil,
		"oversized":       {Data: make([]byte, maxWireEnvelopeBytes+1)},
		"foreign version": {Data: foreign},
	}
	for name, message := range cases {
		err := js.handler(ctx, message)
		if !errors.Is(err, ErrInvalidRecord) || !fnats.IsPermanent(err) {
			t.Fatalf("%s: handler = %v, want a permanent ErrInvalidRecord", name, err)
		}
	}
	if handled != 0 {
		t.Fatal("a refused envelope reached the step handler")
	}
}
