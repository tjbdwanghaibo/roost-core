package saga

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// saga.* 的声明（维护者决定）。缺省值与 coresaga.DefaultOptions 相同（TestSagaDeclaredDefaultsMatchCoreDefaults），
// 写 0 或负数拒绝；要缺省就不写这个键。协调器的权威是 JetStream 分区事件日志（设计
// docs/maintenance/DESIGN-2026-10-28-saga-jetstream-authority.md §9）。

// stepBudgetConfig 是一个步骤的超时与重试预算：saga.step_defaults 与 saga.steps.<type>.<step> 共用。
// 没写的字段不覆盖（定义里的值、step_defaults、框架缺省依次生效）；段下没有声明的字段报错。
type stepBudgetConfig struct {
	Timeout     time.Duration `config:"timeout" min:"1ns" example:"5s"`
	MaxAttempts int           `config:"max_attempts" min:"1" max:"1000" example:"5"`
	BackoffMin  time.Duration `config:"backoff_min" min:"1ns" example:"100ms"`
	BackoffMax  time.Duration `config:"backoff_max" min:"1ns" example:"5s"`
}

// consumerConfig 是一个 JetStream durable consumer 的投递参数。
type consumerConfig struct {
	AckWait        time.Duration `config:"ack_wait" default:"30s" min:"1ns" example:"30s"`
	ProcessTimeout time.Duration `config:"process_timeout" default:"3s" min:"1ns" example:"3s"`
	MaxDeliver     int           `config:"max_deliver" default:"25000" min:"1" example:"25000"`
	MaxAckPending  int           `config:"max_ack_pending" default:"256" min:"1" example:"256"`
	NakBackoffMin  time.Duration `config:"nak_backoff_min" default:"250ms" min:"1ns" example:"250ms"`
	NakBackoffMax  time.Duration `config:"nak_backoff_max" default:"30s" min:"1ns" example:"30s"`
}

// productionReplicas 是生产要求的流与桶副本数（前提：JetStream R3 文件存储，收到 PubAck 即不丢）。
const productionReplicas = 3

