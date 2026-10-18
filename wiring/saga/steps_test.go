package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// Steps 是 D12 方向 A 的步骤运行时：业务登记“名字 → 步骤体”，Start 按 saga Mod 与 DataEngine Mod 的声明组装并订阅。
// 这里的 JetStream 是记录订阅、能手动投递的替身；Mongo 是 mongotest，收件箱与事务是真实实现。

type stepsSubscription struct {
	mu      sync.Mutex
	drains  int
	release chan struct{}
	once    sync.Once
	closed  chan struct{}
}

func (s *stepsSubscription) Stop() { s.once.Do(func() { close(s.closed) }) }
func (s *stepsSubscription) Drain() {
	s.mu.Lock()
	s.drains++
	s.mu.Unlock()
	// 在途投递处理完（release）之后才关闭，与真实消费者一样。
	go func() {
		<-s.release
		s.Stop()
	}()
}
func (s *stepsSubscription) Closed() <-chan struct{} { return s.closed }

type stepsJetStream struct {
	mu       sync.Mutex
	configs  []fnats.JetStreamConsumerConfig
	handlers map[string]fnats.JetStreamHandler
	subs     []*stepsSubscription
	failOn   string
}

func (*stepsJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (*stepsJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (j *stepsJetStream) Subscribe(_ context.Context, cfg fnats.JetStreamConsumerConfig, handler fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	j.mu.Lock()
	defer j.mu.Unlock()
	if cfg.Durable == j.failOn {
		return nil, errors.New("jetstream: consumer refused")
	}
	if j.handlers == nil {
		j.handlers = map[string]fnats.JetStreamHandler{}
	}
	j.configs = append(j.configs, cfg)
	j.handlers[cfg.Durable] = handler
	sub := &stepsSubscription{release: make(chan struct{}), closed: make(chan struct{})}
	j.subs = append(j.subs, sub)
	return sub, nil
}

func (j *stepsJetStream) deliver(t *testing.T, durable string, command coresaga.Command) error {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"version": coresaga.WireVersion, "command": command})
	if err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	handler := j.handlers[durable]
	j.mu.Unlock()
	if handler == nil {
		t.Fatalf("no consumer %q", durable)
	}
	return handler(context.Background(), &fnats.JetStreamMsg{Subject: "roost.saga.command." + command.Topic, Data: raw})
}

func stepsRegistry(t *testing.T, js *stepsJetStream) *app.Registry {
	t.Helper()
	registry := app.NewRegistry(viper.New())
	if err := registry.Register(mods.ModMongo, mongotest.NewClient()); err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(mods.ModNatsJetStream, js); err != nil {
		t.Fatal(err)
	}
	return registry
}

func stepCommand(id, topic string) coresaga.Command {
	now := time.Now().UTC()
	return coresaga.Command{
		ID: id, IdempotencyKey: "op-" + id, SagaID: "saga-1", SagaType: "gift", DefinitionVersion: 1,
		BusinessKey: "gift-1", StepName: "deliver", Phase: coresaga.PhaseForward, Attempt: 1,
		Topic: topic, Payload: []byte(`{}`), CreatedAt: now, DeadlineAt: now.Add(time.Minute),
	}
}

func nativeBody(context.Context, coresaga.NativeStep) error { return nil }

// 每个登记的步骤一个 durable（<group>-<name>），订阅 saga Mod 声明的流与命令前缀；顺序即登记顺序。
func TestStepsSubscribeEveryStepOnTheSagaModsStream(t *testing.T) {
	js := &stepsJetStream{}
	steps, err := NewSteps(stepsRegistry(t, js), "game-gift")
	if err != nil {
		t.Fatal(err)
	}
	steps.Native("debit", "gift.debit", nativeBody)
	steps.Mongo("deliver", "gift.deliver", func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		return coresaga.Completion{Success: true}, nil
	})
	if err := steps.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []struct{ durable, subject string }{{"game-gift-debit", "roost.saga.command.gift.debit"}, {"game-gift-deliver", "roost.saga.command.gift.deliver"}}
	if len(js.configs) != len(want) {
		t.Fatalf("subscribed %d consumers, want %d", len(js.configs), len(want))
	}
	for i, w := range want {
		if got := js.configs[i]; got.Durable != w.durable || got.FilterSubject != w.subject || got.Stream != "ROOST_SAGA" || got.AckWait != coresaga.DefaultStepAckWait {
			t.Errorf("consumer %d = %+v, want durable %s on %s", i, got, w.durable, w.subject)
		}
	}
	if err := steps.Start(context.Background()); err == nil {
		t.Fatal("a second Start succeeded")
	}
}

