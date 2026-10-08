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

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	fnats "github.com/tjbdwanghaibo/roost-core/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/nats/driver"
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
	}
	total := int(duration/time.Second) * rate
	r := result{Mode: mode, Rate: rate, Planned: total, DurationSeconds: duration.Seconds()}
	var mu sync.Mutex
	pending := make(map[string]time.Time)
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
				_, err := asm.Engine.StartSaga(ctx, StartRequest{ID: id, Type: def.Type, DefinitionVersion: 1, BusinessKey: id, DeadlineAt: j.planned.Add(30 * time.Second)})
				mu.Lock()
				if err != nil {
					failures.Add(1)
					if r.FirstError == "" {
						r.FirstError = err.Error()
					}
				} else {
					pending[id] = j.planned
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
				latencies = append(latencies, observed.Sub(pending[doc.ID]))
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
	slices.Sort(latencies)
	q := func(p float64) float64 {
		if len(latencies) == 0 {
			return 0
		}
		return float64(latencies[int(math.Ceil(float64(len(latencies))*p))-1]) / float64(time.Millisecond)
	}
	r.P50MS, r.P95MS, r.P99MS, r.MaxMS = q(.5), q(.95), q(.99), q(1)
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
	t.Logf("mode=%s completed=%d/%d rate=%.2f/s p99=%.1fms start_errors=%d business_verified=%v", mode, r.Completed, total, r.CompletionTPS, r.P99MS, r.StartErrors, r.BusinessVerified)
	if r.Started != total || r.Completed != total || r.StartErrors != 0 || r.WrongTerminal != 0 || !r.BusinessVerified || r.OutboxPending != 0 || r.Leases != 0 || r.FinalStats.StoreFailures != 0 || r.FinalStats.PublishFailures != 0 || r.FinalStats.WorkerFailures != 0 {
		t.Fatal("Saga load verification failed; result retained")
	}
}