type config struct {
	app.ServiceIdentity
	// saga.max_payload_bytes 与发起方（wiring/nest）共用一份声明。
	mods.SagaPayloadConfig
	// singleton.enabled 与 App 共用一份声明：协调器进程在生产必须启用单实例锁（ValidateConfig）。
	Singleton app.SingletonSwitch `config:"singleton"`
	Saga      struct {
		Database     string           `config:"database" shared:"saga.database" default:"saga" example:"{project_db}_saga" help:"Mongo 步骤收件箱（按操作的回执）与冷副本（saga.cold_copy）所在的库"`
		StepDefaults stepBudgetConfig `config:"step_defaults,closed" help:"Step timeout and retry budget. Fields a saga definition leaves unset\ntake these; one operation of a step gets up to max_attempts attempts, at most\none of which takes effect."`
		// Steps 按 <saga type>.<step name> 覆盖，优先于定义与 step_defaults；类型与步骤名要对应已注册的定义。
		Steps         map[string]map[string]stepBudgetConfig `config:"steps" example:"{}" help:"Per-step overrides, winning over the definition and step_defaults:\nsteps.<saga type>.<step name>.<timeout|max_attempts|backoff_min|backoff_max>."`
		SubjectPrefix string                                 `config:"subject_prefix" shared:"saga.subject_prefix" default:"roost.saga" example:"{project}.saga" help:"分区事件、输入与查询的 subject 前缀"`
		Stream        string                                 `config:"stream" shared:"saga.stream" default:"ROOST_SAGA" example:"{PROJECT}_SAGA" help:"流名前缀：第 g 条流是 <stream>_G<g>，快照与准入桶由它派生"`
		Partitions    int                                    `config:"partitions" default:"16" min:"1" max:"1024" example:"16" help:"分区数（全部进程相同，不同时协调器启动报 ErrLayoutMismatch）。改值要先让全部 saga 终态、停全部协调器、删流与桶"`
		StreamGroups  int                                    `config:"stream_groups" default:"4" min:"1" max:"8" example:"4" help:"流数（≤ partitions，每条流的分区数 ceil(partitions/stream_groups) ≤ 50）；一条流选主时只停它上面的分区"`
		Coordinators  []string                               `config:"coordinators" required:"true" example:"1000" help:"跑协调器的 sid 列表（全部进程相同）：排序后第 i 个 sid 拥有 k mod len == i 的分区；不在列表里的进程只做发起方。\n部署规则：一个 sid 只能在一种服务类型里装 saga Mod（单实例锁按服务类型 + sid，挡不住两种类型的同号进程，\n它们会认领同一组分区、被期望序号隔离并 fail-stop；配置检查看不到别的服务类型，靠部署约定，见 SAGA.md）"`
		Replicas      int                                    `config:"replicas" default:"1" min:"1" max:"5" example:"1" help:"流与桶的副本数；生产必须为 3"`
		// 去重窗口要盖住一次发布结果未知的整个重发判定（publish_unknown_budget）两遍。
		DuplicateWindow     time.Duration `config:"duplicate_window" default:"2m" min:"1s" example:"2m" help:"MsgID 去重窗口，≥ 2 × publish_unknown_budget"`
		StreamMaxBytes      int64         `config:"stream_max_bytes" default:"8589934592" min:"1" example:"8589934592" help:"每条流的容量上限；满了拒新写（协调器 fail-stop 告警），只由快照后的清理删旧消息"`
		MaxBatch            int           `config:"max_batch" default:"256" min:"1" max:"1000" example:"256" help:"分区一轮最多取的输入条数"`
		Tick                time.Duration `config:"tick" default:"5ms" min:"1ms" max:"100ms" example:"5ms" help:"分区时间轮的槽宽"`
		SnapshotEveryEvents int           `config:"snapshot_every_events" default:"2000" min:"1" example:"2000"`
		SnapshotInterval    time.Duration `config:"snapshot_interval" default:"30s" min:"1s" example:"30s"`
		TerminalRetention   time.Duration `config:"terminal_retention" default:"1h" min:"1m" example:"1h" help:"终态在内存与快照里留多久：期间 Get / Lookup 可读、同业务键去重、迟到成功可补偿；≤ receipt_retention"`
		ReceiptRetention    time.Duration `config:"receipt_retention" default:"168h" min:"1m" example:"168h" help:"参与方回执保留期：至少是每个步骤重试窗口（超时 × 次数 + 退避上限 × (次数 − 1)）的 2 倍，配置检查按定义强制；更早更新的记录拒绝 Resume"`
		PublishTimeout      time.Duration `config:"publish_timeout" default:"3s" min:"1ns" max:"9s" example:"3s" help:"一次分区事件发布等 PubAck 的时长，< publish_unknown_budget"`
		// 下限 21s：一条流选主停顿实测 5～7s，结果未知的重发判定要撑过 3 次（声明的 min 是唯一的强制点）。
		PublishUnknownBudget time.Duration `config:"publish_unknown_budget" default:"30s" min:"21s" example:"30s" help:"分区事件发布结果未知时重发判定的总时长（≥ 3 × 7s 选主停顿），超过 fail-stop"`
		QueryTimeout         time.Duration `config:"query_timeout" default:"2s" min:"1ns" example:"2s"`
		StartVerdictTimeout  time.Duration `config:"start_verdict_timeout" default:"2s" min:"1ns" example:"2s" help:"StartSaga / Resume / Compensate 等所有者判定回复多久后改为按 Get 补查"`
		PurgeStallAlarm      time.Duration `config:"purge_stall_alarm" default:"10m" min:"1s" example:"10m" help:"某个步骤 durable 挡住清理多久告警（下线的步骤要由运维删掉它的 durable）"`
		Admission            struct {
			Enabled        bool          `config:"enabled" default:"true" example:"true" help:"按参与方投影积压准入新启动；在途推进不受影响"`
			HighRecords    int64         `config:"high_records" default:"5000" min:"1" example:"5000" help:"积压（参与方 WAL 未投影记录数的最大值）≥ 它时关闭准入"`
			LowRecords     int64         `config:"low_records" default:"2500" min:"0" example:"2500" help:"关闭后积压 ≤ 它才重开；< high_records"`
			SignalInterval time.Duration `config:"signal_interval" default:"200ms" min:"1ms" example:"200ms" help:"参与方发布积压信号的间隔"`
			StaleAfter     time.Duration `config:"stale_after" default:"2s" min:"1ms" example:"2s" help:"一个参与方的信号多久没更新算停更（关闭准入）；≥ 5 × signal_interval"`
		} `config:"admission"`
		// 冷副本（设计 §8，缺省关闭）：协调器进程把分区事件里的 saga 记录投影进 <saga.database>.<collection>，供
		// Get / GetByBusinessKey / List 在所有者内存里没有这条记录时回落读取；不参与唯一性与任何判定。
		ColdCopy struct {
			Enabled      bool          `config:"enabled" default:"false" example:"false" help:"开启 Mongo 冷副本：终态过了 terminal_retention 之后 Get / Lookup / List 仍能从 <saga.database>.<collection> 读到（只读、最终一致，不参与唯一性）。\n需要本进程有 Mongo；关闭后要由运维删掉各流上的 <stream>_G<g>-cold-copy durable，否则它们挡住分区清理"`
			TerminalOnly bool          `config:"terminal_only" default:"true" example:"true" help:"只投影终态事件（每笔一次写）；false 时投影全部事件（每次推进一次写）"`
			Collection   string        `config:"collection" default:"sagas" example:"sagas" help:"冷副本集合名（库是 saga.database）"`
			BatchSize    int           `config:"batch_size" default:"256" min:"1" max:"1000" example:"256" help:"一次 BulkWrite 最多的事件数"`
			MaxWait      time.Duration `config:"max_wait" default:"50ms" min:"1ms" max:"10s" example:"50ms" help:"一批从第一条事件起最多等多久"`
		} `config:"cold_copy"`
		// Nest 事务里提交的启动意图在 DataEngine 的效果流上：流名与主题前缀不写时取 dataengine.effects 的值，效果流只在一处
		// 配置。同一进程注册了 DataEngine 时，写了就必须与 dataengine.effects 相同（CheckConfigAcrossMods）。
		StartEffectStream  string         `config:"start_effect_stream" shared:"dataengine.effects.stream" follows:"dataengine.effects.stream" help:"不写取 dataengine.effects.stream；同进程有 DataEngine 时写了必须与它相同"`
		StartEffectPrefix  string         `config:"start_effect_prefix" shared:"dataengine.effects.subject_prefix" follows:"dataengine.effects.subject_prefix" help:"不写取 dataengine.effects.subject_prefix；同进程有 DataEngine 时写了必须与它相同"`
		StartEffectDurable string         `config:"start_effect_durable" default:"roost-saga-start" example:"roost-saga-start"`
		StartEffect        consumerConfig `config:"start_effect_"`
		StartEffectWorkers int            `config:"start_effect_workers" default:"4" min:"1" example:"4" help:"启动中继（Nest 启动意图 → 分区）的并发处理数，不能超过 start_effect_max_ack_pending"`
	} `config:"saga"`
}

