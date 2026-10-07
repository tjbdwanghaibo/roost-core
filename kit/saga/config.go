package saga

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/tjbdwanghaibo/roost-core/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/saga"
)

// saga.* 的声明（维护者决定 A4 ①）。缺省值与 coresaga.DefaultOptions 相同（TestSagaDeclaredDefaultsMatchCoreDefaults），
// 写 0 或负数拒绝；要缺省就不写这个键。

// stepBudgetConfig 是一个步骤的超时与重试预算（U-0280）：saga.step_defaults 与 saga.steps.<type>.<step> 共用。
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

type config struct {
	app.ServiceIdentity
	Saga struct {
		Database     string           `config:"database" default:"saga" example:"saga"`
		StepDefaults stepBudgetConfig `config:"step_defaults,closed" help:"Step timeout and retry budget (U-0280). Fields a saga definition leaves unset\ntake these; one operation of a step gets up to max_attempts attempts, at most\none of which takes effect."`
		// Steps 按 <saga type>.<step name> 覆盖，优先于定义与 step_defaults；类型与步骤名要对应已注册的定义。
		Steps       map[string]map[string]stepBudgetConfig `config:"steps" example:"{}" help:"Per-step overrides, winning over the definition and step_defaults:\nsteps.<saga type>.<step name>.<timeout|max_attempts|backoff_min|backoff_max>."`
		Owner       string                                 `config:"owner" help:"协调器租约的持有者名，不写取 saga-<sid>-<随机 id>"`
		Collections struct {
			Sagas       string `config:"sagas"`
			Outbox      string `config:"outbox"`
			Completions string `config:"completions"`
			Operations  string `config:"operations"`
		} `config:"collections" help:"Mongo 集合名，不写取 core 的缺省"`
		SubjectPrefix         string         `config:"subject_prefix" default:"roost.saga" example:"roost.saga"`
		Stream                string         `config:"stream" default:"ROOST_SAGA" example:"ROOST_SAGA"`
		CoordinatorWorkers    int            `config:"coordinator_workers" default:"4" min:"1" example:"4"`
		PublisherWorkers      int            `config:"publisher_workers" default:"4" min:"1" example:"4"`
		CoordinatorClaimBatch int            `config:"coordinator_claim_batch" default:"3" min:"1" example:"3"`
		PublisherClaimBatch   int            `config:"publisher_claim_batch" default:"1" min:"1" example:"1"`
		LeaseDuration         time.Duration  `config:"lease_duration" default:"15s" min:"1ns" example:"15s"`
		StoreTimeout          time.Duration  `config:"store_timeout" default:"3s" min:"1ns" example:"3s"`
		PollInterval          time.Duration  `config:"poll_interval" default:"100ms" min:"1ns" example:"100ms"`
		PublishTimeout        time.Duration  `config:"publish_timeout" default:"3s" min:"1ns" example:"3s"`
		PublishBackoffMin     time.Duration  `config:"publish_backoff_min" default:"50ms" min:"1ns" example:"50ms"`
		PublishBackoffMax     time.Duration  `config:"publish_backoff_max" default:"5s" min:"1ns" example:"5s"`
		MaxPayloadBytes       int            `config:"max_payload_bytes" default:"65536" min:"1" example:"65536"`
		CompletionReceiptTTL  time.Duration  `config:"completion_receipt_ttl" default:"720h" min:"1ns" example:"720h"`
		StreamMaxAge          time.Duration  `config:"stream_max_age" default:"168h" min:"1ns" example:"168h"`
		StreamMaxBytes        int64          `config:"stream_max_bytes" default:"8589934592" min:"1" example:"8589934592"`
		DuplicateWindow       time.Duration  `config:"duplicate_window" default:"10m" min:"1ns" example:"10m"`
		Replicas              int            `config:"replicas" default:"1" min:"1" example:"1"`
		ResultDurable         string         `config:"result_durable" default:"roost-saga-coordinator"`
		Result                consumerConfig `config:"result_"`
		// 原生 Nest 步骤的开始请求与完成结果都在 DataEngine 的效果流上（RR-20260917-07）。
		StartEffectStream   string         `config:"start_effect_stream" default:"ROOST_EFFECTS" example:"ROOST_EFFECTS"`
		StartEffectPrefix   string         `config:"start_effect_prefix" default:"roost.effect" example:"roost.effect"`
		StartEffectDurable  string         `config:"start_effect_durable" default:"roost-saga-start" example:"roost-saga-start"`
		StartEffect         consumerConfig `config:"start_effect_"`
		ResultEffectStream  string         `config:"result_effect_stream" help:"不写取 start_effect_stream"`
		ResultEffectPrefix  string         `config:"result_effect_prefix" help:"不写取 start_effect_prefix"`
		ResultEffectDurable string         `config:"result_effect_durable" example:"roost-saga-start-result" help:"不写由 core 推出"`
		ResultEffect        consumerConfig `config:"result_effect_"`
	} `config:"saga"`
}

