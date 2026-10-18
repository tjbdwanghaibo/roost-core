package saga

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
)

// O-S5-2（维护者第十二轮决定）：原生 Nest 步骤的完成结果走 DataEngine 的效果流，那条流的保留期是
// dataengine.effects.max_age。完成回执（saga.completion_receipt_ttl）必须比它活得久：回执过期之后
// 流里还留着的结果再投递一次，协调器手里既没有回执也没有 tombstone，分不清“已计入的重复”和“放弃
// 之后才生效的成功”，只能 ErrNotWaiting → Term（REVIEW-2026-10-06-n06s5 §4）。saga 流本身的前提
// （ttl > saga.stream_max_age）启动时早就校验；效果流跨 Mod，之前没有校验，违反时只有一条通用的
// terminal 计数。现在 saga Mod 在结果效果流就是 DataEngine 效果流时一并校验，不满足拒绝启动并点名
// 两个键。
func TestModRefusesAnEffectStreamThatOutlivesTheCompletionReceipts(t *testing.T) {
	cfg := yamlConfig(t, "dataengine:\n  effects:\n    max_age: 800h\nsaga:\n  completion_receipt_ttl: 720h\n")
	err := NewMod().Init(cfg)
	if err == nil {
		t.Fatal("saga Mod started although dataengine.effects.max_age (800h) outlives saga.completion_receipt_ttl (720h)")
	}
	for _, want := range []string{"dataengine.effects.max_age", "saga.completion_receipt_ttl", "800h", "720h"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q does not name %s", err, want)
		}
	}
}

func TestModChecksEffectRetentionOnlyAgainstTheStreamItReadsResultsFrom(t *testing.T) {
	for name, text := range map[string]string{
		"defaults (7d effects, 30d receipts)": "",
		"equal stream, receipts outlive":      "dataengine:\n  effects:\n    max_age: 200h\nsaga:\n  completion_receipt_ttl: 720h\n",
		// 结果效果流不是 DataEngine 的效果流：那条流的保留期由别处管，这里不比较。
		"results on another stream": "dataengine:\n  effects:\n    max_age: 800h\nsaga:\n  completion_receipt_ttl: 720h\n  result_effect_stream: OTHER_EFFECTS\n",
		// DataEngine 改了流名，saga 跟着改：仍是同一条流，要比较——这里 ttl 更长，通过。
		"renamed stream on both sides": "dataengine:\n  effects:\n    stream: GAME_EFFECTS\n    max_age: 100h\nsaga:\n  start_effect_stream: GAME_EFFECTS\n",
	} {
		if err := NewMod().Init(yamlConfig(t, text)); err != nil {
			t.Errorf("%s: %v", name, err)
		}
	}
	renamed := "dataengine:\n  effects:\n    stream: GAME_EFFECTS\n    max_age: 800h\nsaga:\n  completion_receipt_ttl: 720h\n  start_effect_stream: GAME_EFFECTS\n"
	if err := NewMod().Init(yamlConfig(t, renamed)); err == nil || !strings.Contains(err.Error(), "dataengine.effects.max_age") {
		t.Fatalf("renamed shared stream with 800h retention: %v; want the cross-Mod check to refuse", err)
	}
}

// Nest 发起的 start 与原生步骤的结果都由 DataEngine 发布到 dataengine.effects 那条流上，saga 的消费者必须订阅同一条流、
// 同一个主题前缀。没写 saga.start_effect_stream / start_effect_prefix 时直接取 dataengine.effects 的值（效果流只写一处）。
func TestSagaFollowsTheDataEngineEffectStreamByDefault(t *testing.T) {
	mod := NewMod()
	if err := mod.Init(yamlConfig(t, "dataengine:\n  effects:\n    stream: OTHER_EFFECTS\n    subject_prefix: other.effect.\n")); err != nil {
		t.Fatal(err)
	}
	for name, got := range map[string]string{
		"start stream": mod.config.Starts.Stream, "start prefix": mod.config.Starts.EffectPrefix,
		"result stream": mod.config.NestResults.Stream, "result prefix": mod.config.NestResults.EffectPrefix,
	} {
		want := "OTHER_EFFECTS"
		if strings.HasSuffix(name, "prefix") {
			want = "other.effect"
		}
		if got != want {
			t.Errorf("saga %s = %q, want dataengine.effects value %q", name, got, want)
		}
	}
}