// layout 是声明里的分区布局（Layout.Validate 由 ValidateConfig 调用：每条流 ≤ 50 个分区等）。
func (c *config) layout() coresaga.Layout {
	return coresaga.Layout{Prefix: strings.Trim(strings.TrimSpace(c.Saga.SubjectPrefix), "."), Stream: strings.TrimSpace(c.Saga.Stream),
		Partitions: c.Saga.Partitions, Groups: c.Saga.StreamGroups}
}

// coordinators 解析 saga.coordinators（sid 列表，去重前不排序）。
func (c *config) coordinators() ([]int32, error) {
	sids := make([]int32, 0, len(c.Saga.Coordinators))
	for _, text := range c.Saga.Coordinators {
		sid, err := strconv.ParseInt(strings.TrimSpace(text), 10, 32)
		if err != nil || sid <= 0 {
			return nil, fmt.Errorf("saga.coordinators: %q is not a positive sid", text)
		}
		if slices.Contains(sids, int32(sid)) {
			return nil, fmt.Errorf("saga.coordinators: sid %d is listed twice", sid)
		}
		sids = append(sids, int32(sid))
	}
	if len(sids) == 0 {
		return nil, errors.New("saga.coordinators must list at least one sid")
	}
	return sids, nil
}

// ownedPartitions 是选项 P-A 的静态分配：saga.coordinators 排序后本进程 sid 的下标 i，拥有 {k : k mod len == i}。
// 本进程不在列表里时返回 nil（只做发起方）。
func ownedPartitions(coordinators []int32, sid int32, partitions int) []int {
	sorted := slices.Sorted(slices.Values(coordinators))
	index := slices.Index(sorted, sid)
	if index < 0 {
		return nil
	}
	var owned []int
	for k := index; k < partitions; k += len(sorted) {
		owned = append(owned, k)
	}
	return owned
}

