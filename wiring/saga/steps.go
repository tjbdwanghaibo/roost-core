package saga

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"sync"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// Steps 是一个服务的一组 saga 步骤消费者（参与方）：业务按名字登记步骤体，Start 按 saga Mod 与 DataEngine Mod 的声明组装并
// 订阅，Drain / Stop 统一停止。与业务无关的胶水都在这里：
//
//   - 从 Registry 取 JetStream（命令消费、结果发布、准入信号桶）与 Mongo（Mongo 步骤）；按 saga.* 的声明取分区布局、回执保留期、
//     saga 库与准入参数（ParticipantSettings）；
//   - 每个步骤一个 JetStream durable（`<group>-<name>`，每条 saga 流上同名，过滤全部分区的这个主题），同一服务的全部副本共用；
//     执行后把结果直接发回命令所属分区的 in.res，PubAck 之后才 ack 命令；
//   - 原生步骤的回执在被改的实体里（Begin 查、Complete 写，roost add saga -native 生成回执字段）；Mongo 步骤的回执在
//     saga 库的 `_<group>_step_inbox`（按操作一个文档，TTL = saga.receipt_retention）；
//   - 有原生步骤且准入开启时，每 saga.admission.signal_interval 把本进程 DataEngine 投影积压（WAL 未投影记录数）发到准入信号桶
//     的 part.<group>.<sid>，Drain 时删除（优雅下线不算信号停更）；
//   - 指标：saga.step.run_total{consumer,result}、saga.step.run_duration{consumer}、saga.step.admit_refused_total{consumer}，
//     consumer 是 durable 名。
//
// 配置沿用 saga.* 与 dataengine.*，没有新键。Steps 不是 App Mod：步骤体常要用服务 Init 里才发布的能力（例如玩家驻留表），
// 而 Mod 在服务 Init 之前启动；服务在 Init 里 Start、在 Shutdown 里 Drain。
//
// 按 sid / 分片决定“这条命令归不归本进程”属于业务的拓扑，框架不替业务猜：WithAdmit 在执行之前问业务，拒绝时消息 nak 给别的
// 副本；业务若要把命令直接转交给归属进程，由接收方调用 RunHandedOver。
type Steps struct {
	registry *app.Registry
	group    string

	mu          sync.Mutex
	steps       []*registeredStep
	byName      map[string]*registeredStep
	err         error // 登记期的错误，Start 时一并返回
	started     bool
	participant Participant
	subs        []fnats.IJetStreamSubscription
	draining    bool
	load        *loadSignal
}

// loadSignal 是本组步骤的准入信号发布（只在有原生步骤且准入开启时存在）。
type loadSignal struct {
	publisher *coresaga.LoadPublisher
	cancel    context.CancelFunc
	done      chan struct{}
}

// stop 停止发布并等它退出；remove 时再删掉自己的键（优雅下线），失败只返回错误、键由桶的 TTL 清除。
func (l *loadSignal) stop(ctx context.Context, remove bool) error {
	if l == nil {
		return nil
	}
	l.cancel()
	select {
	case <-l.done:
	case <-ctx.Done():
		return ctx.Err()
	}
	if !remove {
		return nil
	}
	return l.publisher.Remove(ctx)
}

// ErrStepsNotStarted 表示 Steps 还没有 Start，RunHandedOver 无法执行（回执保留期等参数在 Start 时才读）。
var ErrStepsNotStarted = errors.New("saga steps: not started")

// StepOption 调整一个步骤消费者。
type StepOption func(*registeredStep)

// WithAdmit 设置步骤的准入判定（coresaga.StepConsumerConfig.Admit）：在执行之前询问，非 nil 错误时这次投递 nak、按退避
// 重新提供给任意副本。必须廉价、无副作用，且不能处处拒绝（没有进程准入的命令会重投到 MaxDeliver）。
// 不设置表示全部准入（单进程部署）。
func WithAdmit(admit func(context.Context, coresaga.Command) error) StepOption {
	return func(step *registeredStep) { step.admit = admit }
}

