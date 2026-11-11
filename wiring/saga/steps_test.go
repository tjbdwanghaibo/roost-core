package saga

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

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

// deliver 把一条分区命令事件（协调器派发的形状：事件带 saga 的记录与这次尝试的命令）交给 durable。
func (j *stepsJetStream) deliver(t *testing.T, durable string, command coresaga.Command) error {
	t.Helper()
	raw, err := json.Marshal(map[string]any{"v": coresaga.WireVersion, "part": int(command.Partition), "writer": "w", "kind": "dispatch",
		"saga": map[string]any{"record": map[string]any{"ID": command.SagaID}}, "command": command})
	if err != nil {
		t.Fatal(err)
	}
	j.mu.Lock()
	handler := j.handlers[durable]
	j.mu.Unlock()
	if handler == nil {
		t.Fatalf("no consumer %q", durable)
	}
	return handler(context.Background(), &fnats.JetStreamMsg{Subject: "roost.saga.0.ev.cmd." + command.Topic, Data: raw})
}

// stepsRegistry 是一个分区、一条流、准入关闭的配置（准入信号要 DataEngine Mod 与 nats.log，见 TestStepsNeedDataEngineForTheAdmissionSignal）。
func stepsRegistry(t *testing.T, js *stepsJetStream) *app.Registry {
	t.Helper()
	cfg := sagaTestConfig()
	cfg.Set("saga.partitions", 1)
	cfg.Set("saga.stream_groups", 1)
	cfg.Set("saga.admission.enabled", false)
	registry := app.NewRegistry(cfg)
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

// 每个登记的步骤一个 durable（<group>-<name>），在 saga Mod 声明的每条流上过滤全部分区的这个主题；顺序即登记顺序。
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
	want := []struct{ durable, subject string }{{"game-gift-debit", "roost.saga.*.ev.cmd.gift.debit"}, {"game-gift-deliver", "roost.saga.*.ev.cmd.gift.deliver"}}
	if len(js.configs) != len(want) {
		t.Fatalf("subscribed %d consumers, want %d", len(js.configs), len(want))
	}
	for i, w := range want {
		if got := js.configs[i]; got.Durable != w.durable || got.FilterSubject != w.subject || got.Stream != "ROOST_SAGA_G0" || got.AckWait != coresaga.DefaultStepAckWait {
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

// 按 sid 路由的步骤（WithSidRoute）：每个 sid 一个 durable `<group>-<name>-<sid>`，只过滤投给本 sid 的命令
// （roost.saga.*.ev.to.<sid>.<topic>）；不路由的步骤照旧共用 `<group>-<name>`。本进程没有 sid 时拒绝启动。
func TestStepsRouteSidBoundStepsToThisSidsDurable(t *testing.T) {
	js := &stepsJetStream{}
	registry := stepsRegistry(t, js)
	registry.Config().Set("sid", 1002)
	steps, _ := NewSteps(registry, "game-gift")
	steps.Native("debit", "gift.debit", nativeBody, WithSidRoute())
	steps.Mongo("deliver", "gift.deliver", func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		return coresaga.Completion{Success: true}, nil
	})
	if err := steps.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := []struct{ durable, subject string }{{"game-gift-debit-1002", "roost.saga.*.ev.to.1002.gift.debit"}, {"game-gift-deliver", "roost.saga.*.ev.cmd.gift.deliver"}}
	if len(js.configs) != len(want) {
		t.Fatalf("subscribed %d consumers, want %d", len(js.configs), len(want))
	}
	for i, w := range want {
		if got := js.configs[i]; got.Durable != w.durable || got.FilterSubject != w.subject {
			t.Errorf("consumer %d = %s on %s, want %s on %s", i, got.Durable, got.FilterSubject, w.durable, w.subject)
		}
	}

	noSid := &stepsJetStream{}
	unnamed, _ := NewSteps(stepsRegistry(t, noSid), "game-gift")
	unnamed.Native("debit", "gift.debit", nativeBody, WithSidRoute())
	if err := unnamed.Start(context.Background()); err == nil || !strings.Contains(err.Error(), "has no sid") || len(noSid.configs) != 0 {
		t.Fatalf("a sid-routed step in a process without a sid: %v (%d consumers), want refused before subscribing", err, len(noSid.configs))
	}
}

// definitionTopics 是协调器的替身：按定义回答 RoutedTopic（*coresaga.Engine 的同一规则由 framework/saga 的用例钉住）。
type definitionTopics coresaga.Definition

func (d definitionTopics) RoutedTopic(topic string) (bool, bool) {
	for _, step := range d.Steps {
		if step.ForwardTopic == topic || step.CompensateTopic == topic {
			return step.Routed, true
		}
	}
	return false, false
}

// 同一进程里有协调器时，消费者的路由必须与定义一致：定义路由、消费者不路由（命令发往 ev.to.<sid>，没人过滤），或者反过来，
// 启动就报错，不订阅任何东西。
func TestStepsRefuseARouteThatDisagreesWithTheDefinition(t *testing.T) {
	definition := coresaga.Definition{Type: "gift", Version: 1, Steps: []coresaga.Step{
		{Name: "debit", ForwardTopic: "gift.debit", CompensateTopic: "gift.debit.compensate", Routed: true, Timeout: time.Second, MaxAttempts: 1, BackoffMin: time.Millisecond, BackoffMax: time.Millisecond},
		{Name: "deliver", ForwardTopic: "gift.deliver", CompensateTopic: "gift.deliver.compensate", Timeout: time.Second, MaxAttempts: 1, BackoffMin: time.Millisecond, BackoffMax: time.Millisecond},
	}}
	engine := definitionTopics(definition)
	for name, tc := range map[string]struct {
		topic  string
		routed bool
		want   string
	}{
		"routed definition, plain consumer":     {"gift.debit", false, "routed=true but the consumer says routed=false"},
		"compensation follows its step":         {"gift.debit.compensate", false, "routed=true but the consumer says routed=false"},
		"plain definition, sid-routed consumer": {"gift.deliver", true, "routed=false but the consumer says routed=true"},
	} {
		js := &stepsJetStream{}
		registry := stepsRegistry(t, js)
		registry.Config().Set("sid", 1000)
		if err := registry.Register(mods.ModSaga, engine); err != nil {
			t.Fatal(err)
		}
		steps, _ := NewSteps(registry, "game-gift")
		var options []StepOption
		if tc.routed {
			options = append(options, WithSidRoute())
		}
		steps.Native("step", tc.topic, nativeBody, options...)
		if err := steps.Start(context.Background()); err == nil || !strings.Contains(err.Error(), tc.want) || len(js.configs) != 0 {
			t.Errorf("%s: %v (%d consumers), want refused with %q before subscribing", name, err, len(js.configs), tc.want)
		}
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

// 有原生步骤且准入开启时，Steps 发布本进程 DataEngine 投影积压作准入信号：进程里没有 DataEngine Mod 是装配错误，Start 点名它。
func TestStepsNeedDataEngineForTheAdmissionSignal(t *testing.T) {
	js := &stepsJetStream{}
	cfg := sagaTestConfig()
	registry := app.NewRegistry(cfg)
	if err := registry.Register(mods.ModNatsJetStream, js); err != nil {
		t.Fatal(err)
	}
	steps, _ := NewSteps(registry, "game-gift")
	steps.Native("debit", "gift.debit", nativeBody)
	if err := steps.Start(context.Background()); err == nil || !strings.Contains(err.Error(), `"dataengine"`) {
		t.Fatalf("Start without the DataEngine Mod = %v, want a refusal naming it", err)
	}
	if len(js.configs) != 0 {
		t.Fatalf("subscribed %d consumers before the admission signal was assembled", len(js.configs))
	}
}

// 设计 T-A 第 2 条：参与方启动只读探测 transactionLifetimeLimitSeconds。长于缺省步骤预算窗口（框架缺省 5 × 5s 加四次退避
// = 26.5s）时 WARN 并计 saga.step.mongo_txn_lifetime_exceeds_budget；部署要求的 20s 不告警；读不到只记“未探测”。探针从不
// 拒绝启动。
func TestStepsProbeTheMongoTransactionLifetimeAgainstTheStepWindow(t *testing.T) {
	if got := stepWindow(stepBudgetConfig{}); got != 26500*time.Millisecond {
		t.Fatalf("default step window = %v, want 26.5s", got)
	}
	if got := stepWindow(stepBudgetConfig{Timeout: 10 * time.Second, MaxAttempts: 2, BackoffMin: time.Second, BackoffMax: time.Second}); got != 21*time.Second {
		t.Fatalf("configured step window = %v, want 21s", got)
	}
	exceeded := func() int64 {
		var total int64
		for _, metric := range metrics.Snapshot() {
			if metric.Name == "saga.step.mongo_txn_lifetime_exceeds_budget" && metric.Labels["group"] == "probe" {
				total += metric.Value
			}
		}
		return total
	}
	window := stepWindow(stepBudgetConfig{})
	before := exceeded()
	for _, tc := range []struct {
		name     string
		value    any
		lifetime time.Duration
		exceeds  bool
	}{
		{"server default", int32(60), time.Minute, true},
		{"deployment requirement", int32(20), 20 * time.Second, false},
		{"int64", int64(27), 27 * time.Second, true},
		{"not readable", nil, 0, false},
	} {
		client := mongotest.NewClient()
		if tc.value != nil {
			client.Parameters = map[string]any{transactionLifetimeParameter: tc.value}
		}
		lifetime, exceeds := probeTransactionLifetime(context.Background(), client, "probe", window)
		if lifetime != tc.lifetime || exceeds != tc.exceeds {
			t.Errorf("%s: probe = %v, %v; want %v, %v", tc.name, lifetime, exceeds, tc.lifetime, tc.exceeds)
		}
	}
	if got := exceeded() - before; got != 2 {
		t.Fatalf("saga.step.mongo_txn_lifetime_exceeds_budget += %d, want 2 (60s and 27s)", got)
	}
}

// Saga 步骤消费者是外部输入：Steps.Start 成功后把自己登记给 App（app.AddIngress），App 停机第一步调 Steps.Drain 等
// 在途步骤处理完，之后才停 Nest / DataEngine（App 生命周期方案缺口 6）。修前只靠业务 Shutdown 记得调 Drain。
func TestStepsAreClosedWithTheAppsInputs(t *testing.T) {
	js := &stepsJetStream{}
	registry := stepsRegistry(t, js)
	steps, _ := NewSteps(registry, "game-gift")
	steps.Native("debit", "gift.debit", nativeBody)
	if err := steps.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	closed := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		closed <- app.CloseRegisteredIngress(ctx, registry)
	}()
	select {
	case err := <-closed:
		t.Fatalf("the App's inputs closed (%v) with a step delivery still in flight; drains=%d: the steps are not one of the App's inputs", err, js.subs[0].drains)
	case <-time.After(50 * time.Millisecond):
	}
	for _, sub := range js.subs {
		close(sub.release)
	}
	if err := <-closed; err != nil {
		t.Fatalf("closing the inputs: %v", err)
	}
	if err := steps.Drain(context.Background()); err != nil {
		t.Fatalf("Drain after the App closed the inputs = %v, want an idempotent nil", err)
	}
	if js.subs[0].drains != 1 {
		t.Fatalf("consumer drained %d times, want 1", js.subs[0].drains)
	}
}
