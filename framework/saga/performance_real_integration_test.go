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
	"testing"
	"time"

	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
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
	if rate < 1 || rate > 1000 || duration < time.Second || duration > 10*time.Minute || duration%time.Second != 0 || (mode != "forward" && mode != "compensate") {
		t.Fatal("invalid Saga performance parameters")
	}
	natsURL := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if natsURL == "" || os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI") == "" {
		t.Fatal("private Mongo/NATS environment required")
	}
	client, database := realRevn06s5Mongo(t)
	suffix := fmt.Sprintf("%d_%d", os.Getpid(), time.Now().UnixNano())
	stream, prefix := "ROOST_PERF_SAGA_"+suffix, "perfsaga"+strings.ReplaceAll(suffix, "_", "x")
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
	cfg := crossProcessAssembly(database, stream, prefix, "perf")
	cfg.Engine = DefaultOptions()
	cfg.Engine.Owner = "perf"
	cfg.Stream.Replicas = 3 // 容量测量保留三副本发布确认，不借故障夹具的单副本降低持久成本。
	asm, err := Assemble(client, js, cfg, def)
	if err != nil {
		t.Fatal(err)
	}
	// 只包一层观察，不换 Assemble 的正式 Store；必须在 Start 之前替换，此后引擎只经 e.store 写记录。
	terminals := &terminalClock{Store: asm.Engine.store, at: make(map[string]time.Time)}
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
	for _, step := range def.Steps {
		for _, topic := range []string{step.ForwardTopic, step.CompensateTopic} {
			sub, err := SubscribeMongoStep(ctx, js, asm.Transport, inbox, StepConsumerConfig{Stream: stream, Durable: strings.ReplaceAll(topic, ".", "-"), Topic: topic}, handler)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(sub.Drain)
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
		Rate, Planned, Started, Completed, WrongTerminal          int
		StartErrors                                               uint64
		FirstError                                                string
		DurationSeconds, ElapsedSeconds, CompletionTPS            float64
		P50MS, P95MS, P99MS, MaxMS                                float64
		FinalStats                                                Stats
		OutboxPending, Leases, BusinessDocuments, EffectDocuments int64
		BusinessVerified                                          bool
		Minutes                                                   []minuteSlice
		// P50MS～MaxMS 从计划启动时刻算到轮询观察到持久终态，混有压测端等待，保留它们与历史样本同口径比较。
		// Harness 与 Server 把同一段时间拆开（同一进程单调时钟）：
		//
		//	planned ─Harness.AdmitLag─▶ called ─Server.Start─▶ StartSaga 返回
		//	called ──────────── Server.Completion ────────────▶ terminal ─Harness.ObserveLag─▶ observed
		//
		// terminal 是终态那次 Store.Apply 提交成功返回的时刻（terminalClock），评估框架延迟看 Server.Completion。
		Harness sagaHarness
		Server  sagaServer
	}
	total := int(duration/time.Second) * rate
	r := result{Mode: mode, Rate: rate, Planned: total, DurationSeconds: duration.Seconds()}
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
	var latencies []time.Duration
	byMinute := make([][]time.Duration, (total+rate*60-1)/(rate*60))
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
	r.Started = int(started.Load())
	r.StartErrors = failures.Load()
	r.CompletionTPS = float64(r.Completed) / r.ElapsedSeconds
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
	terminals.mu.Lock()
	for _, o := range observations {
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
		r.OutboxPending, err = client.Database(database).Collection(defaultOutboxCollection).CountDocuments(ctx, bson.M{})
		if err != nil {
			t.Fatal(err)
		}
		if r.OutboxPending == 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
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
	raw, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(output, append(raw, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("mode=%s completed=%d/%d rate=%.2f/s p99=%.1fms server_p99=%.1fms admit_p99=%.1fms observe_p99=%.1fms start_errors=%d business_verified=%v", mode, r.Completed, total, r.CompletionTPS, r.P99MS, r.Server.Completion.P99MS, r.Harness.AdmitLag.P99MS, r.Harness.ObserveLag.P99MS, r.StartErrors, r.BusinessVerified)
	if r.Started != total || r.Completed != total || r.StartErrors != 0 || r.WrongTerminal != 0 || !r.BusinessVerified || r.OutboxPending != 0 || r.Leases != 0 || r.FinalStats.StoreFailures != 0 || r.FinalStats.PublishFailures != 0 || r.FinalStats.WorkerFailures != 0 {
		t.Fatal("Saga load verification failed; result retained")
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
	mu sync.Mutex
	at map[string]time.Time
}

func (s *terminalClock) Apply(ctx context.Context, request ApplyRequest) (ApplyOutcome, error) {
	outcome, err := s.Store.Apply(ctx, request)
	if err == nil && outcome == ApplyApplied && request.After.Status.Terminal() {
		now := time.Now()
		s.mu.Lock()
		if _, ok := s.at[request.After.ID]; !ok {
			s.at[request.After.ID] = now
		}
		s.mu.Unlock()
	}
	return outcome, err
}