type registeredStep struct {
	name    string
	topic   string
	durable string
	native  coresaga.NativeStepBody
	mongo   coresaga.StepHandler
	admit   func(context.Context, coresaga.Command) error
}

var stepNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)

// NewSteps 返回一组名为 group 的步骤消费者（group 用作 durable 前缀、Mongo 收件箱集合名与准入信号的键名，只能含字母、
// 数字、'-'、'_'）。这里只记下 registry，能力在 Start 时查。
func NewSteps(registry *app.Registry, group string) (*Steps, error) {
	if !stepNamePattern.MatchString(group) {
		return nil, fmt.Errorf("saga steps: group %q must be letters, digits, '-' or '_'", group)
	}
	return &Steps{registry: registry, group: group, byName: make(map[string]*registeredStep)}, nil
}

// Native 登记一个原生步骤：业务是一笔 Nest 事务，body 把 coresaga.NativeStep 交给 Nest handler；生成的 handler 包装在业务
// 函数之前调用 step.Begin（查实体里的回执），业务在事务里 step.Complete。topic 是生成的 saga 定义里的步骤主题常量（正向或
// 补偿）。错误在 Start 时返回。
func (s *Steps) Native(name, topic string, body coresaga.NativeStepBody, options ...StepOption) {
	s.add(&registeredStep{name: name, topic: topic, native: body}, body == nil, options)
}

// Mongo 登记一个 Mongo 步骤：handler 在 Mongo 事务里运行并返回 completion（coresaga.StepHandler 的契约）。不在事务里的
// 副作用（调用别的服务）不受“至多一次生效”保护，必须按 Command.IdempotencyKey 幂等。错误在 Start 时返回。
func (s *Steps) Mongo(name, topic string, handler coresaga.StepHandler, options ...StepOption) {
	s.add(&registeredStep{name: name, topic: topic, mongo: handler}, handler == nil, options)
}

func (s *Steps) add(step *registeredStep, missingHandler bool, options []StepOption) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case s.started:
		s.err = errors.Join(s.err, fmt.Errorf("saga steps %s: step %q registered after Start", s.group, step.name))
		return
	case !stepNamePattern.MatchString(step.name):
		s.err = errors.Join(s.err, fmt.Errorf("saga steps %s: step name %q must be letters, digits, '-' or '_'", s.group, step.name))
		return
	case s.byName[step.name] != nil:
		s.err = errors.Join(s.err, fmt.Errorf("saga steps %s: step %q registered twice", s.group, step.name))
		return
	case missingHandler:
		s.err = errors.Join(s.err, fmt.Errorf("saga steps %s: step %q has no handler", s.group, step.name))
		return
	}
	for _, option := range options {
		if option != nil {
			option(step)
		}
	}
	step.durable = s.group + "-" + step.name
	s.steps = append(s.steps, step)
	s.byName[step.name] = step
}

