//go:build integration

package saga

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/spf13/viper"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// Steps 在真实依赖上端到端（D12 方向 A）：真实 Mongo 副本集 + NATS JetStream 三节点，正式的 Mongo / NATS / DataEngine /
// saga Mod 装配（saga 流与效果流都是三副本发布确认），业务只用 Steps 登记步骤体。一笔赠礼形状的两步 saga：
//
//   - debit（原生步骤）：步骤体在“Nest 事务”里 step.Complete，并写一笔业务 Put；提交经 DataEngine（WAL → 投影 → outbox），
//     completion 作为 effect 发到效果流，saga Mod 的原生完成消费者收下；
//   - deliver（Mongo 步骤）：handler 在 Mongo 事务里写 mails；saga id 含 refuse 的拒绝（mailbox full）→ 协调器补偿 debit，
//     补偿 refund 同样是原生步骤。
//
// 核对：两笔 saga 分别 completed / compensated；每个操作实例的业务写恰好一份（debits 2、refunds 1、mails 1）；
// 按 durable 的执行计数；Drain 在期限内返回。
//
// “Nest 事务”用 corenest.RunIsolatedTransaction 截下 CommitRecord 后交给 DataEngine Projector.CommitSystem：真实的
// WAL、投影（lease fence 判定）与 outbox，只是没有 Nest 调度。
//
// 资源：库 roost_d12_saga_<pid>_<ns> / roost_d12_de_<pid>_<ns>、流 ROOST_D12_SAGA_<…> / ROOST_D12_EFFECTS_<…>，用后删除。
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh，持有 remote-acceptance.lock，
//
//	ROOST_DATAENGINE_IT=1 GOWORK=off go test -tags integration -count=1 -run '^TestRealStepsRun' ./wiring/saga/
func TestRealStepsRunNativeAndMongoStepsThroughTheMods(t *testing.T) {
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 and source the isolated environment")
	}
	mongoURI, natsURL := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"), os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if mongoURI == "" || natsURL == "" {
		t.Fatal("isolated integration environment variables are incomplete")
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	token := strings.ReplaceAll(suffix, "_", "x")
	sagaDatabase, dataDatabase := "roost_d12_saga_"+suffix, "roost_d12_de_"+suffix
	sagaStream, effectStream := "ROOST_D12_SAGA_"+suffix, "ROOST_D12_EFFECTS_"+suffix
	definition := coresaga.Definition{Type: "d12gift", Version: 1, Steps: []coresaga.Step{
		{Name: "debit", ForwardTopic: "d12gift.debit", CompensateTopic: "d12gift.debit.compensate"},
		{Name: "deliver", ForwardTopic: "d12gift.deliver", CompensateTopic: "d12gift.deliver.compensate"},
	}}

	cfg := viper.New()
	for key, value := range map[string]any{
		"sid": int32(902), "server_type": "d12-it", "persistence.engine": "dataengine",
		"mongo.uri": mongoURI, "mongo.require_replica_set": true, "mongo.connect_timeout": 5 * time.Second, "mongo.transaction_timeout": 15 * time.Second,
		"nats.url":            natsURL,
		"dataengine.database": dataDatabase, "dataengine.wal.dir": t.TempDir(),
		"dataengine.effects.stream": effectStream, "dataengine.effects.subject_prefix": "d12effect" + token,
		"dataengine.effects.replicas": 3, "dataengine.effects.max_bytes": int64(64 << 20),
		"dataengine.outbox.poll_interval": 50 * time.Millisecond, "dataengine.outbox.retry_min": 100 * time.Millisecond, "dataengine.outbox.retry_max": time.Second,
		"saga.database": sagaDatabase, "saga.stream": sagaStream, "saga.subject_prefix": "d12saga" + token, "saga.replicas": 3,
		"saga.stream_max_bytes": int64(64 << 20), "saga.poll_interval": 50 * time.Millisecond,
		"saga.step_defaults.timeout": 5 * time.Second, "saga.step_defaults.max_attempts": 5,
		"saga.step_defaults.backoff_min": 100 * time.Millisecond, "saga.step_defaults.backoff_max": time.Second,
	} {
		cfg.Set(key, value)
	}
	registry := app.NewRegistry(cfg)
	mongoMod, natsMod := kitmongo.NewMongoMod(), kitnats.NewNatsMod(nil)
	dataMod := kitdataengine.NewMod(kitdataengine.WithEntityAccess(entity.NewManagerAccess(entity.NewEntityManager())))
	sagaMod := NewMod(definition)
	if err := app.CheckConfig(cfg, mongoMod, natsMod, dataMod, sagaMod); err != nil {
		t.Fatal(err)
	}
	type lifecycle interface {
		Init(*viper.Viper) error
		Provide(*app.Registry) error
		Start() error
		StopWithContext(context.Context) error
	}
	started := []lifecycle{}
	var mongoClient fmongo.IMongo
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for i := len(started) - 1; i >= 0; i-- {
			if i == 0 && mongoClient != nil { // Mongo 最后停：先删本次的库
				for _, database := range []string{sagaDatabase, dataDatabase} {
					if err := mongoClient.Database(database).Drop(ctx); err != nil {
						t.Errorf("drop %s: %v", database, err)
					}
				}
			}
			_ = started[i].StopWithContext(ctx)
		}
		deleteD12Streams(t, natsURL, sagaStream, effectStream)
	})
	for _, mod := range []lifecycle{mongoMod, natsMod, dataMod, sagaMod} {
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, mod := range []lifecycle{mongoMod, natsMod, dataMod, sagaMod} {
		if err := mod.Provide(registry); err != nil {
			t.Fatal(err)
		}
		if err := mod.Start(); err != nil {
			t.Fatal(err)
		}
		started = append(started, mod)
		if mod == lifecycle(mongoMod) {
			mongoClient = mongoMod.Client()
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	var serial atomic.Int64
	// nativeBody 是业务的原生步骤体：在“Nest 事务”里 step.Complete，并写一笔以执行序号为键的业务 Put——同一操作实例
	// 若有两次尝试都生效，resource 里就会有两份。
	nativeBody := func(resource string) coresaga.NativeStepBody {
		return func(ctx context.Context, step coresaga.NativeStep) error {
			committer := &capturingCommitter{}
			if _, err := corenest.RunIsolatedTransaction(ctx, committer, "d12-"+resource, func() (any, error) {
				return nil, step.Complete(true, "")
			}); err != nil {
				return err
			}
			id := serial.Add(1)
			data, err := bson.Marshal(bson.M{"_id": id, "operation": step.Command().IdempotencyKey})
			if err != nil {
				return err
			}
			record := committer.record
			record.Mutations = append(record.Mutations, coredata.Mutation{
				Key:  coredata.DocumentKey{Database: dataDatabase, Resource: resource, ID: id},
				Kind: coredata.MutationPut, NextVersion: 1, Mask: coredata.AllFields, Schema: 1, Codec: "bson-v2", Data: data,
			})
			_, err = dataMod.Runtime().Projector.CommitSystem(ctx, record)
			return err
		}
	}
	group := "d12-" + token
	steps, err := NewSteps(registry, group)
	if err != nil {
		t.Fatal(err)
	}
	steps.Native("debit", "d12gift.debit", nativeBody("d12_debits"))
	steps.Native("refund", "d12gift.debit.compensate", nativeBody("d12_refunds"))
	steps.Mongo("deliver", "d12gift.deliver", func(txCtx context.Context, command coresaga.Command) (coresaga.Completion, error) {
		if strings.Contains(command.SagaID, "refuse") {
			return coresaga.Completion{Success: false, Error: "mailbox full"}, nil
		}
		if _, err := mongoClient.Database(sagaDatabase).Collection("d12_mails").InsertOne(txCtx, bson.M{"_id": command.IdempotencyKey}); err != nil {
			return coresaga.Completion{}, err
		}
		return coresaga.Completion{Success: true}, nil
	})
	steps.Mongo("deliver-undo", "d12gift.deliver.compensate", func(context.Context, coresaga.Command) (coresaga.Completion, error) {
		return coresaga.Completion{Success: true}, nil
	})
	if err := steps.Start(ctx); err != nil {
		t.Fatal(err)
	}

	want := map[string]coresaga.Status{"d12-ok-" + token: coresaga.StatusCompleted, "d12-refuse-" + token: coresaga.StatusCompensated}
	for id := range want {
		if _, err := sagaMod.Engine().StartSaga(ctx, coresaga.StartRequest{ID: id, Type: definition.Type, DefinitionVersion: 1, BusinessKey: id, Data: []byte(`{}`)}); err != nil {
			t.Fatal(err)
		}
	}
	for id, status := range want {
		deadline := time.Now().Add(60 * time.Second)
		for {
			record, err := sagaMod.Store().Get(ctx, id)
			if err == nil && record.Status.Terminal() {
				if record.Status != status {
					t.Fatalf("saga %s ended %s, want %s", id, record.Status, status)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("saga %s did not finish: %+v err=%v", id, record, err)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}
	for database, counts := range map[string]map[string]int64{dataDatabase: {"d12_debits": 2, "d12_refunds": 1}, sagaDatabase: {"d12_mails": 1}} {
		for collection, want := range counts {
			got, err := mongoClient.Database(database).Collection(collection).CountDocuments(ctx, bson.M{})
			if err != nil || got != want {
				t.Errorf("%s.%s has %d documents (err=%v), want %d: each step operation takes effect exactly once", database, collection, got, err, want)
			}
		}
	}
	metricsRegistry := app.MustLookup[*metrics.Registry](registry, app.ModMetrics)
	runs := func(step string) int64 {
		var total int64
		for _, metric := range metricsRegistry.Snapshot() {
			if metric.Name == "saga.step.run_total" && metric.Labels["consumer"] == group+"-"+step {
				total += metric.Value
			}
		}
		return total
	}
	if runs("debit") < 2 || runs("refund") < 1 || runs("deliver") < 2 {
		t.Errorf("saga.step.run_total: debit=%d refund=%d deliver=%d", runs("debit"), runs("refund"), runs("deliver"))
	}
	drainCtx, drainCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer drainCancel()
	if err := steps.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// capturingCommitter 截下“Nest 事务”的 CommitRecord，测试再交给 DataEngine。
type capturingCommitter struct{ record coredata.CommitRecord }

func (committer *capturingCommitter) Commit(_ context.Context, record corenest.CommitRecord) error {
	committer.record = coredata.CloneCommitRecord(record)
	return nil
}

func deleteD12Streams(t *testing.T, url string, streams ...string) {
	t.Helper()
	nc, err := gonats.Connect(url, gonats.Timeout(2*time.Second))
	if err != nil {
		t.Errorf("cleanup: connect: %v", err)
		return
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Errorf("cleanup: JetStream: %v", err)
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	for _, stream := range streams {
		if err := js.DeleteStream(ctx, stream); err != nil && !errors.Is(err, gojs.ErrStreamNotFound) {
			t.Errorf("cleanup: delete stream %s: %v", stream, err)
		}
	}
}
