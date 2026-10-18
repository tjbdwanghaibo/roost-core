//go:build integration

package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
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
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	kitdataengine "github.com/tjbdwanghaibo/roost-core/wiring/dataengine"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	kitmongo "github.com/tjbdwanghaibo/roost-core/wiring/mongo"
	kitnats "github.com/tjbdwanghaibo/roost-core/wiring/nats"
	kitnest "github.com/tjbdwanghaibo/roost-core/wiring/nest"
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
	definition := coresaga.Definition{Type: "d12gift", Version: 1, Steps: []coresaga.Step{
		{Name: "debit", ForwardTopic: "d12gift.debit", CompensateTopic: "d12gift.debit.compensate"},
		{Name: "deliver", ForwardTopic: "d12gift.deliver", CompensateTopic: "d12gift.deliver.compensate"},
	}}
	env := startRealStepsEnv(t, []coresaga.Definition{definition}, false, nil)
	registry, token, mongoClient := env.registry, env.token, env.mongo
	sagaDatabase, dataDatabase, dataMod, sagaMod := env.sagaDatabase, env.dataDatabase, env.dataMod, env.sagaMod
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

// realStepsEnv 是 Steps 真实依赖用例的公共装配：真实 Mongo 副本集 + NATS JetStream 三节点，正式的 Mongo / NATS /
// DataEngine /（可选）Nest / saga Mod，saga 流与效果流都是三副本发布确认。库 roost_d12_saga_<pid>_<ns> /
// roost_d12_de_<pid>_<ns>、流 ROOST_D12_SAGA_<…> / ROOST_D12_EFFECTS_<…>，用后删除。
type realStepsEnv struct {
	registry                   *app.Registry
	access                     *entity.ManagerAccess
	token                      string
	sagaStream                 string
	sagaDatabase, dataDatabase string
	sagaPrefix                 string
	mongo                      fmongo.IMongo
	dataMod                    *kitdataengine.Mod
	sagaMod                    *Mod
	// nest 只在 withNest 时装配：真实 Nest 引擎（wiring/nest Mod），事务经 DataEngine 的 WAL → 投影 → outbox 提交。
	nest corenest.Client
}