// Start 组装收件箱并按登记顺序订阅全部步骤，有原生步骤且准入开启时启动准入信号。任何一步失败都撤掉已订阅的消费者并返回
// 错误（含登记期错误）。只能成功一次；在服务 Init 里调用，此时 saga 流已由协调器建好（Mod 先于服务 Init 启动）。
func (s *Steps) Start(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	if s.started {
		return fmt.Errorf("saga steps %s: already started", s.group)
	}
	if len(s.steps) == 0 {
		return fmt.Errorf("saga steps %s: no step registered", s.group)
	}
	if s.registry == nil {
		return fmt.Errorf("saga steps %s: nil registry", s.group)
	}
	jetstream, ok := app.Lookup[fnats.IJetStream](s.registry, mods.ModNatsJetStream)
	if !ok || jetstream == nil {
		return fmt.Errorf("saga steps %s: capability %q not found", s.group, mods.ModNatsJetStream)
	}
	participant, err := ParticipantSettings(s.registry.Config())
	if err != nil {
		return fmt.Errorf("saga steps %s: %w", s.group, err)
	}
	var mongoInbox *coresaga.MongoStepInbox
	hasNative := false
	for _, step := range s.steps {
		switch {
		case step.native != nil:
			hasNative = true
		case mongoInbox == nil:
			mongoClient, ok := app.Lookup[fmongo.IMongo](s.registry, mods.ModMongo)
			if !ok || mongoClient == nil {
				return fmt.Errorf("saga steps %s: capability %q not found (Mongo steps keep their receipts in %s)", s.group, mods.ModMongo, participant.Database)
			}
			mongoInbox, err = coresaga.NewMongoStepInbox(mongoClient, participant.Database, "_"+s.group+"_step_inbox", participant.ReceiptRetention)
			if err != nil {
				return fmt.Errorf("saga steps %s: %w", s.group, err)
			}
		}
	}
	if mongoClient, ok := app.Lookup[fmongo.IMongo](s.registry, mods.ModMongo); ok && mongoClient != nil {
		probeTransactionLifetime(ctx, mongoClient, s.group, participant.StepWindow)
	}
	var load *loadSignal
	if hasNative && participant.Admission.Enabled {
		if load, err = s.startLoadSignal(ctx, participant); err != nil {
			return err
		}
	}
	subs := make([]fnats.IJetStreamSubscription, 0, len(s.steps)*participant.Layout.Groups)
	for _, step := range s.steps {
		config := coresaga.StepConsumerConfig{Durable: step.durable, Topic: step.topic, Admit: step.countedAdmit(), ReceiptRetention: participant.ReceiptRetention}
		var stepSubs []fnats.IJetStreamSubscription
		if step.native != nil {
			stepSubs, err = coresaga.SubscribeNativeStep(ctx, jetstream, participant.Layout, config, step.countedNative())
		} else {
			stepSubs, err = coresaga.SubscribeMongoStep(ctx, jetstream, participant.Layout, mongoInbox, config, step.countedMongo())
		}
		if err != nil {
			for _, started := range subs {
				started.Stop()
			}
			_ = load.stop(context.WithoutCancel(ctx), true)
			return fmt.Errorf("saga steps %s: subscribe %s: %w", s.group, step.name, err)
		}
		subs = append(subs, stepSubs...)
	}
	s.participant, s.subs, s.load, s.started = participant, subs, load, true
	return nil
}

// transactionLifetimeParameter 是 Mongo 服务端回收打开事务的时限参数（秒）。
const transactionLifetimeParameter = "transactionLifetimeLimitSeconds"

// probeTransactionLifetime 是参与方启动时的只读探针（设计 T-A 第 2 条）：被 kill -9 的进程留下的事务（DataEngine 投影、
// Mongo 步骤）持锁到服务端的 transactionLifetimeLimitSeconds 才回收，重启后的排空与同一操作的下一次尝试这段时间一直
// 写冲突；它长于缺省步骤预算窗口时，在途步骤可能在这段时间里用完全部尝试、进入 manual_required。部署要求是 20s
// （SAGA.md「进程被强杀时遗留的 Mongo 事务」）。超过窗口时 WARN 一次并计 saga.step.mongo_txn_lifetime_exceeds_budget；
// 读不到（托管 Mongo 拒绝 getParameter、账号无权限）只记一条“未探测”。参数归部署，探针从不拒绝启动。返回读到的时限
// （读不到为 0）与是否超过窗口，测试用。
func probeTransactionLifetime(ctx context.Context, client fmongo.IMongo, group string, window time.Duration) (time.Duration, bool) {
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	value, err := client.GetParameter(probeCtx, transactionLifetimeParameter)
	var seconds float64
	if err == nil {
		switch v := value.(type) {
		case int32:
			seconds = float64(v)
		case int64:
			seconds = float64(v)
		case float64:
			seconds = v
		case int:
			seconds = float64(v)
		default:
			err = fmt.Errorf("unexpected type %T", value)
		}
	}
	if err != nil {
		slog.Info("saga steps: Mongo transactionLifetimeLimitSeconds not probed; the deployment requirement is 20s (SAGA.md)", "group", group, "err", err)
		return 0, false
	}
	lifetime := time.Duration(seconds * float64(time.Second))
	if lifetime <= window {
		return lifetime, false
	}
	slog.Warn("saga steps: Mongo transactionLifetimeLimitSeconds exceeds the default step budget window; a killed participant's leftover transactions can outlast every attempt of an in-flight step (deployment requirement: 20s, SAGA.md)",
		"group", group, "transaction_lifetime", lifetime, "step_window", window)
	metrics.IncCounter("saga.step.mongo_txn_lifetime_exceeds_budget", map[string]string{"group": group}, 1)
	return lifetime, true
}