// 同一进程里有 DataEngine 时，显式写了且与 dataengine.effects 不同的 saga 效果流键在配置检查（启动前、--check-config）
// 就拒绝并点名两个键与改法；修前要到启动订阅时才报 "stream not found"，不说是哪两个键。
func TestConfigCheckRefusesSagaEffectKeysThatDisagreeWithDataEngine(t *testing.T) {
	base := "sid: 1\nserver_type: game\ndataengine:\n  effects:\n    stream: OTHER_EFFECTS\n    subject_prefix: other.effect\n"
	for name, tc := range map[string]struct{ saga, keys string }{
		"stream": {"saga:\n  start_effect_stream: ROOST_EFFECTS\n", "saga.start_effect_stream dataengine.effects.stream"},
		"prefix": {"saga:\n  start_effect_prefix: roost.effect\n", "saga.start_effect_prefix dataengine.effects.subject_prefix"},
	} {
		err := app.CheckConfig(yamlConfig(t, base+tc.saga), NewMod(), kitdataengine.NewMod())
		if err == nil {
			t.Errorf("%s: config check passed although saga and dataengine.effects name different effect streams", name)
			continue
		}
		for _, want := range append(strings.Fields(tc.keys), "delete") {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("%s: error %q does not mention %s", name, err, want)
			}
		}
	}
	// 写成相同的值（含首尾点的差别）不报错；没有 DataEngine 的进程（只跑协调器）可以显式指向别的流。
	same := base + "saga:\n  start_effect_stream: OTHER_EFFECTS\n  start_effect_prefix: .other.effect\n"
	if err := app.CheckConfig(yamlConfig(t, same), NewMod(), kitdataengine.NewMod()); err != nil {
		t.Errorf("equal values refused: %v", err)
	}
	if err := app.CheckConfig(yamlConfig(t, base+"saga:\n  start_effect_stream: ROOST_EFFECTS\n"), NewMod()); err != nil {
		t.Errorf("a saga-only process pointing at another effect stream refused: %v", err)
	}
}

// 正式 Mod 装配与 health 检查不能遗漏原生完成消费者。
type healthSubscription struct {
	once   sync.Once
	closed chan struct{}
}

func (s *healthSubscription) Stop()                   { s.once.Do(func() { close(s.closed) }) }
func (s *healthSubscription) Drain()                  { s.Stop() }
func (s *healthSubscription) Closed() <-chan struct{} { return s.closed }

type healthJetStream struct {
	subs map[string]*healthSubscription
}

func (*healthJetStream) EnsureStream(context.Context, fnats.JetStreamConfig) error { return nil }
func (*healthJetStream) Publish(context.Context, string, []byte, fnats.JetStreamPublishOptions) (fnats.JetStreamPublishAck, error) {
	return fnats.JetStreamPublishAck{}, nil
}
func (j *healthJetStream) Subscribe(_ context.Context, cfg fnats.JetStreamConsumerConfig, _ fnats.JetStreamHandler) (fnats.IJetStreamSubscription, error) {
	s := &healthSubscription{closed: make(chan struct{})}
	j.subs[cfg.FilterSubject] = s
	return s, nil
}

func TestSagaModHealthDetectsEveryConsumerAndRecovers(t *testing.T) {
	for _, subject := range []string{"roost.saga.result.>", "roost.effect.saga.start", "roost.effect.saga.result.>"} {
		t.Run(subject, func(t *testing.T) {
			cfg := viper.New()
			registry := app.NewRegistry(cfg)
			js := &healthJetStream{subs: make(map[string]*healthSubscription)}
			if err := registry.Register(mods.ModMongo, mongotest.NewClient()); err != nil {
				t.Fatal(err)
			}
			if err := registry.Register(mods.ModNatsJetStream, js); err != nil {
				t.Fatal(err)
			}
			m := NewMod()
			if err := m.Init(cfg); err != nil {
				t.Fatal(err)
			}
			if err := m.Provide(registry); err != nil {
				t.Fatal(err)
			}
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				ctx, cancel := context.WithTimeout(context.Background(), time.Second)
				defer cancel()
				if err := m.StopWithContext(ctx); err != nil {
					t.Error(err)
				}
			})
			if got := m.checkHealth(context.Background()); got.Status != health.StatusOK {
				t.Fatalf("healthy start: %+v", got)
			}
			if len(js.subs) != 3 || js.subs[subject] == nil {
				t.Fatalf("unexpected subscriptions: %v", js.subs)
			}
			js.subs[subject].Stop()
			if got := m.checkHealth(context.Background()); got.Status != health.StatusFail || got.Message != "durable consumer stopped" {
				t.Errorf("%s closed but health=%+v", subject, got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			err := m.StopWithContext(ctx)
			cancel()
			if err != nil {
				t.Fatal(err)
			}
			if got := m.checkHealth(context.Background()); got.Status != health.StatusFail {
				t.Fatalf("stopped health: %+v", got)
			}
			if err := m.Start(); err != nil {
				t.Fatal(err)
			}
			if got := m.checkHealth(context.Background()); got.Status != health.StatusOK {
				t.Fatalf("fresh subscriptions did not recover health: %+v", got)
			}
		})
	}
}