// isCoordinator 报告本进程（sid）是否在 saga.coordinators 里。
func (c *config) isCoordinator() bool {
	sids, err := c.coordinators()
	return err == nil && slices.Contains(sids, c.Sid)
}

// ValidateConfig 是 saga.* 的跨键规则（设计 §9.2）：
//
//   - 布局：Layout.Validate（前缀、流名、P / G 范围、每条流 ≤ 50 个分区）；saga.coordinators 是不重复的正整数 sid；
//   - 发布：publish_timeout < publish_unknown_budget（声明已要求预算 ≥ 3 × 7s、超时 < 10s），duplicate_window ≥ 2 × 预算；
//   - 保留：terminal_retention ≤ receipt_retention；receipt_retention 与每个步骤重试窗口的关系要用到定义，在
//     checkStepBudgets 里查；
//   - 准入：low_records < high_records，stale_after ≥ 5 × signal_interval；
//   - 启动中继的并发不超过 max_ack_pending；配置里写的步骤超时必须短于步骤消费者的 AckWait（理由见 stepTimeoutError）；
//   - 生产：replicas 必须为 3；本进程在 saga.coordinators 里时必须启用单实例锁（singleton.enabled，App 级“一个 sid 一个
//     进程”的保证；崩溃重启的短暂重叠由期望序号隔离、旧进程 fail-stop）。
func (c *config) ValidateConfig(production bool) error {
	var errs []error
	if err := c.layout().Validate(); err != nil {
		errs = append(errs, fmt.Errorf("saga.subject_prefix / stream / partitions / stream_groups: %w", err))
	}
	if _, err := c.coordinators(); err != nil {
		errs = append(errs, err)
	}
	s := &c.Saga
	if s.PublishTimeout >= s.PublishUnknownBudget {
		errs = append(errs, fmt.Errorf("saga.publish_timeout (%s) must be shorter than saga.publish_unknown_budget (%s)", s.PublishTimeout, s.PublishUnknownBudget))
	}
	if s.DuplicateWindow < 2*s.PublishUnknownBudget {
		errs = append(errs, fmt.Errorf("saga.duplicate_window (%s) must be at least 2 × saga.publish_unknown_budget (%s): a resent event must still be deduplicated", s.DuplicateWindow, s.PublishUnknownBudget))
	}
	if s.TerminalRetention > s.ReceiptRetention {
		errs = append(errs, fmt.Errorf("saga.terminal_retention (%s) must not exceed saga.receipt_retention (%s)", s.TerminalRetention, s.ReceiptRetention))
	}
	if s.Admission.LowRecords >= s.Admission.HighRecords {
		errs = append(errs, fmt.Errorf("saga.admission.low_records (%d) must be below saga.admission.high_records (%d)", s.Admission.LowRecords, s.Admission.HighRecords))
	}
	if s.Admission.StaleAfter < 5*s.Admission.SignalInterval {
		errs = append(errs, fmt.Errorf("saga.admission.stale_after (%s) must be at least 5 × saga.admission.signal_interval (%s)", s.Admission.StaleAfter, s.Admission.SignalInterval))
	}
	if s.StartEffectWorkers > s.StartEffect.MaxAckPending {
		errs = append(errs, fmt.Errorf("saga.start_effect_workers (%d) must not exceed the consumer's max_ack_pending (%d)", s.StartEffectWorkers, s.StartEffect.MaxAckPending))
	}
	if production && s.Replicas != productionReplicas {
		errs = append(errs, fmt.Errorf("saga.replicas (%d) must be %d in production: the saga log is the coordinator's authority and relies on JetStream R3", s.Replicas, productionReplicas))
	}
	if production && c.isCoordinator() && !c.Singleton.Enabled {
		errs = append(errs, fmt.Errorf("sid %d is in saga.coordinators, so production requires singleton.enabled=true: the process owning a saga partition must be the only one for its sid", c.Sid))
	}
	if timeout := s.StepDefaults.Timeout; timeout >= coresaga.DefaultStepAckWait {
		errs = append(errs, stepTimeoutError("saga.step_defaults.timeout", timeout))
	}
	typeNames := make([]string, 0, len(s.Steps))
	for name := range s.Steps {
		typeNames = append(typeNames, name)
	}
	sort.Strings(typeNames)
	for _, typeName := range typeNames {
		stepNames := make([]string, 0, len(s.Steps[typeName]))
		for name := range s.Steps[typeName] {
			stepNames = append(stepNames, name)
		}
		sort.Strings(stepNames)
		for _, stepName := range stepNames {
			if timeout := s.Steps[typeName][stepName].Timeout; timeout >= coresaga.DefaultStepAckWait {
				errs = append(errs, stepTimeoutError("saga.steps."+typeName+"."+stepName+".timeout", timeout))
			}
		}
	}
	return errors.Join(errs...)
}