// startLoadSignal 启动本进程的准入信号：读同进程 DataEngine 投影器的 WAL 未投影记录数。原生步骤一定在有 DataEngine 的进程里
// （Nest 事务的持久化），没有时是装配错误。
func (s *Steps) startLoadSignal(ctx context.Context, participant Participant) (*loadSignal, error) {
	dataEngine, ok := app.Lookup[*kitdataengine.Mod](s.registry, mods.ModDataEngine)
	if !ok || dataEngine == nil {
		return nil, fmt.Errorf("saga steps %s: native steps need the %q Mod in this process (admission reads its projection backlog)", s.group, mods.ModDataEngine)
	}
	log, ok := app.Lookup[fnats.JetStreamLog](s.registry, mods.ModNatsLog)
	if !ok || log == nil {
		return nil, fmt.Errorf("saga steps %s: capability %q not found", s.group, mods.ModNatsLog)
	}
	kv, err := coresaga.OpenLoadBucket(ctx, log, participant.Layout, participant.Replicas, participant.Admission)
	if err != nil {
		return nil, fmt.Errorf("saga steps %s: admission bucket: %w", s.group, err)
	}
	backlog := func() uint64 {
		runtime := dataEngine.Runtime()
		if runtime == nil || runtime.Projector == nil {
			return 0
		}
		return runtime.Projector.Stats().WALUnacked
	}
	publisher, err := coresaga.NewLoadPublisher(kv, fmt.Sprintf("%s.%d", s.group, participant.Sid), participant.SignalInterval, backlog)
	if err != nil {
		return nil, fmt.Errorf("saga steps %s: %w", s.group, err)
	}
	runCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		publisher.Run(runCtx)
	}()
	return &loadSignal{publisher: publisher, cancel: cancel, done: done}, nil
}

// Drain 停止接收新命令并在 ctx 内等全部消费者处理完在途投递（幂等），然后停止准入信号并删掉自己的键。超时返回 ctx 的错误并
// 保留消费者：用新的 ctx 再调用会接着等，Stop 强制停止。在服务 Shutdown 里调用；未 Start 时直接返回 nil。
func (s *Steps) Drain(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	subs, load := s.subs, s.load
	if !s.draining {
		s.draining = true
		for _, sub := range subs {
			sub.Drain()
		}
	}
	s.mu.Unlock()
	for _, sub := range subs {
		select {
		case <-sub.Closed():
		case <-ctx.Done():
			return fmt.Errorf("saga steps %s: drain: %w", s.group, ctx.Err())
		}
	}
	if err := load.stop(ctx, true); err != nil {
		return fmt.Errorf("saga steps %s: remove admission signal: %w", s.group, err)
	}
	return nil
}

// Stop 立即停止全部消费者（取消在途 handler 的 ctx）与准入信号；Drain 超时后的兜底。在途投递未 ack 的由 JetStream 在
// AckWait 后重投；信号键由桶的 TTL 清除。
func (s *Steps) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	subs, load := s.subs, s.load
	s.mu.Unlock()
	for _, sub := range subs {
		sub.Stop()
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_ = load.stop(ctx, false)
}

