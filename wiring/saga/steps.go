package saga

import (
	"context"
	"errors"
	"fmt"
	"os"
	"regexp"
	"strconv"
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

// Steps 是一个服务的一组 saga 步骤消费者（DESIGN-TODO D12 方向 A）：业务按名字登记步骤体，Start 按 saga Mod 与
// DataEngine Mod 的声明组装并订阅，Drain / Stop 统一停止。与业务无关的胶水都在这里：
//
//   - 从 Registry 取 JetStream 与 Mongo；按 saga.* 的声明取命令主题前缀、saga 流与 saga 库，按 dataengine.* 取原生收件箱
//     所在的库（dataengine.database：回执是那个库上的 lease fence，与业务修改同一次投影）；
//   - 建 completion 发布器、Mongo 步骤收件箱（saga 库的 `_<group>_step_inbox`）与原生步骤收件箱（租约
//     NativeLeaseDuration，长于步骤消费者的 AckWait）；租约持有者名是 `<group>-<主机名>-<pid>`，两个副本不会认领同一条命令；
//   - 每个步骤一个 JetStream durable（`<group>-<name>`），同一服务的全部副本共用；
//   - 指标：saga.step.run_total{consumer,result}、saga.step.run_duration{consumer}、saga.step.admit_refused_total{consumer}，
//     consumer 是 durable 名。
//
// 配置沿用 saga.* 与 dataengine.*，没有新键。Steps 不是 App Mod：步骤体常要用服务 Init 里才发布的能力（例如玩家驻留表），
// 而 Mod 在服务 Init 之前启动；服务在 Init 里 Start、在 Shutdown 里 Drain。
//
// 按 sid / 分片决定“这条命令归不归本进程”属于业务的拓扑，框架不替业务猜：WithAdmit 在取得执行权之前问业务，拒绝时消息
// nak 给别的副本；业务若要把命令直接转交给归属进程，由接收方调用 RunHandedOver。
type Steps struct {
	registry *app.Registry
	group    string

	mu       sync.Mutex
	steps    []*registeredStep
	byName   map[string]*registeredStep
	err      error // 登记期的错误，Start 时一并返回
	started  bool
	native   *coresaga.DataEngineStepInbox
	subs     []fnats.IJetStreamSubscription
	draining bool
}

// NativeLeaseDuration 是原生步骤收件箱的租约上限。必须长于步骤消费者的 AckWait（coresaga.DefaultStepAckWait，30s）：
// 否则消息还没 ack 租约就先过期，另一个进程可以开始同一条命令（SubscribeDataEngineStep 拒绝这样的配置）。实际租约再
// 封顶到命令截止。
const NativeLeaseDuration = 2 * time.Minute

// ErrStepsNotStarted 表示 Steps 还没有 Start（收件箱尚未建立），RunHandedOver 无法取执行权。
var ErrStepsNotStarted = errors.New("saga steps: not started")

// StepOption 调整一个步骤消费者。
type StepOption func(*registeredStep)

// WithAdmit 设置步骤的准入判定（coresaga.StepConsumerConfig.Admit）：在收件箱为命令取得执行权之前询问，非 nil 错误时这次
// 投递 nak、按退避重新提供给任意副本。必须廉价、无副作用，且不能处处拒绝（没有进程准入的命令会重投到 MaxDeliver）。
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

// NewSteps 返回一组名为 group 的步骤消费者（group 用作 durable 前缀、收件箱集合名与租约持有者名，只能含字母、数字、
// '-'、'_'）。这里只记下 registry，能力在 Start 时查。
func NewSteps(registry *app.Registry, group string) (*Steps, error) {
	if !stepNamePattern.MatchString(group) {
		return nil, fmt.Errorf("saga steps: group %q must be letters, digits, '-' or '_'", group)
	}
	return &Steps{registry: registry, group: group, byName: make(map[string]*registeredStep)}, nil
}

// Native 登记一个原生步骤：业务是一笔 Nest 事务，body 把 coresaga.NativeStep 交给 Nest handler，handler 在事务里
// step.Complete。topic 是生成的 saga 定义里的步骤主题常量（正向或补偿）。错误在 Start 时返回。
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

// Start 组装收件箱与发布器并按登记顺序订阅全部步骤。任何一步失败都撤掉已订阅的消费者并返回错误（含登记期错误）。
// 只能成功一次；在服务 Init 里调用，此时 saga Mod 已启动（saga 流已存在）。
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
	mongoClient, ok := app.Lookup[fmongo.IMongo](s.registry, mods.ModMongo)
	if !ok || mongoClient == nil {
		return fmt.Errorf("saga steps %s: capability %q not found", s.group, mods.ModMongo)
	}
	// 与协调器同一份声明：命令在同一条流、同一个前缀上，completion 也经它发回。
	prefix, stream, sagaDatabase, err := StreamSettings(s.registry.Config())
	if err != nil {
		return fmt.Errorf("saga steps %s: %w", s.group, err)
	}
	transport, err := coresaga.NewJetStreamPublisher(jetstream, prefix)
	if err != nil {
		return fmt.Errorf("saga steps %s: %w", s.group, err)
	}
	owner := s.group + "-" + processOwnerSuffix()
	var mongoInbox *coresaga.MongoCommandInbox
	var native *coresaga.DataEngineStepInbox
	for _, step := range s.steps {
		switch {
		case step.mongo != nil && mongoInbox == nil:
			mongoInbox, err = coresaga.NewMongoCommandInbox(mongoClient, sagaDatabase, "_"+s.group+"_step_inbox", coresaga.CommandInboxOptions{Owner: owner})
		case step.native != nil && native == nil:
			// 原生收件箱在 Data Engine 的库里：它绑定的回执是那个库上的 lease fence，由带着业务修改的同一次提交投影。
			var dataEngineDatabase string
			dataEngineDatabase, _, _, err = kitdataengine.EffectSettings(s.registry.Config())
			if err == nil {
				native, err = coresaga.NewDataEngineStepInbox(mongoClient, dataEngineDatabase, coresaga.DataEngineStepInboxOptions{Owner: owner, LeaseDuration: NativeLeaseDuration})
			}
		}
		if err != nil {
			return fmt.Errorf("saga steps %s: %w", s.group, err)
		}
	}
	subs := make([]fnats.IJetStreamSubscription, 0, len(s.steps))
	for _, step := range s.steps {
		config := coresaga.StepConsumerConfig{Stream: stream, Durable: step.durable, Topic: step.topic, Admit: step.countedAdmit()}
		var sub fnats.IJetStreamSubscription
		if step.native != nil {
			sub, err = coresaga.SubscribeDataEngineStep(ctx, jetstream, transport, native, config, native.StepHandler(step.countedNative()))
		} else {
			sub, err = coresaga.SubscribeMongoStep(ctx, jetstream, transport, mongoInbox, config, step.countedMongo())
		}
		if err != nil {
			for _, started := range subs {
				started.Stop()
			}
			return fmt.Errorf("saga steps %s: subscribe %s: %w", s.group, step.name, err)
		}
		subs = append(subs, sub)
	}
	s.native, s.subs, s.started = native, subs, true
	return nil
}

