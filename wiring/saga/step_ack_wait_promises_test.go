package saga

import (
	"strings"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
)

// RR-20261006-46（F06-C1）：步骤超时必须短于步骤消费者的 AckWait，启动前校验。
//
// 承诺：一次尝试在命令截止（派发时刻 + 步骤 Timeout）之前结束处理，步骤消费者在 AckWait（coresaga.DefaultStepAckWait，
// 生成的与 demo 的步骤消费者都用它）之内 ack；Timeout 不短于 AckWait 时，JetStream 会在 handler 还在跑时把同一条命令
// 重投给另一个消费者。saga Mod 在 App 启动前（CheckConfig / Init）拒绝这样的配置，并点名键。
//
// 旧行为：只校验 LeaseDuration > AckWait（SubscribeDataEngineStep），步骤 Timeout 与 AckWait 的关系没人查，
// saga.step_defaults.timeout: 45s 照常启动。
func TestStepTimeoutMustBeShorterThanTheStepConsumersAckWait(t *testing.T) {
	ack := coresaga.DefaultStepAckWait
	for name, tc := range map[string]struct {
		yaml       string
		definition coresaga.Definition
		want       string
	}{
		"step_defaults.timeout equal to ack wait": {yaml: "saga:\n  step_defaults:\n    timeout: " + ack.String() + "\n", definition: giftDefinition(), want: "saga.step_defaults.timeout"},
		"per-step override longer than ack wait":  {yaml: "saga:\n  steps:\n    gift_item:\n      debit:\n        timeout: 45s\n", definition: giftDefinition(), want: "saga.steps.gift_item.debit.timeout"},
		"timeout written in the definition": {yaml: "saga: {}\n", definition: func() coresaga.Definition {
			d := giftDefinition()
			d.Steps[1].Timeout = 45 * time.Second
			return d
		}(), want: `saga "gift_item" step "deliver"`},
	} {
		t.Run(name, func(t *testing.T) {
			cfg := yamlConfig(t, tc.yaml)
			err := NewMod(tc.definition).Init(cfg)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("Init = %v, want a refusal naming %s: a step that may run %s or longer outlives the step consumer's ack wait (%s) and is redelivered while it still runs", err, tc.want, ack, ack)
			}
		})
	}
	// 配置部分在任何 Mod Init 之前就被 CheckConfig 拒绝（A4 ① 的跨键规则）。
	if err := app.CheckConfig(yamlConfig(t, "server_type: game\nsid: 1\nsaga:\n  step_defaults:\n    timeout: 45s\n"), NewMod(giftDefinition())); err == nil || !strings.Contains(err.Error(), "saga.step_defaults.timeout") {
		t.Fatalf("CheckConfig = %v, want the step timeout refused before Init", err)
	}
	// 守卫：短于 AckWait 照常启动。
	if err := NewMod(giftDefinition()).Init(yamlConfig(t, "saga:\n  step_defaults:\n    timeout: "+(ack-time.Second).String()+"\n")); err != nil {
		t.Fatalf("a step timeout shorter than the ack wait was refused: %v", err)
	}
}