// admission 是声明里的准入参数（协调器与参与方的 LoadPublisher 共用）。
func (c *config) admission() coresaga.AdmissionOptions {
	a := c.Saga.Admission
	return coresaga.AdmissionOptions{Enabled: a.Enabled, HighRecords: uint64(a.HighRecords), LowRecords: uint64(a.LowRecords), StaleAfter: a.StaleAfter}
}

// checkStepBudgets 查解析完预算之后每个定义的每个步骤（包括定义里写死的预算）：
//
//   - 超时短于步骤消费者的 AckWait（规则同 ValidateConfig 对配置键的检查）；
//   - saga.receipt_retention 至少是这一步重试窗口的 coresaga.ReceiptRetentionFactor 倍（coresaga.CheckReceiptRetention，
//     Engine.Register 用同一个函数）：参与方剪掉过了保留期的回执后，同一操作的重试会再执行一次业务。
//
// Init 与配置检查（CheckConfigAcrossMods，--check-config / doctor）都调它，规则只声明这一次。
func checkStepBudgets(budgets coresaga.StepBudgets, definitions []coresaga.Definition, receiptRetention time.Duration) error {
	var errs []error
	for _, definition := range definitions {
		resolved := budgets.Resolve(definition)
		for _, step := range resolved.Steps {
			if step.Timeout >= coresaga.DefaultStepAckWait {
				errs = append(errs, stepTimeoutError(fmt.Sprintf("saga %q step %q timeout", definition.Type, step.Name), step.Timeout))
			}
		}
		if err := coresaga.CheckReceiptRetention(resolved, receiptRetention); err != nil {
			errs = append(errs, fmt.Errorf("saga.receipt_retention: %w", err))
		}
	}
	return errors.Join(errs...)
}

func stepTimeoutError(what string, timeout time.Duration) error {
	return fmt.Errorf("%s (%s) must be shorter than the step consumers' ack wait (%s): an attempt runs until its command deadline "+
		"(dispatch + step timeout), and a longer one is redelivered to another consumer while it still runs",
		what, timeout, coresaga.DefaultStepAckWait)
}