// RunHandedOver 在本进程执行一条被转交过来的原生步骤命令（另一个进程的 WithAdmit 判定“不归它”后经业务自己的通道
// 送来；按 sid / 分片转交是业务的拓扑）。检查依次是：
//
//  1. name 是登记过的原生步骤，命令的主题就是该步骤订阅的主题——转交信封说是 A、命令其实是 B（例如扣款信封包着退款
//     命令）会在执行任何东西之前被拒绝，返回错误；
//  2. 命令合法、未过截止（过了返回 nil，协调器按超时处理）；
//  3. accept 复核归属（不信任发送方的结论）：false 表示不在本进程执行，安静返回 nil，转交方的重投兜底；nil 表示全部接受；
//  4. 运行步骤体（coresaga.RunNativeStep）：与消费者的互斥由实体锁与实体里的回执保证——谁先提交谁生效，另一条读到回执回放。
//     这里不发布结果、不 ack：原消息仍在转交方手里，它的重投读到回执后把结果发回协调器；转交丢失只多耗时间，不丢步骤。
//
// 未 Start 时第 4 步返回 ErrStepsNotStarted。
func (s *Steps) RunHandedOver(ctx context.Context, name string, command coresaga.Command, accept func(context.Context, coresaga.Command) bool) error {
	if s == nil {
		return ErrStepsNotStarted
	}
	s.mu.Lock()
	step, started, retention := s.byName[name], s.started, s.participant.ReceiptRetention
	s.mu.Unlock()
	switch {
	case step == nil:
		return fmt.Errorf("saga steps %s: handed-over step %q is not registered", s.group, name)
	case step.native == nil:
		return fmt.Errorf("saga steps %s: handed-over step %q is not a native step", s.group, name)
	case command.Topic != step.topic:
		return fmt.Errorf("saga steps %s: handed-over step %q carries a command published on %q, want %q", s.group, name, command.Topic, step.topic)
	}
	if err := command.Validate(); err != nil {
		return fmt.Errorf("saga steps %s: handed-over command: %w", s.group, err)
	}
	if !time.Now().Before(command.DeadlineAt) {
		return nil
	}
	if accept != nil && !accept(ctx, command) {
		return nil
	}
	if !started {
		return ErrStepsNotStarted
	}
	if ctx == nil {
		ctx = context.Background()
	}
	processCtx, cancel := context.WithDeadline(ctx, command.DeadlineAt)
	defer cancel()
	_, err := coresaga.RunNativeStep(processCtx, command, retention, step.countedNative())
	return err
}

// countedAdmit 记下准入拒绝（转交与回压在日志之外唯一可见的地方）。
func (step *registeredStep) countedAdmit() func(context.Context, coresaga.Command) error {
	if step.admit == nil {
		return nil
	}
	admit, labels := step.admit, metrics.Labels{"consumer": step.durable}
	return func(ctx context.Context, command coresaga.Command) error {
		err := admit(ctx, command)
		if err != nil {
			metrics.IncCounter("saga.step.admit_refused_total", labels, 1)
		}
		return err
	}
}

// countedNative 计原生步骤体的执行：result=ok 表示 Nest 调用返回成功（业务拒绝、回放也是 ok，结论在回执里），
// error 表示结果未知、nak 后重投回放。
func (step *registeredStep) countedNative() coresaga.NativeStepBody {
	body, durable := step.native, step.durable
	return func(ctx context.Context, native coresaga.NativeStep) error {
		started := time.Now()
		err := body(ctx, native)
		observeStepRun(durable, started, stepResult(err, true))
		return err
	}
}

// countedMongo 计 Mongo 步骤 handler 的执行：ok / refused（completion 失败）/ error。Mongo 事务重试会重复调用 handler，
// 这里按调用计。
func (step *registeredStep) countedMongo() coresaga.StepHandler {
	handler, durable := step.mongo, step.durable
	return func(ctx context.Context, command coresaga.Command) (coresaga.Completion, error) {
		started := time.Now()
		completion, err := handler(ctx, command)
		observeStepRun(durable, started, stepResult(err, err != nil || completion.Success))
		return completion, err
	}
}

func stepResult(err error, success bool) string {
	switch {
	case err != nil:
		return "error"
	case !success:
		return "refused"
	default:
		return "ok"
	}
}

func observeStepRun(durable string, started time.Time, result string) {
	metrics.IncCounter("saga.step.run_total", metrics.Labels{"consumer": durable, "result": result}, 1)
	metrics.ObserveHistogram("saga.step.run_duration", metrics.Labels{"consumer": durable}, time.Since(started))
}