func startRealStepsEnv(t *testing.T, definitions []coresaga.Definition, withNest bool, overrides map[string]any) *realStepsEnv {
	t.Helper()
	if os.Getenv("ROOST_DATAENGINE_IT") != "1" {
		t.Skip("set ROOST_DATAENGINE_IT=1 and source the isolated environment")
	}
	mongoURI, natsURL := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI"), os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if mongoURI == "" || natsURL == "" {
		t.Fatal("isolated integration environment variables are incomplete")
	}
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano()%1_000_000_000)
	env := &realStepsEnv{token: strings.ReplaceAll(suffix, "_", "x"), sagaDatabase: "roost_d12_saga_" + suffix, dataDatabase: "roost_d12_de_" + suffix}
	env.sagaPrefix = "d12saga" + env.token
	sagaStream, effectStream := "ROOST_D12_SAGA_"+suffix, "ROOST_D12_EFFECTS_"+suffix
	env.sagaStream = sagaStream
	cfg := viper.New()
	for key, value := range map[string]any{
		"sid": int32(902), "server_type": "d12-it", "persistence.engine": "dataengine",
		"mongo.uri": mongoURI, "mongo.require_replica_set": true, "mongo.connect_timeout": 5 * time.Second, "mongo.transaction_timeout": 15 * time.Second,
		"nats.url":            natsURL,
		"dataengine.database": env.dataDatabase, "dataengine.wal.dir": t.TempDir(),
		"dataengine.effects.stream": effectStream, "dataengine.effects.subject_prefix": "d12effect" + env.token,
		"dataengine.effects.replicas": 3, "dataengine.effects.max_bytes": int64(64 << 20),
		"dataengine.outbox.poll_interval": 50 * time.Millisecond, "dataengine.outbox.retry_min": 100 * time.Millisecond, "dataengine.outbox.retry_max": time.Second,
		"saga.database": env.sagaDatabase, "saga.stream": sagaStream, "saga.subject_prefix": env.sagaPrefix, "saga.replicas": 3,
		"saga.stream_max_bytes": int64(64 << 20), "saga.poll_interval": 50 * time.Millisecond,
		"saga.step_defaults.timeout": 5 * time.Second, "saga.step_defaults.max_attempts": 5,
		"saga.step_defaults.backoff_min": 100 * time.Millisecond, "saga.step_defaults.backoff_max": time.Second,
	} {
		cfg.Set(key, value)
	}
	for key, value := range overrides {
		cfg.Set(key, value)
	}
	env.registry = app.NewRegistry(cfg)
	access := entity.NewManagerAccess(entity.NewEntityManager())
	env.access = access
	mongoMod, natsMod := kitmongo.NewMongoMod(), kitnats.NewNatsMod(nil)
	env.dataMod = kitdataengine.NewMod(kitdataengine.WithEntityAccess(access))
	env.sagaMod = NewMod(definitions...)
	type lifecycle interface {
		app.Mod
		Init(*viper.Viper) error
		Provide(*app.Registry) error
		Start() error
		StopWithContext(context.Context) error
	}
	all := []lifecycle{mongoMod, natsMod, env.dataMod}
	if withNest {
		all = append(all, kitnest.NewMod(access))
	}
	all = append(all, env.sagaMod)
	checked := make([]app.Mod, 0, len(all))
	for _, mod := range all {
		checked = append(checked, mod)
	}
	if err := app.CheckConfig(cfg, checked...); err != nil {
		t.Fatal(err)
	}
	started := []lifecycle{}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		for i := len(started) - 1; i >= 0; i-- {
			if i == 0 && env.mongo != nil { // Mongo 最后停：先删本次的库
				for _, database := range []string{env.sagaDatabase, env.dataDatabase} {
					if err := env.mongo.Database(database).Drop(ctx); err != nil {
						t.Errorf("drop %s: %v", database, err)
					}
				}
			}
			if err := started[i].StopWithContext(ctx); err != nil {
				t.Errorf("stop %s: %v", started[i].Name(), err)
			}
		}
		deleteD12Streams(t, natsURL, sagaStream, effectStream)
	})
	for _, mod := range all {
		if err := mod.Init(cfg); err != nil {
			t.Fatal(err)
		}
	}
	for _, mod := range all {
		if err := mod.Provide(env.registry); err != nil {
			t.Fatal(err)
		}
		if err := mod.Start(); err != nil {
			t.Fatal(err)
		}
		started = append(started, mod)
		if mod == lifecycle(mongoMod) {
			env.mongo = mongoMod.Client()
		}
	}
	if withNest {
		env.nest = app.MustLookup[corenest.Client](env.registry, mods.ModNest)
	}
	return env
}