// Drain 停止接收新命令并在 ctx 内等全部消费者处理完在途投递（幂等）。超时返回 ctx 的错误并保留消费者：用新的 ctx
// 再调用会接着等，Stop 强制停止。在服务 Shutdown 里调用；未 Start 时直接返回 nil。
func (s *Steps) Drain(ctx context.Context) error {
	if s == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	subs := s.subs
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
	return nil
}

// Stop 立即停止全部消费者（取消在途 handler 的 ctx）；Drain 超时后的兜底。在途投递未 ack 的由 JetStream 在 AckWait 后重投。
func (s *Steps) Stop() {
	if s == nil {
		return
	}
	s.mu.Lock()
	subs := s.subs
	s.mu.Unlock()
	for _, sub := range subs {
		sub.Stop()
	}
}

// RunHandedOver 在本进程执行一条被转交过来的原生步骤命令（另一个进程的 WithAdmit 判定“不归它”后经业务自己的通道
// 送来；按 sid / 分片转交是业务的拓扑）。检查依次是：
//
//  1. name 是登记过的原生步骤，命令的主题就是该步骤订阅的主题——转交信封说是 A、命令其实是 B（例如扣款信封包着退款
//     命令）会在记录或认领任何东西之前被拒绝，返回错误；
//  2. 命令合法、未过截止（过了返回 nil，协调器按超时处理）；
//  3. accept 复核归属（不信任发送方的结论）：false 表示不在本进程执行，安静返回 nil，转交方的重投兜底；nil 表示全部接受；
//  4. 取执行权并运行步骤体，语义见 coresaga.DataEngineStepInbox.RunHandedOver（与消费者互斥、不等投影、已有结果或
//     别人持有执行权时返回 nil）。
//
// 未 Start 时第 4 步返回 ErrStepsNotStarted。
func (s *Steps) RunHandedOver(ctx context.Context, name string, command coresaga.Command, accept func(context.Context, coresaga.Command) bool) error {
	if s == nil {
		return ErrStepsNotStarted
	}
	s.mu.Lock()
	step, native, started := s.byName[name], s.native, s.started
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
	return native.RunHandedOver(ctx, command, step.countedNative())
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

// countedNative 计原生步骤体的执行：result=ok 表示 Nest 调用返回成功（业务拒绝也是 ok，结论在 completion 里），
// error 表示结果未知、等回执或下一次尝试。
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

// processOwnerSuffix 区分本进程与别的副本的认领：主机名 + pid。认领在步骤结束或租约到期时释放，进程内稳定即可。
func processOwnerSuffix() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = "unknown"
	}
	return host + "-" + strconv.Itoa(os.Getpid())
}
