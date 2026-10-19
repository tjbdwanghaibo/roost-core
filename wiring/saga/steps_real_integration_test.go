//go:build integration

package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	"github.com/tjbdwanghaibo/roost-core/framework/nestwal"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
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

	// 1. 正向与业务拒绝 → 补偿。两笔的 debit 都扣 a、在同一个 durable 上：回调不再等前一条的回执投影，后一条可能在
	// 前一条投影之前进入 Nest、撞上 a 的实体屏障（交还租约、nak 后重投），所以下面按“未被屏障挡回的执行”计次数。
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
	// 然后 Y（另一个 saga 类型的 bonus 步骤，另一个 durable）给 D 加钱，撞上屏障。（X 等回执在后台，不占 durable 的
	// 回调；Y 放在另一个 durable 上只是让场景与步骤类型无关。）
	closeGate := func() {
		gate := make(chan struct{})
		ackGate.Store(&gate)
		if _, err := nestClient.Request(ctx, stepAccountApply, accounts["e"], corenest.NewParams("touch", int64(1), int64(0), stepCompleter(nil))); err != nil {
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

	// 核对执行次数与生效尝试。被实体屏障挡回的执行（key/fenced）不改实体、交还租约，不计入。
	for key, wantRuns := range map[string]int{
		"ok/debit": 1, "ok/credit": 1, "refuse/debit": 1, "refuse/credit": 1, "refuse/refund": 1,
		"dup/debit": 1, "lostreply/debit": 1, "failfirst/debit": 2,
	} {
		if got := probe.counted(key) - probe.counted(key+"/fenced"); got != wantRuns {
			t.Errorf("%s: step body ran %d times not refused by the entity barrier, want %d", key, got, wantRuns)
		}
	}
	for _, key := range []string{"ok/debit", "refuse/debit"} {
		if attempts := probe.attempts(key); len(attempts) != 1 || attempts[0] != 1 {
			t.Errorf("%s: effective attempts %v, want [1] (a barrier refusal hands the lease back and runs again on the same attempt)", key, attempts)
		}
	}
	t.Logf("same-durable, same-entity debits refused by the barrier: ok %d, refuse %d", probe.counted("ok/debit/fenced"), probe.counted("refuse/debit/fenced"))
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

// stepsPerfVariant 是 D12 方向 A 的写法：Steps 按 Mod 声明组装，步骤体拿到 coresaga.NativeStep（满足 stepCompleter）。
// 登记在这里而不是性能用例文件里，是为了让那个文件只用 D12 之前就有的 API、能拷到基线提交上运行 glue 变体。
func init() {
	perfVariants["steps"] = func(ctx context.Context, env *realStepsEnv, group string, steps []perfStep) (func(context.Context) error, error) {
		runtime, err := NewSteps(env.registry, group)
		if err != nil {
			return nil, err
		}
		for _, step := range steps {
			body := step.body
			runtime.Native(step.name, step.topic, func(ctx context.Context, native coresaga.NativeStep) error {
				return body(ctx, native.Command(), native)
			})
		}
		if err := runtime.Start(ctx); err != nil {
			return nil, err
		}
		return runtime.Drain, nil
	}
}

// 崩溃注入（saga 级，投影器合并 checkpoint 的回归）：真实 Nest + DataEngine + saga，跑完正向、业务拒绝 → 补偿、投影
// 停顿超过命令截止被跳过后重试三种 saga，停掉进程，把 WAL 目录的 checkpoint 槽恢复成开跑前的内容——等价于进程在
// 这之后的每次 checkpoint 落盘前崩溃——再用同一个 WAL 目录、同一套库与流重启。启动恢复从开跑前的 checkpoint 起重放
// 全部已投影的原生步骤记录：每条之后都已有同账户的更新版本、操作状态文档已结算、effect 已发布并删除。重放必须没有
// 冲突、不重新判定 lease fence、不重复暂存回执与 effect：账户余额与版本、回执、事务标记（含 skipped）、操作状态
// 文档、saga 终态都与重启前相同，效果流没有新消息。
//
// 运行：source ~/.roost-it/roost-dataengine-it/env.sh，持有 remote-acceptance.lock，
//
//	ROOST_DATAENGINE_IT=1 GOWORK=off go test -tags integration -count=1 -run '^TestRealStepsRestartReplaysUnconfirmedProjectionsExactlyOnce$' ./wiring/saga/
func TestRealStepsRestartReplaysUnconfirmedProjectionsExactlyOnce(t *testing.T) {
	definition := coresaga.Definition{Type: "ckptsaga", Version: 1, Steps: []coresaga.Step{
		{Name: "debit", ForwardTopic: "ckptsaga.debit", CompensateTopic: "ckptsaga.debit.compensate"},
		{Name: "credit", ForwardTopic: "ckptsaga.credit", CompensateTopic: "ckptsaga.credit.compensate"},
	}}
	ensureStepAccountKind()
	names := newRealStepsNames(t)
	overrides := map[string]any{"saga.step_defaults.timeout": 5 * time.Second}
	env := startRealStepsEnvNamed(t, []coresaga.Definition{definition}, true, overrides, names)
	stepAccountDatabase.Store(env.dataDatabase)
	stepAccountHandlerProbe.Store(nil)
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	accounts := map[string]int64{}
	for i, name := range []string{"a", "b", "c", "d", "e"} {
		id, err := entity.BuildEntityID(int64(9_300+i), stepAccountKind)
		if err != nil {
			t.Fatal(err)
		}
		accounts[name] = id
		balance := map[string]int64{"a": 1000, "b": 1000, "c": 990, "d": 1000, "e": 0}[name]
		if _, err := env.mongo.Database(env.dataDatabase).Collection(stepAccountCollection).InsertOne(ctx,
			bson.M{"_id": id, "_version": uint64(1), "_schema": uint32(1), "balance": balance}); err != nil {
			t.Fatal(err)
		}
	}

	// 崩溃点：开跑前的 checkpoint 槽（可能还不存在）。重启前把它恢复回去。
	crashPoint := map[string][]byte{}
	for _, slot := range []string{"ack-0.chk", "ack-1.chk"} {
		raw, err := os.ReadFile(filepath.Join(names.walDir, slot))
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			t.Fatal(err)
		}
		crashPoint[slot] = raw
	}
	// ackGate 关着时投影器停在 ack 上（stall 场景用）。
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

	probe := newStepProbe()
	nestClient := env.nest
	body := func(stepName string) coresaga.NativeStepBody {
		return func(ctx context.Context, step coresaga.NativeStep) error {
			command := step.Command()
			var payload transfer
			if err := json.Unmarshal(command.Payload, &payload); err != nil {
				return err
			}
			account, amount, limit := accounts[payload.From], -payload.Amount, int64(0)
			switch stepName {
			case "refund":
				amount = payload.Amount
			case "credit":
				account, amount = accounts[payload.To], payload.Amount
				if payload.To == "c" {
					limit = 1000
				}
			case "credit-undo":
				account = accounts[payload.To]
			}
			if _, err := nestClient.Request(ctx, stepAccountApply, account, corenest.NewParams(payload.Scenario+"/"+stepName, amount, limit, step)); err != nil {
				return err
			}
			if payload.Scenario == "stall" && stepName == "debit" {
				probe.remember("stall/debit", command)
				probe.signal("stall-committed")
			}
			return nil
		}
	}
	steps, err := NewSteps(env.registry, "ckpt-"+env.token)
	if err != nil {
		t.Fatal(err)
	}
	steps.Native("debit", "ckptsaga.debit", body("debit"))
	steps.Native("refund", "ckptsaga.debit.compensate", body("refund"))
	steps.Native("credit", "ckptsaga.credit", body("credit"))
	steps.Native("credit-undo", "ckptsaga.credit.compensate", body("credit-undo"))
	if err := steps.Start(ctx); err != nil {
		t.Fatal(err)
	}
	start := func(scenario string, n int, from, to string, value int64) string {
		t.Helper()
		id := fmt.Sprintf("ckpt-%s%d-%s", scenario, n, env.token)
		data, err := json.Marshal(transfer{Scenario: scenario, From: from, To: to, Amount: value})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := env.sagaMod.Engine().StartSaga(ctx, coresaga.StartRequest{ID: id, Type: definition.Type, DefinitionVersion: 1, BusinessKey: id, Data: data}); err != nil {
			t.Fatal(err)
		}
		return id
	}
	want := map[string]coresaga.Status{}
	finish := func() {
		t.Helper()
		waitFor(t, ctx, "every saga reaches its terminal state", func() bool {
			for id := range want {
				record, err := env.sagaMod.Store().Get(ctx, id)
				if err != nil || !record.Status.Terminal() {
					return false
				}
				if record.Status != want[id] {
					t.Fatalf("saga %s ended %s (last error %q), want %s", id, record.Status, record.LastError, want[id])
				}
			}
			return true
		})
	}
	// 正向 3 笔（a → b），业务拒绝 2 笔（c 有上限，credit 拒绝 → 补偿 refund）。
	for i := range 3 {
		want[start("ok", i, "a", "b", 10)] = coresaga.StatusCompleted
	}
	for i := range 2 {
		want[start("refuse", i, "a", "c", 20)] = coresaga.StatusCompensated
	}
	finish()
	// 投影停顿超过命令截止：先提交一笔普通事务让投影器停在 ack 上，S 的 debit 准入后等过截止再放行——投影时租约已
	// 过期，整笔记录被跳过（skipped 标记），协调器的第二次尝试生效。
	gate := make(chan struct{})
	ackGate.Store(&gate)
	if _, err := nestClient.Request(ctx, stepAccountApply, accounts["e"], corenest.NewParams("touch", int64(1), int64(0), stepCompleter(nil))); err != nil {
		t.Fatal(err)
	}
	stallID := start("stall", 0, "d", "b", 17)
	want[stallID] = coresaga.StatusCompleted
	select {
	case <-probe.wait("stall-committed"):
	case <-ctx.Done():
		t.Fatal("stall: the debit never committed")
	}
	time.Sleep(time.Until(probe.lastCommand("stall/debit").DeadlineAt) + 500*time.Millisecond)
	openGate()
	finish()

	data := env.mongo.Database(env.dataDatabase)
	wantBalance := map[string]int64{"a": 1000 - 30, "b": 1000 + 30 + 17, "c": 990, "d": 1000 - 17, "e": 1}
	waitFor(t, ctx, "every projection reaches Mongo and every effect leaves the outbox", func() bool {
		for name, id := range accounts {
			var doc struct {
				Balance int64 `bson:"balance"`
			}
			if err := data.Collection(stepAccountCollection).FindOne(ctx, bson.M{"_id": id}, &doc); err != nil || doc.Balance != wantBalance[name] {
				return false
			}
		}
		staged, err := data.Collection("_dataengine_outbox").CountDocuments(ctx, bson.M{})
		return err == nil && staged == 0
	})
	var skipped int64
	if skipped, err = data.Collection("_dataengine_transactions").CountDocuments(ctx, bson.M{"skipped": true}); err != nil || skipped < 1 {
		t.Fatalf("skipped transaction markers=%d err=%v, want the stalled debit", skipped, err)
	}
	snapshot := func(env *realStepsEnv) string {
		t.Helper()
		var out strings.Builder
		for _, collection := range []string{stepAccountCollection, "_dataengine_receipts", "_dataengine_transactions", "_dataengine_step_operations"} {
			var docs []bson.D
			if err := env.mongo.Database(env.dataDatabase).Collection(collection).Find(ctx, bson.M{}, &docs); err != nil {
				t.Fatal(err)
			}
			lines := make([]string, len(docs))
			for i := range docs {
				lines[i] = fmt.Sprint(canonicalBSON(docs[i]))
			}
			slices.Sort(lines)
			fmt.Fprintf(&out, "%s %d\n%s\n", collection, len(docs), strings.Join(lines, "\n"))
		}
		for _, id := range slices.Sorted(maps.Keys(want)) {
			record, err := env.sagaMod.Store().Get(ctx, id)
			if err != nil {
				t.Fatal(err)
			}
			fmt.Fprintf(&out, "saga %s %s phase=%v completed=%d attempt=%d version=%d\n", id, record.Status, record.Phase, record.CompletedSteps, record.Attempt, record.Version)
		}
		return out.String()
	}
	effectStream := "ROOST_D12_EFFECTS_" + names.suffix
	before, effectsBefore := snapshot(env), streamMessages(t, ctx, effectStream)

	drainCtx, drainCancel := context.WithTimeout(ctx, 15*time.Second)
	defer drainCancel()
	if err := steps.Drain(drainCtx); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	env.stopForRestart(t)
	for slot, raw := range crashPoint {
		path := filepath.Join(names.walDir, slot)
		var err error
		if raw == nil {
			err = os.Remove(path)
			if errors.Is(err, os.ErrNotExist) {
				err = nil
			}
		} else {
			err = os.WriteFile(path, raw, 0o600)
		}
		if err != nil {
			t.Fatal(err)
		}
	}
	wal, err := nestwal.Open(nestwal.DefaultOptions(names.walDir))
	if err != nil {
		t.Fatal(err)
	}
	pending := 0
	if err := wal.Replay(ctx, func(corenest.CommitFence, coredata.CommitRecord) error { pending++; return nil }); err != nil {
		t.Fatal(err)
	}
	if err := wal.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// 每笔正向 2 条、拒绝 3 条、stall 3 条（被跳过的第一次 debit + 重试 + credit）、touch 1 条。
	if pending < 3*2+2*3+3+1 {
		t.Fatalf("WAL records after the restored checkpoint=%d, want every saga record", pending)
	}
	t.Logf("crash state: %d WAL records after the last durable checkpoint, %d skipped markers, %d effect messages", pending, skipped, effectsBefore)
	restarted := startRealStepsEnvNamed(t, []coresaga.Definition{definition}, true, overrides, names)
	// 启动恢复（Runtime.Start → Projector.Flush）已在 Start 里排空 WAL。
	projector := restarted.dataMod.Runtime().Projector
	if stats := projector.Stats(); stats.FatalProjectionConflicts != 0 || stats.Projected < uint64(pending) || stats.WALUnacked != 0 {
		t.Fatalf("restart recovery stats=%+v, want %d records replayed without conflicts", stats, pending)
	}
	remaining := 0
	if err := restarted.dataMod.Runtime().WAL.Replay(ctx, func(corenest.CommitFence, coredata.CommitRecord) error { remaining++; return nil }); err != nil || remaining != 0 {
		t.Fatalf("WAL after restart recovery remaining=%d err=%v", remaining, err)
	}
	// 给重启后的 outbox 几个轮询周期：重放若错误地重新暂存了 effect，会在这里被发布。
	time.Sleep(2 * time.Second)
	if staged, err := restarted.mongo.Database(restarted.dataDatabase).Collection("_dataengine_outbox").CountDocuments(ctx, bson.M{}); err != nil || staged != 0 {
		t.Errorf("outbox after restart=%d err=%v, want no re-staged effect", staged, err)
	}
	if after := streamMessages(t, ctx, effectStream); after != effectsBefore {
		t.Errorf("effect stream messages %d → %d across the restart, want no republished effect", effectsBefore, after)
	}
	if after := snapshot(restarted); after != before {
		t.Errorf("state changed across the restart replay:\nbefore:\n%s\nafter:\n%s", before, after)
	}
}

// canonicalBSON 把文档按字段名递归排序，便于前后快照逐字比较（服务端返回的字段顺序不保证稳定）。
func canonicalBSON(value any) any {
	switch typed := value.(type) {
	case bson.D:
		sorted := make(bson.D, len(typed))
		for i, element := range typed {
			sorted[i] = bson.E{Key: element.Key, Value: canonicalBSON(element.Value)}
		}
		slices.SortFunc(sorted, func(left, right bson.E) int { return strings.Compare(left.Key, right.Key) })
		return sorted
	case bson.M:
		sorted := make(bson.D, 0, len(typed))
		for key, element := range typed {
			sorted = append(sorted, bson.E{Key: key, Value: canonicalBSON(element)})
		}
		slices.SortFunc(sorted, func(left, right bson.E) int { return strings.Compare(left.Key, right.Key) })
		return sorted
	case bson.A:
		out := make(bson.A, len(typed))
		for i := range typed {
			out[i] = canonicalBSON(typed[i])
		}
		return out
	default:
		return value
	}
}

func streamMessages(t *testing.T, ctx context.Context, stream string) uint64 {
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
	handle, err := js.Stream(ctx, stream)
	if err != nil {
		t.Fatal(err)
	}
	info, err := handle.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return info.State.Msgs
}
