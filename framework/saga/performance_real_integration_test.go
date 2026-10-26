//go:build integration

package saga

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
	"go.mongodb.org/mongo-driver/v2/bson"
	mongodriver "go.mongodb.org/mongo-driver/v2/mongo"
	mongooptions "go.mongodb.org/mongo-driver/v2/mongo/options"
)

// TestRealSagaPerformance 只在显式入口运行。测正式协调器、真实 Mongo 步骤和 JetStream，
// 不将微基准、Saga、Remote 和 Nest 普通业务混成同一个 TPS，也不修改生产持久级别。
func TestRealSagaPerformance(t *testing.T) {
	output := os.Getenv("ROOST_SAGA_PERF_OUTPUT")
	if output == "" {
		t.Skip("run scripts/perf/saga.sh")
	}
	mode := os.Getenv("ROOST_SAGA_PERF_MODE")
	if mode == "" {
		mode = "forward"
	}
	rate := 20
	if v := os.Getenv("ROOST_SAGA_PERF_RATE"); v != "" {
		var err error
		rate, err = strconv.Atoi(v)
		if err != nil {
			t.Fatal(err)
		}
	}
	duration := time.Minute
	if v := os.Getenv("ROOST_SAGA_PERF_DURATION"); v != "" {
		var err error
		duration, err = time.ParseDuration(v)
		if err != nil {
			t.Fatal(err)
		}
	}
	// ROOST_SAGA_PERF_CONCURRENCY 选闭环：N 个发起者各自“启动一笔 → 等到持久终态 → 再启动下一笔”，到 duration 停止
	// 发起并排空在途。测的是给定并发度下能完成多少笔，用来找饱和点；不设时保持原来的开环固定速率。
	concurrency := 0
	if v := os.Getenv("ROOST_SAGA_PERF_CONCURRENCY"); v != "" {
		var err error
		concurrency, err = strconv.Atoi(v)
		if err != nil || concurrency < 1 || concurrency > 512 {
			t.Fatal("concurrency must be between 1 and 512")
		}
		rate = 0
	}
	if (concurrency == 0 && (rate < 1 || rate > 1000)) || duration < time.Second || duration > 10*time.Minute || duration%time.Second != 0 || (mode != "forward" && mode != "compensate") {
		t.Fatal("invalid Saga performance parameters")
	}
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if natsURL == "" || os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI") == "" {
		t.Fatal("private Mongo/NATS environment required")
	}
	if os.Getenv("ROOST_SAGA_PERF_HOPS") == "1" {
		recorder := &sagaCommandTimings{values: make(map[string]sagaCommandTiming), marks: make(map[string]time.Time), epoch: time.Now()}
		hoptrace.Install(recorder)
		t.Cleanup(func() {
			hoptrace.Install(nil)
			recorder.mu.Lock()
			defer recorder.mu.Unlock()
			timeline, timelineErr := json.Marshal(struct {
				Events  []sagaTimelineEvent
				Dropped int
			}{recorder.events, recorder.dropped})
			if timelineErr == nil {
				timelineErr = os.WriteFile(output+".timeline.json", timeline, 0600)
			}
			if timelineErr != nil {
				t.Error(timelineErr)
			}
			raw, err := json.MarshalIndent(recorder.values, "", "  ")
			if err == nil {
				err = os.WriteFile(output+".hops.json", append(raw, '\n'), 0600)
			}
			if err != nil {
				t.Error(err)
			}
		})
	}
	client, database := realRevn06s5Mongo(t)
	// 最先登记，晚于下面的消费者和引擎停止执行、早于 realRevn06s5Mongo 清库执行。
	// 即使主测试提前失败，也保留实际终态、步骤回执和效果，不能只留下聚合计数。
	t.Cleanup(func() { retainSagaPerformanceEvidence(t, client, database, output) })
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	stream, prefix := perfStreamPrefix(t, "ROOST_PERF_SAGA")+"_"+suffix, "perfsaga"+strings.ReplaceAll(suffix, "_", "x")
	t.Cleanup(func() { deleteRevn06s5Stream(t, natsURL, stream) })
	nc, err := ndriver.NewClient(fnats.DefaultConfig(natsURL), ndriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { nc.Close() })
	js, err := ndriver.NewJetStreamClient(nc)
	if err != nil {
		t.Fatal(err)
	}
	def := Definition{Type: "perf", Version: 1, Steps: []Step{
		{Name: "reserve", ForwardTopic: prefix + ".reserve", CompensateTopic: prefix + ".reserve.cancel"},
		{Name: "ship", ForwardTopic: prefix + ".ship", CompensateTopic: prefix + ".ship.cancel"},
	}}
	consumerWorkers := 1
	if value := os.Getenv("ROOST_SAGA_PERF_WORKERS"); value != "" {
		var err error
		consumerWorkers, err = strconv.Atoi(value)
		if err != nil || consumerWorkers < 1 || consumerWorkers > 32 {
			t.Fatal("workers must be between 1 and 32")
		}
	}
	cfg := crossProcessAssembly(database, stream, prefix, "perf")
	cfg.Engine = DefaultOptions()
	if value := os.Getenv("ROOST_SAGA_PERF_COORDINATOR_BATCH"); value != "" {
		batch, parseErr := strconv.Atoi(value)
		if parseErr != nil || batch < 1 || batch > 3 {
			t.Fatal("coordinator batch must be between 1 and 3 for this comparison")
		}
		cfg.Engine.CoordinatorBatch = batch
	}
	cfg.Completions.Workers = consumerWorkers
	cfg.Engine.Owner = "perf"
	cfg.Stream.Replicas = 3 // 容量测量保留三副本发布确认，不借故障夹具的单副本降低持久成本。
	asm, err := Assemble(client, js, cfg, def)
	if err != nil {
		t.Fatal(err)
	}
	// 只包一层观察，不换 Assemble 的正式 Store；必须在 Start 之前替换，此后引擎只经 e.store 写记录。
	terminals := &terminalClock{Store: asm.Engine.store, at: make(map[string]time.Time), waiters: make(map[string]chan struct{})}
	asm.Engine.store = terminals
	ctx, cancel := context.WithTimeout(context.Background(), duration+2*time.Minute)
	defer cancel()
	if err = asm.Start(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		stop, c := context.WithTimeout(context.Background(), 20*time.Second)
		defer c()
		if err := asm.Stop(stop); err != nil {
			t.Error(err)
		}
	})
	inbox, err := NewMongoCommandInbox(client, database, "perf_steps", CommandInboxOptions{ReceiptTTL: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	business, effects := client.Database(database).Collection("business"), client.Database(database).Collection("effects")
	// 预建集合，避免首次并发事务的建表开销进入普通步骤测量。
	for _, coll := range []fmongo.ICollection{business, effects} {
		if _, err := coll.InsertOne(ctx, bson.M{"_id": "seed"}); err != nil {
			t.Fatal(err)
		}
		if _, err := coll.DeleteOne(ctx, bson.M{"_id": "seed"}); err != nil {
			t.Fatal(err)
		}
	}
	handler := func(tx context.Context, command Command) (Completion, error) {
		if mode == "compensate" && command.Phase == PhaseForward && command.StepName == "ship" {
			return Completion{Success: false, Error: "planned business refusal"}, nil
		}
		amount := 1
		if command.Phase == PhaseCompensate {
			amount = -1
		}
		var doc bson.M
		upsert := fmongo.FindOneAndUpdateOption{Upsert: true, ReturnAfter: true}
		if err := business.FindOneAndUpdate(tx, bson.M{"_id": command.SagaID}, bson.M{"$inc": bson.M{"value": amount}}, &doc, upsert); err != nil {
			return Completion{}, err
		}
		if err := effects.FindOneAndUpdate(tx, bson.M{"_id": command.IdempotencyKey}, bson.M{"$inc": bson.M{"commits": 1}}, &doc, upsert); err != nil {
			return Completion{}, err
		}
		return Completion{Success: true}, nil
	}
	stepConsumers := 1
	if value := os.Getenv("ROOST_SAGA_PERF_STEP_CONSUMERS"); value != "" {
		var err error
		stepConsumers, err = strconv.Atoi(value)
		if err != nil || stepConsumers < 1 || stepConsumers > 32 {
			t.Fatal("step consumers must be between 1 and 32")
		}
	}
	// 同一 durable 的多个正式订阅共享消费进度，模拟多个服务实例；不复制步骤或改重试预算。
	for range stepConsumers {
		for _, step := range def.Steps {
			for _, topic := range []string{step.ForwardTopic, step.CompensateTopic} {
				sub, err := SubscribeMongoStep(ctx, js, asm.Transport, inbox, StepConsumerConfig{MongoWorkers: consumerWorkers, Stream: stream, Durable: strings.ReplaceAll(topic, ".", "-"), Topic: topic}, handler)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(sub.Drain)
			}
		}
	}
	type job struct {
		index   int
		planned time.Time
	}
	// started 是一个已成功启动、尚未观察到终态的 saga 的压测端时刻。
	type startedSaga struct {
		planned, called time.Time
	}
	// observation 留到最后再与 terminals 对照：轮询读到终态时，同一次 Apply 可能还没返回到包装层。
	type observation struct {
		id                        string
		planned, called, observed time.Time
	}
	// 按计划启动时刻所在的分钟分段统计，区分偶发尖峰与随运行时间逐渐变慢。ServerP99MS 是同一分段的服务端口径。
	type minuteSlice struct {
		Minute, Count       int
		P50MS, P99MS, MaxMS float64
		ServerP99MS         float64
	}
	type result struct {
		Mode                                                      string
		Rate, Concurrency, Planned, Started, Completed            int
		WrongTerminal                                             int
		ConsumerWorkers, StepConsumers                            int
		CoordinatorBatch                                          int
		DiagnosticHops                                            bool
		StartErrors                                               uint64
		FirstError                                                string
		DurationSeconds, ElapsedSeconds, CompletionTPS            float64
		ExpectedTerminalTPS                                       float64
		P50MS, P95MS, P99MS, MaxMS                                float64
		FinalStats                                                Stats
		OutboxPending, Leases, BusinessDocuments, EffectDocuments int64
		BusinessVerified                                          bool
		Minutes                                                   []minuteSlice
		// Steady* 只统计稳态窗口 [warmup, duration) 内到达期望终态的笔数（按服务端终态时刻），排除爬升与末尾排空；
		// 闭环档以 SteadyTPS 作为该并发度的成功吞吐。ProcessCPUCores 是同一进程整段运行的 CPU 用量（核数）。
		SteadyWindowSeconds, SteadyTPS float64
		SteadyCompleted                int
		ProcessCPUCores                float64
		Statuses                       map[string]int
		// P50MS～MaxMS 从计划启动时刻算到轮询观察到持久终态，混有压测端等待，保留它们与历史样本同口径比较。
		// Harness 与 Server 把同一段时间拆开（同一进程单调时钟）：
		//
		//	planned ─Harness.AdmitLag─▶ called ─Server.Start─▶ StartSaga 返回
		//	called ──────────── Server.Completion ────────────▶ terminal ─Harness.ObserveLag─▶ observed
		//
		// terminal 是终态那次 Store.Apply 提交成功返回的时刻（terminalClock），评估框架延迟看 Server.Completion。
		Harness sagaHarness
		Server  sagaServer
		// MongoCommands 是压测窗口（开始发起 → 排空并核对完）内 Mongo 数据面命令的次数差值；HarnessMongoCommands 是其中
		// 夹具自己发的（终态轮询、排空计数、核对读），不属于被测链路。
		MongoCommands        map[string]int64
		HarnessMongoCommands int64
		// FinalStatuses 是结束后按 ID 逐笔复读的最终状态（Statuses 只记轮询第一次读到的终态，重开的 saga 之后会变）；
		// FinalWrongTerminal 是最终不是期望终态的笔数。
		FinalStatuses      map[string]int
		FinalWrongTerminal int
	}
	total := int(duration/time.Second) * rate
	r := result{CoordinatorBatch: cfg.Engine.CoordinatorBatch, Mode: mode, Rate: rate, Concurrency: concurrency, Planned: total, DurationSeconds: duration.Seconds(), ConsumerWorkers: consumerWorkers, StepConsumers: stepConsumers, DiagnosticHops: os.Getenv("ROOST_SAGA_PERF_HOPS") == "1"}
	var mu sync.Mutex
	pending := make(map[string]startedSaga)
	var observations []observation
	var startDurations []time.Duration
	var started atomic.Int64
	var failures atomic.Uint64
	var finished atomic.Bool
	jobs := make(chan job, 64)
	producerDone := make(chan struct{})
	// 断言失败也先撤销输入并等待提交协程，避免清库与在途事务竞争。
	defer func() { cancel(); <-producerDone }()
	var workers sync.WaitGroup
	for range 8 {
		workers.Go(func() {
			for j := range jobs {
				id := fmt.Sprintf("perf-%d", j.index)
				called := time.Now()
				_, err := asm.Engine.StartSaga(ctx, StartRequest{ID: id, Type: def.Type, DefinitionVersion: 1, BusinessKey: id, DeadlineAt: j.planned.Add(30 * time.Second)})
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
	mongoURI := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	commandsBefore := perfMongoCommands(ctx, mongoURI)
	var harnessMongo atomic.Int64
	begin := time.Now()
	cpuBegin := processCPU()
	if concurrency > 0 {
		// 闭环：每个发起者拿下一个编号后自己调用 StartSaga，等 terminalClock（或轮询兜底）报终态再继续。
		// 超过 duration 不再取编号，因此编号连续、全部已启动；8 个开环提交协程照常存在但收不到任务。
		var next atomic.Int64
		go func() {
			defer func() { close(jobs); workers.Wait(); finished.Store(true); close(producerDone) }()
			var initiators sync.WaitGroup
			for range concurrency {
				initiators.Go(func() {
					for ctx.Err() == nil && time.Since(begin) < duration {
						i := int(next.Add(1) - 1)
						id := fmt.Sprintf("perf-%d", i)
						done := terminals.wait(id)
						called := time.Now()
						_, err := asm.Engine.StartSaga(ctx, StartRequest{ID: id, Type: def.Type, DefinitionVersion: 1, BusinessKey: id, DeadlineAt: called.Add(30 * time.Second)})
						returned := time.Now()
						mu.Lock()
						if err != nil {
							failures.Add(1)
							if r.FirstError == "" {
								r.FirstError = err.Error()
							}
						} else {
							pending[id] = startedSaga{planned: called, called: called}
							startDurations = append(startDurations, returned.Sub(called))
							started.Add(1)
						}
						mu.Unlock()
						if err != nil {
							terminals.release(id)
							continue
						}
						select {
						case <-done:
						case <-ctx.Done():
							return
						}
					}
				})
			}
			initiators.Wait()
			total = int(next.Load())
		}()
	} else {
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
	}
	var latencies []time.Duration
	byMinute := make([][]time.Duration, int((duration+time.Minute-1)/time.Minute))
	statuses := make(map[Status]int)
	terminal := StatusCompleted
	if mode == "compensate" {
		terminal = StatusCompensated
	}
	sagas := client.Database(database).Collection(defaultSagaCollection)
	for {
		mu.Lock()
		ids := make([]string, 0, len(pending))
		for id := range pending {
			ids = append(ids, id)
		}
		mu.Unlock()
		if len(ids) > 0 {
			var docs []struct {
				ID     string `bson:"_id"`
				Status Status `bson:"status"`
			}
			harnessMongo.Add(1)
			if err := sagas.Find(ctx, bson.M{"_id": bson.M{"$in": ids}, "status": bson.M{"$in": []Status{StatusCompleted, StatusCompensated, StatusFailed, StatusManualRequired}}}, &docs); err != nil {
				t.Fatal(err)
			}
			observed := time.Now() // 完成计时等到可读取持久终态；包含最多约50ms的观察间隔。
			mu.Lock()
			for _, doc := range docs {
				s := pending[doc.ID]
				latency := observed.Sub(s.planned)
				latencies = append(latencies, latency)
				minute := int(s.planned.Sub(begin) / time.Minute)
				byMinute[minute] = append(byMinute[minute], latency)
				observations = append(observations, observation{id: doc.ID, planned: s.planned, called: s.called, observed: observed})
				delete(pending, doc.ID)
				terminals.release(doc.ID) // 终态若没经过 terminalClock，轮询读到也放行闭环发起者。
				statuses[doc.Status]++
				r.Completed++
				if doc.Status != terminal {
					r.WrongTerminal++
				}
			}
			mu.Unlock()
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
	r.ProcessCPUCores = (processCPU() - cpuBegin).Seconds() / r.ElapsedSeconds
	// 闭环的 total 由发起协程在 finished 之前写好；finished 的原子读写保证这里读到最终值。
	r.Planned = total
	r.Statuses = make(map[string]int)
	for status, n := range statuses {
		r.Statuses[strconv.Itoa(int(status))] = n
	}
	r.Started = int(started.Load())
	r.StartErrors = failures.Load()
	r.CompletionTPS = float64(r.Completed) / r.ElapsedSeconds
	r.ExpectedTerminalTPS = float64(r.Completed-r.WrongTerminal) / r.ElapsedSeconds
	quantile := func(sorted []time.Duration, p float64) float64 {
		if len(sorted) == 0 {
			return 0
		}
		return float64(sorted[int(math.Ceil(float64(len(sorted))*p))-1]) / float64(time.Millisecond)
	}
	slices.Sort(latencies)
	r.P50MS, r.P95MS, r.P99MS, r.MaxMS = quantile(latencies, .5), quantile(latencies, .95), quantile(latencies, .99), quantile(latencies, 1)
	summarize := func(values []time.Duration) sagaLatency {
		out := sagaLatency{Samples: len(values)}
		for _, v := range values {
			if v < 0 {
				out.NegativeDurations++
			}
		}
		slices.Sort(values)
		out.P50MS, out.P95MS, out.P99MS, out.MaxMS = quantile(values, .5), quantile(values, .95), quantile(values, .99), quantile(values, 1)
		return out
	}
	// 轮询读到的是已提交的终态，但同一次 Apply 可能还没返回到 terminalClock；限时等它们全部记下再对照，
	// 超时仍缺的计入 TerminalMissing，不拿观察时刻代替。
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(10 * time.Millisecond) {
		terminals.mu.Lock()
		recorded := 0
		for _, o := range observations {
			if _, ok := terminals.at[o.id]; ok {
				recorded++
			}
		}
		terminals.mu.Unlock()
		if recorded == len(observations) {
			break
		}
	}
	var admitLag, observeLag, serverDone []time.Duration
	serverByMinute := make([][]time.Duration, len(byMinute))
	warmup := min(10*time.Second, duration/6)
	r.SteadyWindowSeconds = (duration - warmup).Seconds()
	terminals.mu.Lock()
	for _, o := range observations {
		if at, ok := terminals.at[o.id]; ok && terminals.status[o.id] == terminal && at.Sub(begin) >= warmup && at.Sub(begin) < duration {
			r.SteadyCompleted++
		}
		admitLag = append(admitLag, o.called.Sub(o.planned))
		at, ok := terminals.at[o.id]
		if !ok {
			r.Server.TerminalMissing++
			continue
		}
		observeLag = append(observeLag, o.observed.Sub(at))
		serverDone = append(serverDone, at.Sub(o.called))
		minute := int(o.planned.Sub(begin) / time.Minute)
		serverByMinute[minute] = append(serverByMinute[minute], at.Sub(o.called))
	}
	terminals.mu.Unlock()
	r.SteadyTPS = float64(r.SteadyCompleted) / r.SteadyWindowSeconds
	r.Harness = sagaHarness{AdmitLag: summarize(admitLag), ObserveLag: summarize(observeLag)}
	r.Server.Start, r.Server.Completion = summarize(startDurations), summarize(serverDone)
	for minute, slice := range byMinute {
		slices.Sort(slice)
		server := serverByMinute[minute]
		slices.Sort(server)
		r.Minutes = append(r.Minutes, minuteSlice{Minute: minute, Count: len(slice), P50MS: quantile(slice, .5), P99MS: quantile(slice, .99), MaxMS: quantile(slice, 1), ServerP99MS: quantile(server, .99)})
		t.Logf("minute=%d count=%d p50=%.1fms p99=%.1fms max=%.1fms server_p99=%.1fms", minute, len(slice), quantile(slice, .5), quantile(slice, .99), quantile(slice, 1), quantile(server, .99))
	}
	// 不在完成后补发/篡改业务数据；等待框架自然排空，再逐文档核对一次且仅一次生效。
	for deadline := time.Now().Add(10 * time.Second); ; {
		harnessMongo.Add(1)
		r.OutboxPending, err = client.Database(database).Collection(defaultOutboxCollection).CountDocuments(ctx, bson.M{})
		if err != nil {
			t.Fatal(err)
		}
		if r.OutboxPending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	harnessMongo.Add(3) // 租约计数 + 两次核对读
	r.Leases, err = sagas.CountDocuments(ctx, bson.M{"lease_owner": bson.M{"$exists": true, "$ne": ""}})
	if err != nil {
		t.Fatal(err)
	}
	var values []struct {
		Value int `bson:"value"`
	}
	if err := business.Find(ctx, bson.M{}, &values); err != nil {
		t.Fatal(err)
	}
	var commits []struct {
		Commits int `bson:"commits"`
	}
	if err := effects.Find(ctx, bson.M{}, &commits); err != nil {
		t.Fatal(err)
	}
	r.BusinessDocuments, r.EffectDocuments = int64(len(values)), int64(len(commits))
	r.BusinessVerified = len(values) == total && len(commits) == total*2
	want := 2
	if mode == "compensate" {
		want = 0
	}
	for _, v := range values {
		if v.Value != want {
			r.BusinessVerified = false
		}
	}
	for _, v := range commits {
		if v.Commits != 1 {
			r.BusinessVerified = false
		}
	}
	r.FinalStats = asm.Engine.Stats()
	r.MongoCommands, r.HarnessMongoCommands = perfCommandDelta(commandsBefore, perfMongoCommands(ctx, mongoURI)), harnessMongo.Load()
	// 终态复读在命令计数之后：逐笔读一次，不计入被测链路。
	r.FinalStatuses = map[string]int{}
	for i := range total {
		final, err := asm.Engine.Get(ctx, fmt.Sprintf("perf-%d", i))
		if err != nil {
			t.Fatalf("re-read perf-%d: %v", i, err)
		}
		r.FinalStatuses[strconv.Itoa(int(final.Status))]++
		if final.Status != terminal {
			r.FinalWrongTerminal++
		}
	}
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("concurrency=%d steady_tps=%.2f cpu_cores=%.2f statuses=%v", r.Concurrency, r.SteadyTPS, r.ProcessCPUCores, r.Statuses)
	t.Logf("mode=%s completed=%d/%d rate=%.2f/s p99=%.1fms server_p99=%.1fms admit_p99=%.1fms observe_p99=%.1fms start_errors=%d business_verified=%v", mode, r.Completed, total, r.CompletionTPS, r.P99MS, r.Server.Completion.P99MS, r.Harness.AdmitLag.P99MS, r.Harness.ObserveLag.P99MS, r.StartErrors, r.BusinessVerified)
	if r.Started != total || r.Completed != total || r.StartErrors != 0 || r.WrongTerminal != 0 || r.FinalWrongTerminal != 0 || !r.BusinessVerified || r.OutboxPending != 0 || r.Leases != 0 || r.FinalStats.StoreFailures != 0 || r.FinalStats.PublishFailures != 0 || r.FinalStats.WorkerFailures != 0 {
		t.Fatal("Saga load verification failed; result retained")
	}
}

// retainSagaPerformanceEvidence 在计时结束并停止后台处理后保存测试库快照。
// 仅显式压测入口使用；原始 BSON JSON 便于按 saga ID 核对终态、重试和一次性效果。
func retainSagaPerformanceEvidence(t *testing.T, client fmongo.IMongo, database, output string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	for _, name := range []string{defaultSagaCollection, defaultOperationCollection, defaultCompletionCollection, defaultOutboxCollection, "perf_steps", "business", "effects"} {
		var docs []bson.M
		if err := client.Database(database).Collection(name).Find(ctx, bson.M{}, &docs); err != nil {
			t.Errorf("retain %s evidence: %v", name, err)
			continue
		}
		raw, err := json.Marshal(docs)
		if err == nil {
			err = os.WriteFile(output+"."+name+".json", append(raw, '\n'), 0600)
		}
		if err != nil {
			t.Errorf("retain %s evidence: %v", name, err)
		}
	}
}

// sagaLatency 是一段时间的分位（毫秒）；NegativeDurations 非零说明两个时刻的先后假设不成立，该段不可信。
type sagaLatency struct {
	Samples, NegativeDurations int
	P50MS, P95MS, P99MS, MaxMS float64
}

// sagaHarness 是压测端自己引入的等待，不计入服务端延迟。
type sagaHarness struct {
	// AdmitLag：计划启动时刻 → 提交协程调用 StartSaga。含计时器迟到、64 格任务通道与 8 个提交协程都在等上一笔 StartSaga。
	AdmitLag sagaLatency
	// ObserveLag：终态 Apply 返回 → 轮询读到终态。上界约为一个 50ms 轮询间隔加一次 Find 与本轮其余记录的处理。
	ObserveLag sagaLatency
}

// sagaServer 是请求交给协调器之后的耗时。
type sagaServer struct {
	// Start：StartSaga 调用本身（建记录并提交）。
	Start sagaLatency
	// Completion：调用 StartSaga → 终态那次 Store.Apply 提交返回，含两个步骤经 JetStream 派发、Mongo 步骤执行与完成回传。
	Completion sagaLatency
	// TerminalMissing：轮询读到终态、却没有经过 terminalClock 的 saga 数；非零说明终态有别的写出口，拆分不完整。
	TerminalMissing int
}

// terminalClock 包住协调器的 Store，记下每个 saga 第一次终态写入提交成功返回的时刻（同一进程单调时钟）。
// Store.Apply 是协调器唯一的状态写出口（stepTransition），终态都经过这里；请求与结果原样透传，不改变持久语义。
type terminalClock struct {
	Store
	mu     sync.Mutex
	at     map[string]time.Time
	status map[string]Status
	// waiters 是闭环发起者在等的 saga：第一次终态（或轮询兜底）关闭并删除通道。
	waiters map[string]chan struct{}
}

func (s *terminalClock) Apply(ctx context.Context, request ApplyRequest) (ApplyOutcome, error) {
	outcome, err := s.Store.Apply(ctx, request)
	if err == nil && outcome == ApplyApplied && request.After.Status.Terminal() {
		now := time.Now()
		s.mu.Lock()
		if _, ok := s.at[request.After.ID]; !ok {
			s.at[request.After.ID] = now
			if s.status == nil {
				s.status = make(map[string]Status)
			}
			s.status[request.After.ID] = request.After.Status
		}
		s.mu.Unlock()
		s.release(request.After.ID)
	}
	return outcome, err
}

// wait 在 StartSaga 之前登记，避免终态先于登记到达而永远等不到。
func (s *terminalClock) wait(id string) <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	done := make(chan struct{})
	s.waiters[id] = done
	return done
}

func (s *terminalClock) release(id string) {
	s.mu.Lock()
	done, ok := s.waiters[id]
	delete(s.waiters, id)
	s.mu.Unlock()
	if ok {
		close(done)
	}
}

// sagaCommandTimings 只用于显式诊断。单进程单调时钟关联交易与命令，事件与未取标记各有上限。
// 含 Mongo 命令监视器与记录开销；重复事件原样保留，分析方不能把重试错当一次正常执行。
type sagaCommandTiming struct {
	Count          int64
	TotalMS, MaxMS float64
}
type sagaCommandTimings struct {
	mu      sync.Mutex
	values  map[string]sagaCommandTiming
	marks   map[string]time.Time
	epoch   time.Time
	events  []sagaTimelineEvent
	dropped int
}

func (r *sagaCommandTimings) Observe(name string, elapsed time.Duration) {
	r.mu.Lock()
	defer r.mu.Unlock()
	v := r.values[name]
	v.Count++
	ms := float64(elapsed) / float64(time.Millisecond)
	v.TotalMS += ms
	v.MaxMS = max(v.MaxMS, ms)
	r.values[name] = v
}

// 10 分钟、30/s 的正常链路约 60 万事件；超限明确报丢弃，不能静默给出完整覆盖结论。
const sagaTimelineLimit = 1_000_000

type sagaTimelineEvent struct {
	Key  string
	AtNS int64
}

func (r *sagaCommandTimings) Mark(key string, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if strings.HasPrefix(key, "timeline/") {
		if len(r.events) >= sagaTimelineLimit {
			r.dropped++
			return
		}
		r.events = append(r.events, sagaTimelineEvent{key, at.Sub(r.epoch).Nanoseconds()})
		return
	}
	if len(r.marks) >= sagaTimelineLimit {
		r.dropped++
		return
	}
	r.marks[key] = at
}
func (r *sagaCommandTimings) Take(key string) (time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	at, ok := r.marks[key]
	delete(r.marks, key)
	return at, ok
}

// 同一个命令的重复事件不能被覆盖；关联标记取走即删，缺失标记不伪造时间。
func TestSagaTimelineRecorder(t *testing.T) {
	epoch := time.Unix(100, 0)
	r := &sagaCommandTimings{epoch: epoch, marks: make(map[string]time.Time)}
	r.Mark("timeline/step.begin/cmd", epoch.Add(time.Millisecond))
	r.Mark("timeline/step.begin/cmd", epoch.Add(2*time.Millisecond))
	if len(r.events) != 2 || r.events[0].AtNS != int64(time.Millisecond) || r.events[1].AtNS != int64(2*time.Millisecond) {
		t.Fatalf("events lost or clock changed: %v", r.events)
	}
	r.Mark("published/cmd", epoch)
	if at, ok := r.Take("published/cmd"); !ok || !at.Equal(epoch) {
		t.Fatal("missing mark")
	}
	if _, ok := r.Take("published/cmd"); ok {
		t.Fatal("mark was not consumed")
	}
}

// processCPU 是本进程至今的用户态 + 内核态 CPU 时间，压测前后相减得到整段用量。
func processCPU() time.Duration {
	var usage syscall.Rusage
	if err := syscall.Getrusage(syscall.RUSAGE_SELF, &usage); err != nil {
		return 0
	}
	return time.Duration(usage.Utime.Nano() + usage.Stime.Nano())
}

// perfResourcePrefix 读压测资源名前缀：ROOST_PERF_DB_PREFIX（库名）与 ROOST_PERF_STREAM_PREFIX（流名）。
// 在云库或共享环境上压测时用自己的唯一前缀，便于事后按前缀核对清理；不设时保持各夹具原来的名字。
func perfResourcePrefix(t *testing.T, variable, required, fallback string) string {
	t.Helper()
	value := os.Getenv(variable)
	if value == "" {
		return fallback
	}
	if !strings.HasPrefix(value, required) || len(value) > 40 || strings.ContainsFunc(value, func(r rune) bool {
		return !(r == '_' || r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z')
	}) {
		t.Fatalf("%s must start with %s and contain only letters, digits and underscores (at most 40)", variable, required)
	}
	return value
}

func perfStreamPrefix(t *testing.T, fallback string) string {
	return perfResourcePrefix(t, "ROOST_PERF_STREAM_PREFIX", "SAGAPERF_", fallback)
}

// perfMongoCommands 读一次 serverStatus.metrics.commands 里数据面命令的累计次数（一次往返一条命令）。压测窗口前后各读一次，
// 差值减去夹具自己的读（HarnessMongoCommands）就是被测链路在 Mongo 上的往返数。连接用一条独立的驱动连接，serverStatus 本身
// 不在统计的命令里。读不到（没有权限）时返回 nil。
func perfMongoCommands(ctx context.Context, uri string) map[string]int64 {
	client, err := mongodriver.Connect(mongooptions.Client().ApplyURI(uri))
	if err != nil {
		return nil
	}
	defer func() { _ = client.Disconnect(context.Background()) }()
	raw, err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Raw()
	if err != nil {
		return nil
	}
	out := make(map[string]int64)
	// getMore 不计：副本集的从节点经 getMore 拉主节点的 oplog，它随写入量增长，不是客户端往返。
	for _, name := range []string{"insert", "update", "delete", "find", "findAndModify", "aggregate", "bulkWrite", "count", "commitTransaction", "abortTransaction"} {
		value, err := raw.LookupErr("metrics", "commands", name, "total")
		if err != nil {
			continue
		}
		if n, ok := value.AsInt64OK(); ok {
			out[name] = n
		}
	}
	return out
}

// perfCommandDelta 是两次 perfMongoCommands 的差值。
func perfCommandDelta(before, after map[string]int64) map[string]int64 {
	if before == nil || after == nil {
		return nil
	}
	out := make(map[string]int64, len(after))
	for name, value := range after {
		out[name] = value - before[name]
	}
	return out
}
