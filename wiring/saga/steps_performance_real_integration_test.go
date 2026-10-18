//go:build integration

package saga

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	corenest "github.com/tjbdwanghaibo/roost-core/framework/nest"
	coresaga "github.com/tjbdwanghaibo/roost-core/framework/saga"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// perfStep 是负载里的一个原生步骤：名字（durable 后缀）、主题与步骤体。步骤体拿到命令与 stepCompleter，经 Nest 请求
// 余额 handler，handler 在事务里 Complete。
type perfStep struct {
	name, topic string
	body        func(ctx context.Context, command coresaga.Command, step stepCompleter) error
}

// perfVariant 按一种写法组装并订阅原生步骤消费者，返回停止函数（排空在途投递）。
type perfVariant func(ctx context.Context, env *realStepsEnv, group string, steps []perfStep) (stop func(context.Context) error, err error)

// perfVariants 是可选的写法。glue 只用 D12 之前就有的 API；steps（wiring/saga.Steps + saga.NativeStep）在
// steps_real_integration_test.go 里登记——本文件与 real_fixture_integration_test.go 因此可以原样拷到 D12 之前的提交上
// 运行 glue 作基线。
var perfVariants = map[string]perfVariant{"glue": gluePerfVariant}

// gluePerfVariant 是 D12 之前业务手写的组装（与 79d15114 的 game-demo gift_saga.go 相同）：completion 发布器、原生
// 收件箱（租约 2 分钟）、每步一个 SubscribeDataEngineStep，StepHandler 从 ctx 取 Reservation，Complete = Bind +
// EmitCompletion（旧 demo 的 gift.NativeStep）。
func gluePerfVariant(ctx context.Context, env *realStepsEnv, group string, steps []perfStep) (func(context.Context) error, error) {
	jetstream := app.MustLookup[fnats.IJetStream](env.registry, mods.ModNatsJetStream)
	prefix, stream, _, err := StreamSettings(env.registry.Config())
	if err != nil {
		return nil, err
	}
	transport, err := coresaga.NewJetStreamPublisher(jetstream, prefix)
	if err != nil {
		return nil, err
	}
	inbox, err := coresaga.NewDataEngineStepInbox(env.mongo, env.dataDatabase, coresaga.DataEngineStepInboxOptions{Owner: group + "-" + strconv.Itoa(os.Getpid()), LeaseDuration: 2 * time.Minute})
	if err != nil {
		return nil, err
	}
	var subs []fnats.IJetStreamSubscription
	for _, step := range steps {
		body := step.body
		handler := func(ctx context.Context, command coresaga.Command) (coresaga.Completion, error) {
			reservation, ok := coresaga.ReservationFromContext(ctx)
			if !ok {
				return coresaga.Completion{}, errors.New("glue: no reservation on the context")
			}
			return coresaga.Completion{}, body(ctx, command, glueStep{inbox: inbox, command: command, reservation: reservation})
		}
		sub, err := coresaga.SubscribeDataEngineStep(ctx, jetstream, transport, inbox, coresaga.StepConsumerConfig{Stream: stream, Durable: group + "-" + step.name, Topic: step.topic}, handler)
		if err != nil {
			for _, started := range subs {
				started.Stop()
			}
			return nil, err
		}
		subs = append(subs, sub)
	}
	return func(ctx context.Context) error {
		for _, sub := range subs {
			sub.Drain()
		}
		for _, sub := range subs {
			select {
			case <-sub.Closed():
			case <-ctx.Done():
				return ctx.Err()
			}
		}
		return nil
	}, nil
}

// glueStep 是旧 demo 的 gift.NativeStep：在当前 Nest 事务里绑定回执并发出 completion。
type glueStep struct {
	inbox       *coresaga.DataEngineStepInbox
	command     coresaga.Command
	reservation coresaga.Reservation
}