// ValidateConfig 是 saga.* 的跨键规则（A4 ①）：配置里写的步骤超时必须短于步骤消费者的 AckWait（RR-20261006-46）。
// 一次尝试的处理以命令截止（派发时刻 + 步骤 Timeout）为界，步骤消费者在 AckWait 之内 ack；Timeout 不短于 AckWait 时
// JetStream 会在 handler 还在跑时把同一条命令重投给另一个消费者。步骤消费者的 AckWait 不是配置：生成的与 demo 的步骤消费者
// 都取 coresaga.DefaultStepAckWait，这里与它比较（不另声明一个没人读的 ack_wait 键，RR-20261006-38 的教训）。
// 定义里写死的 Timeout 不在配置里，由 Init 在解析步骤预算之后用 checkStepTimeouts 查。
func (c *config) ValidateConfig(bool) error {
	var errs []error
	if timeout := c.Saga.StepDefaults.Timeout; timeout >= coresaga.DefaultStepAckWait {
		errs = append(errs, stepTimeoutError("saga.step_defaults.timeout", timeout))
	}
	typeNames := make([]string, 0, len(c.Saga.Steps))
	for name := range c.Saga.Steps {
		typeNames = append(typeNames, name)
	}
	sort.Strings(typeNames)
	for _, typeName := range typeNames {
		stepNames := make([]string, 0, len(c.Saga.Steps[typeName]))
		for name := range c.Saga.Steps[typeName] {
			stepNames = append(stepNames, name)
		}
		sort.Strings(stepNames)
		for _, stepName := range stepNames {
			if timeout := c.Saga.Steps[typeName][stepName].Timeout; timeout >= coresaga.DefaultStepAckWait {
				errs = append(errs, stepTimeoutError("saga.steps."+typeName+"."+stepName+".timeout", timeout))
			}
		}
	}
	return errors.Join(errs...)
}

// checkStepTimeouts 查解析完预算之后每个定义的每个步骤（包括定义里写死的 Timeout），规则同 ValidateConfig。
func checkStepTimeouts(budgets coresaga.StepBudgets, definitions []coresaga.Definition) error {
	var errs []error
	for _, definition := range definitions {
		for _, step := range budgets.Resolve(definition).Steps {
			if step.Timeout >= coresaga.DefaultStepAckWait {
				errs = append(errs, stepTimeoutError(fmt.Sprintf("saga %q step %q timeout", definition.Type, step.Name), step.Timeout))
			}
		}
	}
	return errors.Join(errs...)
}

func stepTimeoutError(what string, timeout time.Duration) error {
	return fmt.Errorf("%s (%s) must be shorter than the step consumers' ack wait (%s): an attempt runs until its command deadline "+
		"(dispatch + step timeout), and a longer one is redelivered to another consumer while it still runs",
		what, timeout, coresaga.DefaultStepAckWait)
}