// 登记期的错误（重名、非法名字、空处理函数、Start 之后登记）在 Start 时一并返回，什么都不订阅。
func TestStepsReportRegistrationMistakesAtStart(t *testing.T) {
	js := &stepsJetStream{}
	steps, err := NewSteps(stepsRegistry(t, js), "game-gift")
	if err != nil {
		t.Fatal(err)
	}
	steps.Native("debit", "gift.debit", nativeBody)
	steps.Native("debit", "gift.debit.compensate", nativeBody)
	steps.Native("bad.name", "gift.x", nativeBody)
	steps.Mongo("deliver", "gift.deliver", nil)
	err = steps.Start(context.Background())
	for _, want := range []string{`"debit" registered twice`, `"bad.name"`, `"deliver" has no handler`} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Start = %v, want it to report %s", err, want)
		}
	}
	if len(js.configs) != 0 {
		t.Fatalf("subscribed %d consumers despite registration errors", len(js.configs))
	}
	if _, err := NewSteps(nil, "game.gift"); err == nil {
		t.Fatal("a group name with '.' was accepted (it becomes part of every durable name)")
	}
}

// 某个步骤订阅失败：撤掉已订阅的消费者，返回点名步骤的错误。
func TestStepsStartRollsBackOnASubscribeFailure(t *testing.T) {
	js := &stepsJetStream{failOn: "game-gift-deliver"}
	steps, _ := NewSteps(stepsRegistry(t, js), "game-gift")
	steps.Native("debit", "gift.debit", nativeBody)
	steps.Mongo("deliver", "gift.deliver", func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		return coresaga.Completion{}, nil
	})
	if err := steps.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "subscribe deliver") {
		t.Fatalf("Start = %v, want the deliver subscription failure", err)
	}
	select {
	case <-js.subs[0].Closed():
	default:
		t.Fatal("the debit consumer subscribed before the failure was left running")
	}
}

// Drain 按停止入口三步：发起（幂等，每个消费者只 Drain 一次）、在 ctx 内等真实排空（超时返回错误、保留消费者）、
// 新的 ctx 再调用接着等。修前 demo 的停止函数只发起 Drain 就返回，Shutdown 报告“已停”时 handler 可能还在跑。
func TestStepsDrainWaitsForInFlightDeliveriesWithinTheContext(t *testing.T) {
	js := &stepsJetStream{}
	steps, _ := NewSteps(stepsRegistry(t, js), "game-gift")
	steps.Native("debit", "gift.debit", nativeBody)
	steps.Native("refund", "gift.debit.compensate", nativeBody)
	if err := steps.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	short, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := steps.Drain(short); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Drain with deliveries still in flight = %v, want the context's deadline", err)
	}
	for _, sub := range js.subs {
		close(sub.release)
	}
	if err := steps.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after the deliveries finished: %v", err)
	}
	for i, sub := range js.subs {
		if sub.drains != 1 {
			t.Errorf("consumer %d drained %d times, want 1 (Drain is idempotent)", i, sub.drains)
		}
	}
	var never *Steps
	if err := never.Drain(context.Background()); err != nil {
		t.Fatalf("Drain on a nil Steps: %v", err)
	}
}