func (step glueStep) Complete(success bool, reason string) error {
	if err := step.inbox.Bind(step.command, step.reservation); err != nil {
		return err
	}
	completion := coresaga.Completion{CommandID: step.command.ID, IdempotencyKey: step.command.IdempotencyKey, SagaID: step.command.SagaID, Success: success}
	if !success {
		completion.Error = reason
		if completion.Error == "" {
			completion.Error = "glue: step refused"
		}
	}
	return coresaga.EmitCompletion(completion)
}

// TestRealNativeStepsPerformance 测原生步骤经真实 Nest 调度 + 真实 DataEngine 的负载（D12 验证项 3 经过新 API 的对照）。
// 只在显式入口运行：
//
//	ROOST_STEPS_PERF_OUTPUT=<json> ROOST_STEPS_PERF_VARIANT=steps|glue ROOST_STEPS_PERF_MODE=forward|compensate \
//	ROOST_STEPS_PERF_RATE=20 ROOST_STEPS_PERF_DURATION=5m ROOST_DATAENGINE_IT=1 \
//	GOWORK=off go test -tags integration -count=1 -timeout 0 -run '^TestRealNativeStepsPerformance$' ./wiring/saga/
//
// 一笔 saga 两个原生步骤：debit（from −1）→ credit（to +1）；compensate 模式下 to 账户都在上限，credit 业务拒绝，
// 协调器补偿 debit（refund，from +1）。账户 64 对轮转。配置取生产缺省（saga / outbox 轮询 100ms、步骤超时 5s、
// 最多 5 次尝试、退避 100ms..5s），三副本流。
//
// 延迟口径与 framework/saga TestRealSagaPerformance 相同：P50～Max 从计划启动时刻到轮询（50ms）读到持久终态，含压测端
// 等待；Server 是协调器记录的 updated_at − created_at（终态那次写入的时刻减去创建时刻，Mongo 毫秒精度）。核对：
// 全部终态正确、每个账户余额恰好按完成数变化（每个操作实例恰好生效一次）。
func TestRealNativeStepsPerformance(t *testing.T) {
	output := os.Getenv("ROOST_STEPS_PERF_OUTPUT")
	if output == "" {
		t.Skip("set ROOST_STEPS_PERF_OUTPUT (see the comment)")
	}
	variantName := os.Getenv("ROOST_STEPS_PERF_VARIANT")
	variant, ok := perfVariants[variantName]
	if !ok {
		t.Fatalf("ROOST_STEPS_PERF_VARIANT=%q: this checkout has %v", variantName, slices.Sorted(func(yield func(string) bool) {
			for name := range perfVariants {
				if !yield(name) {
					return
				}
			}
		}))
	}
	mode := os.Getenv("ROOST_STEPS_PERF_MODE")
	rate, _ := strconv.Atoi(os.Getenv("ROOST_STEPS_PERF_RATE"))
	duration, _ := time.ParseDuration(os.Getenv("ROOST_STEPS_PERF_DURATION"))
	if (mode != "forward" && mode != "compensate") || rate < 1 || rate > 500 || duration < time.Second || duration > 30*time.Minute || duration%time.Second != 0 {
		t.Fatal("invalid parameters: ROOST_STEPS_PERF_MODE forward|compensate, ROOST_STEPS_PERF_RATE 1..500, ROOST_STEPS_PERF_DURATION whole seconds up to 30m")
	}
	definition := coresaga.Definition{Type: "d12perf", Version: 1, Steps: []coresaga.Step{
		{Name: "debit", ForwardTopic: "d12perf.debit", CompensateTopic: "d12perf.debit.compensate"},
		{Name: "credit", ForwardTopic: "d12perf.credit", CompensateTopic: "d12perf.credit.compensate"},
	}}
	ensureStepAccountKind()
	env := startRealStepsEnv(t, []coresaga.Definition{definition}, true, map[string]any{
		"saga.poll_interval": 100 * time.Millisecond, "dataengine.outbox.poll_interval": 100 * time.Millisecond,
		"dataengine.outbox.retry_min": time.Second, "dataengine.outbox.retry_max": time.Minute,
		"saga.step_defaults.timeout": 5 * time.Second, "saga.step_defaults.max_attempts": 5,
		"saga.step_defaults.backoff_min": 100 * time.Millisecond, "saga.step_defaults.backoff_max": 5 * time.Second,
	})
	stepAccountDatabase.Store(env.dataDatabase)
	stepAccountHandlerProbe.Store(nil)
	ctx, cancel := context.WithTimeout(context.Background(), duration+3*time.Minute)
	defer cancel()

	const pairs = 64
	const balance, limit = int64(1_000_000), int64(1_000_000)
	accounts := make(map[string]int64, 2*pairs)
	initial := make(map[string]int64, 2*pairs)
	docs := make([]any, 0, 2*pairs)
	for i := range pairs {
		for _, side := range []string{"f", "t"} {
			name := fmt.Sprintf("%s%d", side, i)
			unique := int64(20_000 + 2*i)
			if side == "t" {
				unique++
			}
			id, err := entity.BuildEntityID(unique, stepAccountKind)
			if err != nil {
				t.Fatal(err)
			}
			accounts[name], initial[name] = id, balance
			docs = append(docs, bson.M{"_id": id, "_version": uint64(1), "_schema": uint32(1), "balance": balance})
		}
	}
	collection := env.mongo.Database(env.dataDatabase).Collection(stepAccountCollection)
	for _, doc := range docs {
		if _, err := collection.InsertOne(ctx, doc); err != nil {
			t.Fatal(err)
		}
	}
	nestClient := env.nest
	// timing 记下每步 Nest 请求的耗时与相邻两次步骤体开始的间隔（同一 durable 在一个进程里逐条处理，满载时间隔就是
	// 一条命令占住消费者的时间：Reserve + Nest 提交 + 等回执投影）。
	var timingMu sync.Mutex
	nestTimes, gaps := map[string][]time.Duration{}, map[string][]time.Duration{}
	lastStart := map[string]time.Time{}
	bodyErrors := map[string]int{}
	apply := func(ctx context.Context, name string, account int64, delta, cap int64, step stepCompleter) error {
		begin := time.Now()
		timingMu.Lock()
		if previous, ok := lastStart[name]; ok {
			gaps[name] = append(gaps[name], begin.Sub(previous))
		}
		lastStart[name] = begin
		timingMu.Unlock()
		_, err := nestClient.Request(ctx, stepAccountApply, account, corenest.NewParams("perf", delta, cap, step))
		timingMu.Lock()
		nestTimes[name] = append(nestTimes[name], time.Since(begin))
		if err != nil {
			reason := err.Error()
			if errors.Is(err, coredata.ErrFencedEntityPending) {
				reason = "fenced_entity_pending"
			} else if len(reason) > 120 {
				reason = reason[:120]
			}
			bodyErrors[name+": "+reason]++
		}
		timingMu.Unlock()
		return err
	}
	decode := func(command coresaga.Command) (transfer, error) {
		var payload transfer
		err := json.Unmarshal(command.Payload, &payload)
		return payload, err
	}
	creditCap := int64(0)
	if mode == "compensate" {
		creditCap = limit
	}
	perfSteps := []perfStep{
		{"debit", "d12perf.debit", func(ctx context.Context, command coresaga.Command, step stepCompleter) error {
			p, err := decode(command)
			if err != nil {
				return err
			}
			return apply(ctx, "debit", accounts[p.From], -p.Amount, 0, step)
		}},
		{"refund", "d12perf.debit.compensate", func(ctx context.Context, command coresaga.Command, step stepCompleter) error {
			p, err := decode(command)
			if err != nil {
				return err
			}
			return apply(ctx, "refund", accounts[p.From], p.Amount, 0, step)
		}},
		{"credit", "d12perf.credit", func(ctx context.Context, command coresaga.Command, step stepCompleter) error {
			p, err := decode(command)
			if err != nil {
				return err
			}
			return apply(ctx, "credit", accounts[p.To], p.Amount, creditCap, step)
		}},
		{"credit-undo", "d12perf.credit.compensate", func(ctx context.Context, command coresaga.Command, step stepCompleter) error {
			p, err := decode(command)
			if err != nil {
				return err
			}
			return apply(ctx, "credit-undo", accounts[p.To], -p.Amount, 0, step)
		}},
	}
	stop, err := variant(ctx, env, "d12p-"+env.token, perfSteps)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer stopCancel()
		if err := stop(stopCtx); err != nil {
			t.Errorf("stop consumers: %v", err)
		}
	})

	type job struct {
		index   int
		planned time.Time
	}
	type startedSaga struct{ planned, called time.Time }
	type minuteSlice struct {
		Minute, Count       int
		P50MS, P99MS, MaxMS float64
		ServerP99MS         float64
	}
	type result struct {
		Variant, Mode                                    string
		Rate, Planned, Started, Completed, WrongTerminal int
		StartErrors                                      uint64
		FirstError                                       string
		DurationSeconds, ElapsedSeconds, CompletionTPS   float64
		P50MS, P95MS, P99MS, MaxMS                       float64
		Server                                           sagaLatencyMS
		StartCall                                        sagaLatencyMS
		BusinessVerified                                 bool
		Minutes                                          []minuteSlice
		// NestRequest：步骤体里 Nest 请求的耗时（事务 + strict WAL）；BodyGap：同一步骤相邻两次步骤体开始的间隔。
		NestRequest, BodyGap map[string]sagaLatencyMS
		// BodyErrors：Nest 请求失败按步骤与原因计数（fenced_entity_pending 是实体屏障）。
		BodyErrors map[string]int
		// ProjectionBacklogMax：轮询时看到的 Committed − Projected 最大值（已准入 WAL、还没投影的记录数）。
		ProjectionBacklogMax uint64
		FinalProjector       engine.ProjectorStats
		// Statuses 是轮询第一次读到的终态计数；Mismatches 是核对失败时余额不符的账户（最多 10 个，want / got）。
		Statuses   map[string]int
		Mismatches []string
	}
	total := int(duration/time.Second) * rate
	r := result{Variant: variantName, Mode: mode, Rate: rate, Planned: total, DurationSeconds: duration.Seconds()}
	var mu sync.Mutex
	pending := make(map[string]startedSaga)
	var startDurations []time.Duration
	var started atomic.Int64
	var failures atomic.Uint64
	var finished atomic.Bool
	jobs := make(chan job, 64)
	producerDone := make(chan struct{})
	defer func() { cancel(); <-producerDone }()
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for j := range jobs {
				id := fmt.Sprintf("d12p-%d", j.index)
				data, _ := json.Marshal(transfer{Scenario: "perf", From: fmt.Sprintf("f%d", j.index%pairs), To: fmt.Sprintf("t%d", j.index%pairs), Amount: 1})
				called := time.Now()
				_, err := env.sagaMod.Engine().StartSaga(ctx, coresaga.StartRequest{ID: id, Type: definition.Type, DefinitionVersion: 1, BusinessKey: id, Data: data, DeadlineAt: j.planned.Add(30 * time.Second)})
				returned := time.Now()
				mu.Lock()
				if err != nil {
					failures.Add(1)
					if r.FirstError == "" {
						r.FirstError = err.Error()
					}
				} else {
					pending[id] = startedSaga{planned: j.planned, called: called}
					startDurations = append(startDurations, returned.Sub(called))
					started.Add(1)
				}
				mu.Unlock()
			}
		})
	}
	begin := time.Now()
	go func() {
		defer func() { close(jobs); workers.Wait(); finished.Store(true); close(producerDone) }()
		for i := range total {
			planned := begin.Add(time.Duration(i) * time.Second / time.Duration(rate))
			timer := time.NewTimer(max(time.Until(planned), 0))
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return
			}
			select {
			case jobs <- job{i, planned}:
			case <-ctx.Done():
				return
			}
		}
	}()
	terminal := coresaga.StatusCompleted
	if mode == "compensate" {
		terminal = coresaga.StatusCompensated
	}
	var latencies, server []time.Duration
	byMinute := make([][]time.Duration, (total+rate*60-1)/(rate*60))
	serverByMinute := make([][]time.Duration, len(byMinute))
	statuses := make(map[string]coresaga.Status, total)
	sagas := env.mongo.Database(env.sagaDatabase).Collection("_sagas")
	projector := env.dataMod.Runtime().Projector
	for {
		mu.Lock()
		ids := make([]string, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		mu.Unlock()
		if len(ids) > 0 {
			var found []struct {
				ID        string          `bson:"_id"`
				Status    coresaga.Status `bson:"status"`
				CreatedAt time.Time       `bson:"created_at"`
				UpdatedAt time.Time       `bson:"updated_at"`
			}
			if err := sagas.Find(ctx, bson.M{"_id": bson.M{"$in": ids}, "status": bson.M{"$in": []coresaga.Status{coresaga.StatusCompleted, coresaga.StatusCompensated, coresaga.StatusFailed, coresaga.StatusManualRequired}}}, &found); err != nil {
				t.Fatal(err)
			}
			observed := time.Now()
			mu.Lock()
			for _, doc := range found {
				s := pending[doc.ID]
				latency := observed.Sub(s.planned)
				latencies = append(latencies, latency)
				minute := int(s.planned.Sub(begin) / time.Minute)
				byMinute[minute] = append(byMinute[minute], latency)
				serverDone := doc.UpdatedAt.Sub(doc.CreatedAt)
				server = append(server, serverDone)
				serverByMinute[minute] = append(serverByMinute[minute], serverDone)
				delete(pending, doc.ID)
				statuses[doc.ID] = doc.Status
				r.Completed++
				if doc.Status != terminal {
					r.WrongTerminal++
				}
			}
			mu.Unlock()
		}
		if stats := projector.Stats(); stats.Committed > stats.Projected && stats.Committed-stats.Projected > r.ProjectionBacklogMax {
			r.ProjectionBacklogMax = stats.Committed - stats.Projected
		}
		mu.Lock()
		remaining := len(pending)
		mu.Unlock()
		if finished.Load() && remaining == 0 {
			break
		}
		select {
		case <-time.After(50 * time.Millisecond):
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	r.ElapsedSeconds = time.Since(begin).Seconds()
	r.Started, r.StartErrors = int(started.Load()), failures.Load()
	r.CompletionTPS = float64(r.Completed) / r.ElapsedSeconds
	slices.Sort(latencies)
	r.P50MS, r.P95MS, r.P99MS, r.MaxMS = quantileMS(latencies, .5), quantileMS(latencies, .95), quantileMS(latencies, .99), quantileMS(latencies, 1)
	r.Server, r.StartCall = summarizeMS(server), summarizeMS(startDurations)
	timingMu.Lock()
	r.NestRequest, r.BodyGap = map[string]sagaLatencyMS{}, map[string]sagaLatencyMS{}
	for name, values := range nestTimes {
		r.NestRequest[name] = summarizeMS(values)
	}
	for name, values := range gaps {
		r.BodyGap[name] = summarizeMS(values)
	}
	r.BodyErrors = bodyErrors
	r.FinalProjector = projector.Stats()
	timingMu.Unlock()
	for minute, slice := range byMinute {
		slices.Sort(slice)
		serverSlice := serverByMinute[minute]
		slices.Sort(serverSlice)
		r.Minutes = append(r.Minutes, minuteSlice{Minute: minute, Count: len(slice), P50MS: quantileMS(slice, .5), P99MS: quantileMS(slice, .99), MaxMS: quantileMS(slice, 1), ServerP99MS: quantileMS(serverSlice, .99)})
		t.Logf("minute=%d count=%d p50=%.1fms p99=%.1fms max=%.1fms server_p99=%.1fms", minute, len(slice), quantileMS(slice, .5), quantileMS(slice, .99), quantileMS(slice, 1), quantileMS(serverSlice, .99))
	}
	// 每个账户恰好按终态变化：completed 的一笔 f<i> 减 1、t<i> 加 1；compensated / failed 没有净变化（补偿撤销了已生效
	// 的步骤，或没有步骤生效）；manual_required 无法断定，计为核对失败。过载时终态可能不是期望的那个，这里仍逐账户
	// 核对每个操作实例恰好生效一次。
	expected := make(map[string]int64, len(initial))
	for name, value := range initial {
		expected[name] = value
	}
	manual := false
	for i := range total {
		switch statuses[fmt.Sprintf("d12p-%d", i)] {
		case coresaga.StatusCompleted:
			expected[fmt.Sprintf("f%d", i%pairs)]--
			expected[fmt.Sprintf("t%d", i%pairs)]++
		case coresaga.StatusManualRequired:
			manual = true
		}
	}
	var mismatches []string
	verify := func() bool {
		mismatches = mismatches[:0]
		var stored []struct {
			ID      int64 `bson:"_id"`
			Balance int64 `bson:"balance"`
		}
		if err := collection.Find(ctx, bson.M{}, &stored); err != nil || len(stored) != len(accounts) {
			return false
		}
		byID := make(map[int64]int64, len(stored))
		for _, doc := range stored {
			byID[doc.ID] = doc.Balance
		}
		for name, id := range accounts {
			if byID[id] != expected[name] && len(mismatches) < 10 {
				mismatches = append(mismatches, fmt.Sprintf("%s want %d got %d", name, expected[name], byID[id]))
			}
		}
		return len(mismatches) == 0
	}
	// 过载时被放弃的 saga 可能因迟到的成功被重新打开补偿（failed → compensating → compensated），给它们两分钟收尾。
	for deadline := time.Now().Add(2 * time.Minute); !manual && !r.BusinessVerified && time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		r.BusinessVerified = verify()
	}
	r.Mismatches = mismatches
	r.Statuses = map[string]int{}
	for _, status := range statuses {
		r.Statuses[strconv.Itoa(int(status))]++
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(output, append(raw, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Logf("variant=%s mode=%s completed=%d/%d rate=%.2f/s p50=%.1fms p95=%.1fms p99=%.1fms max=%.1fms server_p99=%.1fms start_errors=%d business_verified=%v",
		variantName, mode, r.Completed, total, r.CompletionTPS, r.P50MS, r.P95MS, r.P99MS, r.MaxMS, r.Server.P99MS, r.StartErrors, r.BusinessVerified)
	if r.Started != total || r.Completed != total || r.StartErrors != 0 || r.WrongTerminal != 0 || !r.BusinessVerified {
		t.Fatal("native step load verification failed; result retained")
	}
}

// sagaLatencyMS 是一段时间的分位（毫秒）。
type sagaLatencyMS struct {
	Samples                    int
	P50MS, P95MS, P99MS, MaxMS float64
}

func summarizeMS(values []time.Duration) sagaLatencyMS {
	sorted := slices.Clone(values)
	slices.Sort(sorted)
	return sagaLatencyMS{Samples: len(sorted), P50MS: quantileMS(sorted, .5), P95MS: quantileMS(sorted, .95), P99MS: quantileMS(sorted, .99), MaxMS: quantileMS(sorted, 1)}
}

func quantileMS(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	return float64(sorted[int(math.Ceil(float64(len(sorted))*p))-1]) / float64(time.Millisecond)
}