// 原生步骤在真实 Nest 调度下（D12 验证项 1）：真实 Nest 引擎（wiring/nest Mod，正式的 short / io 池与冷加载）+
// 真实 DataEngine（WAL → 投影 → outbox），步骤体经 Nest Client 请求一个手写 handler，handler 改实体余额并在事务里
// step.Complete。一份转账形状的两步 saga（debit 扣 from、credit 加 to，补偿 refund / credit-undo，四步都是原生步骤）：
//
//   - ok：正向完成；
//   - refuse：credit 超上限，业务拒绝（Complete(false, …)，只提交回执与失败的 completion），协调器补偿 debit；
//   - dup：debit 第一次投递执行中时再投一份同一命令（不带 MsgID，绕过流的去重窗口），完成后再投一份——都只回放；
//   - lostreply：debit 的 Nest 提交成功后步骤体返回错误（结果未知）——消费者保留租约，重投等到回执后 ack，不再执行；
//   - failfirst：debit 第一次在调用 Nest 之前返回错误（结果未知、其实没提交）——这次尝试等到截止，协调器按步骤超时发
//     第二次尝试执行；
//   - fenced：投影器被挡在 checkpoint 上，saga X 的 debit 已准入 WAL、投影未定（实体屏障），另一个 saga 类型 Y 的 bonus
//     步骤写同一实体，被 dataengine.ErrFencedEntityPending 拒绝——交还租约、立刻重投，屏障解除后在第一次尝试里生效，
//     而不是等到步骤超时；
//   - stall：投影器停到 S 的 debit 命令截止之后——投影时租约已过期，整笔记录被跳过、实体被驱逐，协调器的第二次尝试在
//     从 Mongo 重载的实体上生效，仍恰好一次。
//
// 核对：每笔 saga 的终态；Mongo 里每个账户的余额（每个操作实例恰好生效一次）与内存一致；拒绝没有改收款方；按场景的
// 步骤体 / handler 执行次数与生效尝试号。
//
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh，持有 remote-acceptance.lock，
//
//	ROOST_DATAENGINE_IT=1 GOWORK=off go test -tags integration -count=1 -run '^TestRealStepsRun' ./wiring/saga/
func TestRealStepsRunNativeStepsUnderTheRealNestScheduler(t *testing.T) {
	definition := coresaga.Definition{Type: "d12nest", Version: 1, Steps: []coresaga.Step{
		{Name: "debit", ForwardTopic: "d12nest.debit", CompensateTopic: "d12nest.debit.compensate"},
		{Name: "credit", ForwardTopic: "d12nest.credit", CompensateTopic: "d12nest.credit.compensate"},
	}}
	// bonus 是只有一步的另一个 saga 类型（另一个 durable），fenced 场景用它在 X 的屏障期间写同一实体。
	bonus := coresaga.Definition{Type: "d12bonus", Version: 1, Steps: []coresaga.Step{
		{Name: "bonus", ForwardTopic: "d12bonus.bonus", CompensateTopic: "d12bonus.bonus.compensate"},
	}}
	ensureStepAccountKind()
	// 步骤超时 10s：failfirst 的第二次尝试在它之后；fenced 的 Y 若错误地保留租约，也要等到它之后才会生效。
	env := startRealStepsEnv(t, []coresaga.Definition{definition, bonus}, true, map[string]any{"saga.step_defaults.timeout": 10 * time.Second})
	stepAccountDatabase.Store(env.dataDatabase)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	accounts := map[string]int64{}
	initial := map[string]int64{"a": 1000, "b": 1000, "c": 990, "d": 1000, "e": 0}
	for name, balance := range initial {
		id, err := entity.BuildEntityID(int64(9_100+len(accounts)), stepAccountKind)
		if err != nil {
			t.Fatal(err)
		}
		accounts[name] = id
		if _, err := env.mongo.Database(env.dataDatabase).Collection(stepAccountCollection).InsertOne(ctx,
			bson.M{"_id": id, "_version": uint64(1), "_schema": uint32(1), "balance": balance}); err != nil {
			t.Fatal(err)
		}
	}

	// probe 记下每个场景里步骤体与 handler 的执行，以及生效那一次的尝试号。
	probe := newStepProbe()
	// 投影 checkpoint 的闸门（fenced 场景）：关着时投影器停在 ack 上，后面的记录准入了却投影不了。
	runtime := env.dataMod.Runtime()
	var ackGate atomic.Pointer[chan struct{}]
	runtime.Projector.OverrideAck(func(ackCtx context.Context, fence corenest.CommitFence) error {
		if gate := ackGate.Load(); gate != nil {
			select {
			case <-*gate:
			case <-ackCtx.Done():
				return ackCtx.Err()
			}
		}
		return runtime.WAL.Ack(ackCtx, fence)
	})
	openGate := func() {
		if gate := ackGate.Swap(nil); gate != nil {
			close(*gate)
		}
	}
	t.Cleanup(openGate)

	nestClient := env.nest
	body := func(stepName string, rule func(transfer) (int64, int64)) coresaga.NativeStepBody {
		return func(ctx context.Context, step coresaga.NativeStep) error {
			command := step.Command()
			var payload transfer
			if err := json.Unmarshal(command.Payload, &payload); err != nil {
				return err
			}
			key := payload.Scenario + "/" + stepName
			calls := probe.body(key)
			probe.remember(key, command)
			if payload.Scenario == "failfirst" && stepName == "debit" && calls == 1 {
				return errors.New("d12: failed before the Nest call (outcome unknown to the consumer)")
			}
			if payload.Scenario == "dup" && stepName == "debit" && calls == 1 {
				probe.signal("dup-running")
				<-probe.wait("dup-release")
			}
			target, limit := rule(payload)
			account, amount := accounts[payload.From], -target
			if stepName == "credit" || stepName == "credit-undo" || stepName == "bonus" {
				account, amount = accounts[payload.To], target
			}
			if stepName == "refund" {
				amount = target
			}
			if stepName == "credit-undo" {
				amount = -target
			}
			_, err := nestClient.Request(ctx, stepAccountApply, account, corenest.NewParams(key, amount, limit, step))
			switch {
			case errors.Is(err, coredata.ErrFencedEntityPending):
				probe.count(key + "/fenced")
				return err
			case err != nil:
				return err
			}
			probe.effective(key, int(command.Attempt))
			if payload.Scenario == "fenced" && payload.SagaTag == "x" && stepName == "debit" {
				probe.signal("fenced-x-committed")
			}
			if payload.Scenario == "stall" && stepName == "debit" {
				probe.signal("stall-committed")
			}
			if payload.Scenario == "lostreply" && stepName == "debit" && calls == 1 {
				return errors.New("d12: Nest committed but the reply was lost (outcome unknown to the consumer)")
			}
			return nil
		}
	}
	amount := func(p transfer) (int64, int64) { return p.Amount, 0 }
	// 账户 c 有余额上限 1000（refuse 场景）；别的账户不设上限。
	capped := func(p transfer) (int64, int64) {
		if p.To == "c" {
			return p.Amount, 1000
		}
		return p.Amount, 0
	}
	stepAccountHandlerProbe.Store(probe)

	steps, err := NewSteps(env.registry, "d12n-"+env.token)
	if err != nil {
		t.Fatal(err)
	}
	steps.Native("debit", "d12nest.debit", body("debit", amount))
	steps.Native("refund", "d12nest.debit.compensate", body("refund", amount))
	steps.Native("credit", "d12nest.credit", body("credit", capped))
	steps.Native("credit-undo", "d12nest.credit.compensate", body("credit-undo", amount))
	steps.Native("bonus", "d12bonus.bonus", body("bonus", amount))
	steps.Native("bonus-undo", "d12bonus.bonus.compensate", body("bonus-undo", amount))
	if err := steps.Start(ctx); err != nil {
		t.Fatal(err)
	}
	start := func(scenario, tag, from, to string, value int64) string {
		t.Helper()
		id := "d12n-" + scenario + tag + "-" + env.token
		data, err := json.Marshal(transfer{Scenario: scenario, SagaTag: tag, From: from, To: to, Amount: value})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.sagaMod.Engine().StartSaga(ctx, coresaga.StartRequest{ID: id, Type: definition.Type, DefinitionVersion: 1, BusinessKey: id, Data: data}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	startBonus := func(scenario, tag, to string, value int64) string {
		t.Helper()
		id := "d12b-" + scenario + tag + "-" + env.token
		data, err := json.Marshal(transfer{Scenario: scenario, SagaTag: tag, To: to, Amount: value})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.sagaMod.Engine().StartSaga(ctx, coresaga.StartRequest{ID: id, Type: bonus.Type, DefinitionVersion: 1, BusinessKey: id, Data: data}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	finish := func(id string, want coresaga.Status, within time.Duration) time.Duration {
		t.Helper()
		begin := time.Now()
		for {
			record, err := env.sagaMod.Store().Get(ctx, id)
			if err == nil && record.Status.Terminal() {
				if record.Status != want {
					t.Fatalf("saga %s ended %s (last error %q), want %s", id, record.Status, record.LastError, want)
				}
				return time.Since(begin)
			}
			if time.Since(begin) > within {
				t.Fatalf("saga %s did not finish within %s: %+v err=%v", id, within, record, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}

	// 1. 正向与业务拒绝 → 补偿。
	okID, refuseID := start("ok", "", "a", "b", 10), start("refuse", "", "a", "c", 20)
	finish(okID, coresaga.StatusCompleted, time.Minute)
	finish(refuseID, coresaga.StatusCompensated, time.Minute)

	// 2. 重复投递：第一次投递执行中再投一份同一命令，完成后再投一份。
	dupID := start("dup", "", "a", "b", 5)
	select {
	case <-probe.wait("dup-running"):
	case <-ctx.Done():
		t.Fatal("dup: the first delivery never started")
	}
	dupCommand := probe.lastCommand("dup/debit")
	debitDurable := "d12n-" + env.token + "-debit"
	delivered := consumerDelivered(t, ctx, env, debitDurable)
	republishCommand(t, ctx, env, dupCommand)
	waitFor(t, ctx, "dup: the duplicate delivered while the first attempt holds the lease", func() bool {
		return consumerDelivered(t, ctx, env, debitDurable) > delivered
	})
	probe.signal("dup-release")
	finish(dupID, coresaga.StatusCompleted, time.Minute)
	republishCommand(t, ctx, env, dupCommand)
	waitFor(t, ctx, "dup: the debit consumer settles every delivery", func() bool { return consumerIdle(t, ctx, env, debitDurable) })

	// 3. 结果未知：提交后丢回复；调用前失败。
	lostID, failID := start("lostreply", "", "a", "b", 7), start("failfirst", "", "a", "b", 3)
	finish(lostID, coresaga.StatusCompleted, time.Minute)
	finish(failID, coresaga.StatusCompleted, time.Minute)

	// 4. 实体屏障：挡住 checkpoint，先提交一笔普通事务让投影器停在 ack 上，再让 X 的 debit 准入（投影不了，D 上有屏障），
	// 然后 Y（另一个 saga 类型的 bonus 步骤，另一个 durable）给 D 加钱，撞上屏障。Y 必须在另一个 durable 上：一个进程里
	// 同一 durable 的投递逐条处理，X 的消费者在等自己的投影，同一 durable 上的下一条要等它返回。
	closeGate := func() {
		gate := make(chan struct{})
		ackGate.Store(&gate)
		if _, err := nestClient.Request(ctx, stepAccountApply, accounts["e"], corenest.NewParams("touch", int64(1), int64(0), coresaga.NativeStep{})); err != nil {
			t.Fatal(err)
		}
	}
	closeGate()
	xID := start("fenced", "x", "d", "b", 11)
	select {
	case <-probe.wait("fenced-x-committed"):
	case <-ctx.Done():
		t.Fatal("fenced: X's debit never committed")
	}
	yID := startBonus("fenced", "y", "d", 13)
	waitFor(t, ctx, "fenced: Y's bonus is refused by the entity barrier and redelivered", func() bool { return probe.counted("fenced/bonus/fenced") >= 2 })
	opened := time.Now()
	openGate()
	finish(xID, coresaga.StatusCompleted, time.Minute)
	finish(yID, coresaga.StatusCompleted, time.Minute)
	yLatency := time.Since(opened)

	// 5. 投影停顿超过命令截止：S 的 debit 准入后投影器停到它的截止之后。投影时租约已过期，整笔记录被跳过、D 被驱逐，
	// 协调器按超时发第二次尝试，在从 Mongo 重载的 D 上生效——仍然恰好一次。
	closeGate()
	stallID := start("stall", "", "d", "b", 17)
	select {
	case <-probe.wait("stall-committed"):
	case <-ctx.Done():
		t.Fatal("stall: S's debit never committed")
	}
	time.Sleep(time.Until(probe.lastCommand("stall/debit").DeadlineAt) + 500*time.Millisecond)
	openGate()
	finish(stallID, coresaga.StatusCompleted, time.Minute)

	// 核对余额：Mongo（投影）与内存一致，每个操作实例恰好生效一次。
	want := map[string]int64{"a": 1000 - 10 - 5 - 7 - 3, "b": 1000 + 10 + 5 + 7 + 3 + 11 + 17, "c": 990, "d": 1000 - 11 + 13 - 17, "e": 2}
	waitFor(t, ctx, "every projection reaches Mongo", func() bool {
		for name, id := range accounts {
			var doc struct {
				Balance int64 `bson:"balance"`
			}
			if err := env.mongo.Database(env.dataDatabase).Collection(stepAccountCollection).FindOne(ctx, bson.M{"_id": id}, &doc); err != nil || doc.Balance != want[name] {
				return false
			}
		}
		return true
	})
	for name, id := range accounts {
		loaded := env.access.Manager().Get(id)
		if loaded == nil {
			continue // 已卸载：Mongo 已核对
		}
		if got := loaded.(*stepAccount).dao.balance; got != want[name] {
			t.Errorf("account %s: resident balance %d, Mongo %d", name, got, want[name])
		}
	}
	var refused struct {
		Version uint64 `bson:"_version"`
	}
	if err := env.mongo.Database(env.dataDatabase).Collection(stepAccountCollection).FindOne(ctx, bson.M{"_id": accounts["c"]}, &refused); err != nil || refused.Version != 1 {
		t.Errorf("refused credit changed account c: version %d err=%v, want 1 (receipt and failed completion only)", refused.Version, err)
	}

	// 核对执行次数与生效尝试。
	for key, wantRuns := range map[string]int{
		"ok/debit": 1, "ok/credit": 1, "refuse/debit": 1, "refuse/credit": 1, "refuse/refund": 1,
		"dup/debit": 1, "lostreply/debit": 1, "failfirst/debit": 2,
	} {
		if got := probe.counted(key); got != wantRuns {
			t.Errorf("%s: step body ran %d times, want %d", key, got, wantRuns)
		}
	}
	for key, wantHandler := range map[string]int{"dup/debit": 1, "lostreply/debit": 1, "failfirst/debit": 1, "refuse/credit": 1} {
		if got := probe.handlerRuns(key); got != wantHandler {
			t.Errorf("%s: Nest handler ran %d times, want %d", key, got, wantHandler)
		}
	}
	if attempts := probe.attempts("failfirst/debit"); len(attempts) != 1 || attempts[0] != 2 {
		t.Errorf("failfirst: effective debit attempts %v, want [2] (the first attempt waits out its deadline, the coordinator's second runs)", attempts)
	}
	if attempts := probe.attempts("lostreply/debit"); len(attempts) != 1 || attempts[0] != 1 {
		t.Errorf("lostreply: effective debit attempts %v, want [1]", attempts)
	}
	if attempts := probe.attempts("fenced/debit"); len(attempts) != 1 || attempts[0] != 1 {
		t.Errorf("fenced: X's effective debit attempts %v, want [1]", attempts)
	}
	if attempts := probe.attempts("fenced/bonus"); len(attempts) != 1 || attempts[0] != 1 {
		t.Errorf("fenced: Y's effective bonus attempts %v, want [1]: a delivery refused by the barrier hands its lease back and runs on the same attempt", attempts)
	}
	// stall：第一次尝试的 Nest 提交返回了成功（内存里生效过），投影时被 lease fence 跳过；第二次尝试才真正落库。
	if attempts := probe.attempts("stall/debit"); len(attempts) != 2 || attempts[0] != 1 || attempts[1] != 2 {
		t.Errorf("stall: debit commits by attempt %v, want [1 2] (the first skipped at projection, the second projected)", attempts)
	}
	evictions := int64(0)
	for _, metric := range app.MustLookup[*metrics.Registry](env.registry, app.ModMetrics).Snapshot() {
		if metric.Name == "dataengine.fence.evictions.started.total" {
			evictions += metric.Value
		}
	}
	if evictions != 1 {
		t.Errorf("dataengine.fence.evictions.started.total = %d, want 1 (the stalled attempt's entity)", evictions)
	}
	if yLatency >= 10*time.Second {
		t.Errorf("fenced: Y completed %s after the barrier lifted, not before the step timeout (10s)", yLatency)
	}
	t.Logf("fenced: Y refused by the barrier %d times, completed %s after the barrier lifted", probe.counted("fenced/bonus/fenced"), yLatency)

	drainCtx, drainCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer drainCancel()
	if err := steps.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
}

// transfer 是 d12nest saga 的载荷；Scenario 选择步骤体里的注入行为。
type transfer struct {
	Scenario string `json:"scenario"`
	SagaTag  string `json:"tag,omitempty"`
	From     string `json:"from"`
	To       string `json:"to"`
	Amount   int64  `json:"amount"`
}

// stepProbe 记下场景里的执行：步骤体次数、handler 次数、生效尝试号、最近一次命令与场景信号。
type stepProbe struct {
	mu       sync.Mutex
	counts   map[string]int
	handler  map[string]int
	tried    map[string][]int
	commands map[string]coresaga.Command
	signals  map[string]chan struct{}
}

func newStepProbe() *stepProbe {
	return &stepProbe{counts: map[string]int{}, handler: map[string]int{}, tried: map[string][]int{}, commands: map[string]coresaga.Command{}, signals: map[string]chan struct{}{}}
}

func (p *stepProbe) body(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.counts[key]++
	return p.counts[key]
}

func (p *stepProbe) count(key string) { p.body(key) }

func (p *stepProbe) counted(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.counts[key]
}

func (p *stepProbe) handled(key string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.handler[key]++
}

func (p *stepProbe) remember(key string, command coresaga.Command) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.commands[key] = command
}

func (p *stepProbe) handlerRuns(key string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.handler[key]
}

func (p *stepProbe) lastCommand(key string) coresaga.Command {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.commands[key]
}

func (p *stepProbe) effective(key string, attempt int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.tried[key] = append(p.tried[key], attempt)
}

func (p *stepProbe) attempts(key string) []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]int(nil), p.tried[key]...)
}

func (p *stepProbe) channel(name string) chan struct{} {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.signals[name] == nil {
		p.signals[name] = make(chan struct{})
	}
	return p.signals[name]
}

func (p *stepProbe) wait(name string) <-chan struct{} { return p.channel(name) }

func (p *stepProbe) signal(name string) {
	ch := p.channel(name)
	p.mu.Lock()
	defer p.mu.Unlock()
	select {
	case <-ch:
	default:
		close(ch)
	}
}

// 手写的余额实体：一个 DAO、一个字段，经正式 DataEngine 仓储从 Mongo 冷加载，提交时整文档 Put。
const (
	stepAccountKind       entity.EntityKind = 203
	stepAccountCollection                   = "d12_accounts"
)

var (
	stepAccountApply        = corenest.NewHandlerName("d12_step_account_apply")
	stepAccountDatabase     atomic.Value // string：本轮的 dataengine.database
	stepAccountHandlerProbe atomic.Pointer[stepProbe]
	registerStepAccount     sync.Once
)

type stepAccountDAO struct {
	id      int64
	balance int64
	tracker coredata.Tracker
}

func (dao *stepAccountDAO) Id() int64                       { return dao.id }
func (dao *stepAccountDAO) SetId(id int64)                  { dao.id = id }
func (*stepAccountDAO) DbName() string                      { return stepAccountDatabase.Load().(string) }
func (*stepAccountDAO) CollName() string                    { return stepAccountCollection }
func (dao *stepAccountDAO) Dirty() entity.IDirty            { return &dao.tracker }
func (dao *stepAccountDAO) CleanDirty()                     { dao.tracker.SelfClean() }
func (*stepAccountDAO) SchemaVersion() uint32               { return 1 }
func (dao *stepAccountDAO) DirtyTracker() *coredata.Tracker { return &dao.tracker }

func (dao *stepAccountDAO) PrepareMutation(change corenest.PersistChange) (coredata.Mutation, error) {
	raw, err := bson.Marshal(bson.M{"_id": dao.id, "balance": dao.balance})
	if err != nil {
		return coredata.Mutation{}, err
	}
	version := dao.tracker.Version()
	return coredata.Mutation{
		Key:  coredata.DocumentKey{Database: dao.DbName(), Resource: stepAccountCollection, ID: dao.id},
		Kind: coredata.MutationPut, ExpectedVersion: version, NextVersion: version + 1,
		Mask: change.Mask, Schema: 1, Codec: "bson-v2", Data: raw,
	}, nil
}

func (dao *stepAccountDAO) AcceptMutation(mutation coredata.Mutation) error {
	return dao.tracker.AcceptVersion(mutation.ExpectedVersion, mutation.NextVersion)
}

func (dao *stepAccountDAO) RestorePersisted(raw []byte, schema uint32, version uint64) error {
	if schema != 1 {
		return coredata.ErrSchemaMismatch
	}
	var doc struct {
		ID      int64 `bson:"_id"`
		Balance int64 `bson:"balance"`
	}
	if err := bson.Unmarshal(raw, &doc); err != nil {
		return err
	}
	dao.balance = doc.Balance
	dao.tracker.SetVersion(version)
	dao.tracker.SelfClean()
	return nil
}

type stepAccount struct {
	*entity.EntityBase
	dao *stepAccountDAO
}

func (value *stepAccount) Base() *entity.EntityBase                 { return value.EntityBase }
func (*stepAccount) OnInitFinish(*entity.EntityCreateParam) error   { return nil }
func (*stepAccount) OnDestroy(entity.EntityDestroyReason)           {}
func (*stepAccount) AutoPersist() bool                              { return true }
func (*stepAccount) IsRemoved() bool                                { return false }
func (*stepAccount) SetRemoved()                                    {}
func (*stepAccount) Touch() bool                                    { return true }
func (*stepAccount) UnTouch()                                       {}
func (*stepAccount) ClearBase()                                     {}
func (*stepAccount) IsClear() bool                                  { return false }
func (value *stepAccount) RangeDao(visit func(entity.DaoInterface)) { visit(value.dao) }

// ensureStepAccountKind 登记实体与 handler（进程级注册表，只登记一次；handler 经 stepAccountHandlerProbe 找本轮的探针）。
//
// handler 是业务写的原生步骤 handler：改余额（undo 回滚）、MarkPersist，并在同一事务里 step.Complete；limit > 0 时
// 超上限是业务拒绝——不改余额，只 Complete(false, …)。step 为零值（普通事务，fenced 场景的 touch）时不 Complete。
func ensureStepAccountKind() {
	registerStepAccount.Do(func() {
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: stepAccountKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface { return &stepAccountDAO{} }},
			Builder: func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
				dao, ok := param.Dao[stepAccountCollection].(*stepAccountDAO)
				if !ok {
					return nil, fmt.Errorf("d12: account %d has no DAO", param.Id)
				}
				return &stepAccount{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind), dao: dao}, nil
			},
		})
		corenest.MustRegisterHandlerWithMeta(stepAccountApply, func(entities []entity.IThreadSafeEntity, params []any, _ ...corenest.HandlerOption) (any, error) {
			if len(entities) != 1 || len(params) != 4 {
				return nil, fmt.Errorf("d12: apply wants one account and four params")
			}
			account, ok := entities[0].(*stepAccount)
			key, _ := params[0].(string)
			delta, _ := params[1].(int64)
			limit, _ := params[2].(int64)
			step, _ := params[3].(coresaga.NativeStep)
			if !ok {
				return nil, fmt.Errorf("d12: target is %T", entities[0])
			}
			if probe := stepAccountHandlerProbe.Load(); probe != nil {
				probe.handled(key)
			}
			if limit > 0 && account.dao.balance+delta > limit {
				return nil, step.Complete(false, "d12: balance cap exceeded")
			}
			previous := account.dao.balance
			account.dao.balance += delta
			corenest.RecordUndo(account.dao, 1, func() error { account.dao.balance = previous; return nil })
			if err := corenest.MarkPersist(account.dao, 1); err != nil {
				return nil, err
			}
			if step.Command().ID == "" {
				return nil, nil
			}
			return nil, step.Complete(true, "")
		}, corenest.HandlerMeta{Rollback: corenest.RollbackUndo, Durability: corenest.DurabilityStrict})
	})
}