// 转交接收方的检查在取执行权之前：未登记的名字、不是原生步骤、命令主题与该步骤订阅的主题不一致（扣款信封包着
// 退款命令）都拒绝，而且在归属复核之前（不为拒绝的转交把玩家接入本进程）；未 Start 时报 ErrStepsNotStarted。
func TestStepsRunHandedOverChecksTheStepBeforeTheClaim(t *testing.T) {
	steps, _ := NewSteps(app.NewRegistry(viper.New()), "game-gift")
	steps.Native("debit", "gift.debit", nativeBody)
	steps.Mongo("deliver", "gift.deliver", func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		return coresaga.Completion{}, nil
	})
	for name, tc := range map[string]struct {
		step, topic, want string
	}{
		"unknown step":   {"refund", "gift.debit.compensate", "not registered"},
		"mongo step":     {"deliver", "gift.deliver", "not a native step"},
		"topic mismatch": {"debit", "gift.debit.compensate", `published on "gift.debit.compensate", want "gift.debit"`},
	} {
		accepted := false
		err := steps.RunHandedOver(context.Background(), tc.step, stepCommand("cmd-"+tc.step, tc.topic), func(context.Context, coresaga.Command) bool {
			accepted = true
			return true
		})
		if accepted {
			t.Errorf("%s: the ownership check ran for a handoff that should have been refused first", name)
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) || errors.Is(err, ErrStepsNotStarted) {
			t.Errorf("%s: %v, want a refusal mentioning %q", name, err, tc.want)
		}
	}
	if err := steps.RunHandedOver(context.Background(), "debit", stepCommand("cmd-ok", "gift.debit"), nil); !errors.Is(err, ErrStepsNotStarted) {
		t.Fatalf("a valid handoff before Start = %v, want ErrStepsNotStarted", err)
	}
	// 归属复核说“不归我”：安静放弃（nil），不取执行权；过了截止的命令连复核都不问。
	if err := steps.RunHandedOver(context.Background(), "debit", stepCommand("cmd-foreign", "gift.debit"), func(context.Context, coresaga.Command) bool { return false }); err != nil {
		t.Fatalf("a handoff the accept callback declines = %v, want nil", err)
	}
	late := stepCommand("cmd-late", "gift.debit")
	late.DeadlineAt = time.Now().Add(-time.Second)
	if err := steps.RunHandedOver(context.Background(), "debit", late, func(context.Context, coresaga.Command) bool {
		t.Error("the ownership check ran for an expired handoff")
		return true
	}); err != nil {
		t.Fatalf("an expired handoff = %v, want nil", err)
	}
}

// 指标：准入拒绝与每次执行都按 durable 计数；Mongo 步骤区分 ok / refused / error。坏信封仍由框架 Term。
func TestStepsCountRunsAndAdmissionRefusals(t *testing.T) {
	js := &stepsJetStream{}
	// app.NewRegistry 安装进程级指标注册表（App 的 /metrics 导出它），从它读回。
	appRegistry := stepsRegistry(t, js)
	registry := app.MustLookup[*metrics.Registry](appRegistry, app.ModMetrics)
	steps, _ := NewSteps(appRegistry, "game-gift")
	refuse := true
	steps.Mongo("deliver", "gift.deliver", func(_ context.Context, command coresaga.Command) (coresaga.Completion, error) {
		if command.ID == "cmd-refused" {
			return coresaga.Completion{Success: false, Error: "mailbox full"}, nil
		}
		return coresaga.Completion{Success: true}, nil
	}, WithAdmit(func(context.Context, coresaga.Command) error {
		if refuse {
			return errors.New("not mine")
		}
		return nil
	}))
	if err := steps.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := js.deliver(t, "game-gift-deliver", stepCommand("cmd-ok", "gift.deliver")); err == nil || !strings.Contains(err.Error(), "not mine") {
		t.Fatalf("a refused admission = %v, want the refusal (nak)", err)
	}
	refuse = false
	if err := js.deliver(t, "game-gift-deliver", stepCommand("cmd-ok", "gift.deliver")); err != nil {
		t.Fatal(err)
	}
	if err := js.deliver(t, "game-gift-deliver", stepCommand("cmd-refused", "gift.deliver")); err != nil {
		t.Fatal(err)
	}
	counter := func(name string, labels metrics.Labels) int64 {
		for _, metric := range registry.Snapshot() {
			if metric.Name != name {
				continue
			}
			match := true
			for key, value := range labels {
				if metric.Labels[key] != value {
					match = false
				}
			}
			if match {
				return metric.Value
			}
		}
		return 0
	}
	if got := counter("saga.step.admit_refused_total", metrics.Labels{"consumer": "game-gift-deliver"}); got != 1 {
		t.Errorf("admit_refused_total = %d, want 1", got)
	}
	for result, want := range map[string]int64{"ok": 1, "refused": 1} {
		if got := counter("saga.step.run_total", metrics.Labels{"consumer": "game-gift-deliver", "result": result}); got != want {
			t.Errorf("run_total{result=%s} = %d, want %d", result, got, want)
		}
	}
	handler := js.handlers["game-gift-deliver"]
	if err := handler(context.Background(), &fnats.JetStreamMsg{Data: []byte("not json")}); !fnats.IsPermanent(err) {
		t.Fatalf("a poison envelope = %v, want a permanent (Term) error", err)
	}
}
