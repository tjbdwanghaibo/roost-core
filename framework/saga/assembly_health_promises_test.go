package saga

import (
	"context"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/mongotest"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
)

// RR-20261005-NC-37：任意一个必需消费者缺失或退出，都必须使装配报告不健康。
// 第三个消费者负责原生 Nest 完成回执，不能只检查启动意图和普通完成消费者。
func TestAssemblyHealthIncludesEveryRequiredConsumer(t *testing.T) {
	for _, which := range []string{"ordinary_result", "start", "native_result"} {
		for _, missing := range []bool{false, true} {
			t.Run(which+map[bool]string{false: "/closed", true: "/missing"}[missing], func(t *testing.T) {
				subs := []*recordedSubscription{
					{closed: make(chan struct{})}, {closed: make(chan struct{})}, {closed: make(chan struct{})},
				}
				a := &Assembly{resultSub: subs[0], startSub: subs[1], nestResults: subs[2]}
				if a.ConsumersClosed() {
					t.Fatal("three open consumers reported closed")
				}
				index := map[string]int{"ordinary_result": 0, "start": 1, "native_result": 2}[which]
				if missing {
					switch index {
					case 0:
						a.resultSub = nil
					case 1:
						a.startSub = nil
					case 2:
						a.nestResults = nil
					}
				} else {
					subs[index].Stop()
				}
				if !a.ConsumersClosed() {
					t.Fatalf("%s unavailable but ConsumersClosed=false (missing=%v)", which, missing)
				}
				// 独立的新生命周期恢复三个消费者；不伪造旧订阅复活。
				recovered := &Assembly{resultSub: &recordedSubscription{closed: make(chan struct{})}, startSub: &recordedSubscription{closed: make(chan struct{})}, nestResults: &recordedSubscription{closed: make(chan struct{})}}
				if recovered.ConsumersClosed() {
					t.Fatal("replacement consumers did not restore health")
				}
			})
		}
	}
}

func TestAssemblyNativeConsumerClosureIsVisibleAfterFormalStart(t *testing.T) {
	cfg := AssemblyConfig{
		Store: MongoStoreOptions{Database: "health"}, Engine: DefaultOptions(), Prefix: "roost.saga",
		Stream:      fnats.JetStreamConfig{Name: "ROOST_SAGA"},
		Completions: CompletionConsumerConfig{Stream: "ROOST_SAGA", Durable: "results", SubjectPrefix: "roost.saga"},
		Starts:      NestStartConsumerConfig{Stream: "ROOST_EFFECTS", Durable: "starts", EffectPrefix: "roost.effect"},
	}
	a, err := Assemble(mongotest.NewClient(), newRecordingJetStream(), cfg, testDefinition())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Start(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		if err := a.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	if !a.Running() || a.ConsumersClosed() {
		t.Fatal("formal Start did not establish healthy runtime")
	}
	a.nestResults.Stop()
	if !a.Running() || !a.ConsumersClosed() {
		t.Fatalf("native consumer failure masked by running loop: running=%v closed=%v", a.Running(), a.ConsumersClosed())
	}
}