// republishCommand 把同一条命令再发一次，不带 MsgID（绕过流的去重窗口）：消费者看到的就是一次重复投递。
func republishCommand(t *testing.T, ctx context.Context, env *realStepsEnv, command coresaga.Command) {
	t.Helper()
	raw, err := json.Marshal(struct {
		Version uint16           `json:"version"`
		Command coresaga.Command `json:"command"`
	}{coresaga.WireVersion, command})
	if err != nil {
		t.Fatal(err)
	}
	jetstream := app.MustLookup[fnats.IJetStream](env.registry, mods.ModNatsJetStream)
	if _, err := jetstream.Publish(ctx, env.sagaPrefix+".command."+command.Topic, raw, fnats.JetStreamPublishOptions{}); err != nil {
		t.Fatal(err)
	}
}

func consumerInfo(t *testing.T, ctx context.Context, env *realStepsEnv, durable string) *gojs.ConsumerInfo {
	t.Helper()
	nc, err := gonats.Connect(os.Getenv("ROOST_DATAENGINE_IT_NATS_URL"), gonats.Timeout(2*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	consumer, err := js.Consumer(ctx, env.sagaStream, durable)
	if err != nil {
		t.Fatalf("consumer %s on %s: %v", durable, env.sagaStream, err)
	}
	return consumer.CachedInfo()
}

func consumerDelivered(t *testing.T, ctx context.Context, env *realStepsEnv, durable string) uint64 {
	t.Helper()
	return consumerInfo(t, ctx, env, durable).Delivered.Consumer
}

func consumerIdle(t *testing.T, ctx context.Context, env *realStepsEnv, durable string) bool {
	t.Helper()
	info := consumerInfo(t, ctx, env, durable)
	return info.NumPending == 0 && info.NumAckPending == 0
}

func waitFor(t *testing.T, ctx context.Context, what string, done func() bool) {
	t.Helper()
	for deadline := time.Now().Add(time.Minute); !done(); time.Sleep(50 * time.Millisecond) {
		if time.Now().After(deadline) || ctx.Err() != nil {
			t.Fatalf("timed out: %s", what)
		}
	}
}
