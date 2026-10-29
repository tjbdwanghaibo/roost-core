//go:build integration

package saga

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"go.mongodb.org/mongo-driver/v2/bson"
	"math/rand/v2"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
)

// TestRealJetStreamProbe 隔离总线交付成本：三副本文件流、正式驱动、立即 ACK。
// 发布与接收在同进程，用单调时钟计算；rate=0 是有界并发闭环，不代表开环容量。
func TestRealJetStreamProbe(t *testing.T) {
	output := os.Getenv("ROOST_JS_PROBE_OUTPUT")
	if output == "" {
		t.Skip("set ROOST_JS_PROBE_OUTPUT and private NATS environment")
	}
	integer := func(key string, fallback int) int {
		if os.Getenv(key) == "" {
			return fallback
		}
		n, err := strconv.Atoi(os.Getenv(key))
		if err != nil || n < 0 {
			t.Fatalf("invalid %s", key)
		}
		return n
	}
	workers, rate, seconds := integer("ROOST_JS_PROBE_WORKERS", 4), integer("ROOST_JS_PROBE_RATE", 120), integer("ROOST_JS_PROBE_SECONDS", 30)
	if workers < 1 || workers > 128 || seconds < 1 || seconds > 120 || rate > 10000 {
		t.Fatal("probe bounds exceeded")
	}
	url := os.Getenv("ROOST_DATAENGINE_IT_NATS_URL")
	if url == "" {
		t.Fatal("private NATS required")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second+30*time.Second)
	defer cancel()
	nc, err := ndriver.NewClient(fnats.DefaultConfig(url), ndriver.ClientOptions{})
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := ndriver.NewJetStreamClient(nc)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := gonats.Connect(url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	raw, err := gojs.New(admin)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("ROOST_JS_PROBE_%d_%d", os.Getpid(), time.Now().UnixNano())
	subject := name + ".command"
	if err = js.EnsureStream(ctx, fnats.JetStreamConfig{Name: name, Subjects: []string{subject}, Storage: fnats.JetStreamStorageFile, Replicas: 3, MaxAge: time.Hour, MaxBytes: 512 << 20}); err != nil {
		t.Fatal(err)
	}
	defer func() {
		cleanup, c := context.WithTimeout(context.Background(), 10*time.Second)
		defer c()
		if e := raw.DeleteStream(cleanup, name); e != nil {
			t.Error(e)
		}
	}()
	type sample struct {
		start        time.Time
		ack, receive time.Duration
		delivered    bool
	}
	var mu sync.Mutex
	var rows []*sample
	duplicates, invalid, publishErrors := 0, 0, 0
	sub, err := js.Subscribe(ctx, fnats.JetStreamConsumerConfig{Stream: name, Durable: "probe", FilterSubject: subject, AckWait: 30 * time.Second, MaxDeliver: 25000, MaxAckPending: 256}, func(_ context.Context, m *fnats.JetStreamMsg) error {
		now := time.Now()
		mu.Lock()
		defer mu.Unlock()
		if len(m.Data) != 1024 {
			invalid++
			return fmt.Errorf("invalid payload")
		}
		id := binary.LittleEndian.Uint64(m.Data)
		if id >= uint64(len(rows)) {
			invalid++
			return fmt.Errorf("unknown id")
		}
		r := rows[id]
		if r.delivered {
			duplicates++
			return nil
		}
		r.delivered = true
		r.receive = now.Sub(r.start)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	defer sub.Stop()
	start := time.Now()
	deadline := start.Add(time.Duration(seconds) * time.Second)
	jobs := make(chan struct{}) // 无积压生成队列，开放速率档另记是否达到目标。
	var wg sync.WaitGroup
	for range workers {
		wg.Go(func() {
			payload := make([]byte, 1024)
			for range jobs {
				mu.Lock()
				id := len(rows)
				r := &sample{start: time.Now()}
				rows = append(rows, r)
				mu.Unlock()
				binary.LittleEndian.PutUint64(payload, uint64(id))
				_, e := js.Publish(ctx, subject, payload, fnats.JetStreamPublishOptions{MsgID: strconv.Itoa(id)})
				elapsed := time.Since(r.start)
				mu.Lock()
				r.ack = elapsed
				if e != nil {
					publishErrors++
				}
				mu.Unlock()
			}
		})
	}
	generated := 0
publishLoop:
	for time.Now().Before(deadline) {
		if rate > 0 {
			due := start.Add(time.Duration(generated) * time.Second / time.Duration(rate))
			if d := time.Until(due); d > 0 {
				time.Sleep(d)
			}
		}
		if !time.Now().Before(deadline) {
			break
		}
		select {
		case jobs <- struct{}{}:
			generated++
		case <-ctx.Done():
			t.Error(ctx.Err())
			break publishLoop
		}
	}
	close(jobs)
	wg.Wait()
	publishElapsed := time.Since(start)
	// 直到服务器确认消费 ACK 排空；最后的 ACK 不只看本地 handler 已返回。
	consumer, err := raw.Consumer(ctx, name, "probe")
	if err != nil {
		t.Fatal(err)
	}
	var info *gojs.ConsumerInfo
	for {
		info, err = consumer.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		mu.Lock()
		received := 0
		for _, r := range rows {
			if r.delivered {
				received++
			}
		}
		total := len(rows)
		mu.Unlock()
		if received == total && info.NumAckPending == 0 && info.NumPending == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("probe did not drain", ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	sub.Drain()
	select {
	case <-sub.Closed():
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	drainElapsed := time.Since(start)
	type latency struct{ MeanMS, P50MS, P99MS, MaxMS float64 }
	summarize := func(values []float64) latency {
		slices.Sort(values)
		sum := 0.0
		for _, v := range values {
			sum += v
		}
		return latency{sum / float64(len(values)), values[(len(values)-1)/2], values[int(float64(len(values)-1)*.99)], values[len(values)-1]}
	}
	mu.Lock()
	defer mu.Unlock()
	a, d := make([]float64, 0, len(rows)), make([]float64, 0, len(rows))
	for _, r := range rows {
		a = append(a, float64(r.ack)/1e6)
		if r.delivered {
			d = append(d, float64(r.receive)/1e6)
		}
	}
	// 按发布秒分桶，资源采样可以对齐阶段，避免全程平均掩盖混合负载窗口。
	type secondSample struct {
		Second, Count    int
		PubAck, Delivery latency
	}
	var perSecond []secondSample
	for second := 0; second <= int(publishElapsed.Seconds()); second++ {
		var ackValues, deliveryValues []float64
		for _, r := range rows {
			if int(r.start.Sub(start).Seconds()) != second {
				continue
			}
			ackValues = append(ackValues, float64(r.ack)/1e6)
			if r.delivered {
				deliveryValues = append(deliveryValues, float64(r.receive)/1e6)
			}
		}
		if len(ackValues) > 0 && len(deliveryValues) > 0 {
			perSecond = append(perSecond, secondSample{second, len(ackValues), summarize(ackValues), summarize(deliveryValues)})
		}
	}
	result := struct {
		Workers, Rate, Seconds, PayloadBytes, Published, Received, PublishErrors, Duplicates, Invalid int
		PublishTPS, DrainInclusiveTPS                                                                 float64
		PubAck, Delivery                                                                              latency
		Consumer                                                                                      *gojs.ConsumerInfo
		StartedAt                                                                                     time.Time
		PerSecond                                                                                     []secondSample
	}{
		Workers: workers, Rate: rate, Seconds: seconds, PayloadBytes: 1024,
		Published: len(rows), Received: len(d), PublishErrors: publishErrors,
		Duplicates: duplicates, Invalid: invalid,
		PublishTPS:        float64(len(rows)) / publishElapsed.Seconds(),
		DrainInclusiveTPS: float64(len(d)) / drainElapsed.Seconds(),
		PubAck:            summarize(a), Delivery: summarize(d), Consumer: info,
		StartedAt: start, PerSecond: perSecond,
	}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(output, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("workers=%d rate=%d pub=%d received=%d tps=%.1f delivery_mean=%.2fms p99=%.2fms ack_mean=%.2fms", workers, rate, len(rows), len(d), result.DrainInclusiveTPS, result.Delivery.MeanMS, result.Delivery.P99MS, result.PubAck.MeanMS)
	if publishErrors != 0 || invalid != 0 || duplicates != 0 || len(rows) != len(d) {
		t.Error("probe correctness check failed")
	}
}

// TestRealMongoCommitProbe 只制造与30 Saga/s约等量的提交频率，不建立任何NATS连接。
// 每笔两条独立文档，保留正式驱动的majority+journal，避免热点冲突混入对照。
func TestRealMongoCommitProbe(t *testing.T) {
	output := os.Getenv("ROOST_MONGO_PROBE_OUTPUT")
	if output == "" {
		t.Skip("set ROOST_MONGO_PROBE_OUTPUT")
	}
	client, database := realRevn06s5Mongo(t)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	coll := client.Database(database).Collection("probe")
	if _, err := coll.InsertOne(ctx, bson.M{"_id": "seed"}); err != nil {
		t.Fatal(err)
	}
	if _, err := coll.DeleteOne(ctx, bson.M{"_id": "seed"}); err != nil {
		t.Fatal(err)
	}
	var completed, failed atomic.Int64
	var wg sync.WaitGroup
	jobs := make(chan int)
	for range 8 {
		wg.Go(func() {
			for id := range jobs {
				session, err := client.StartSession(ctx)
				if err == nil {
					err = session.WithTransaction(ctx, func(tx context.Context) error {
						for i := range 2 {
							if _, e := coll.InsertOne(tx, bson.M{"_id": fmt.Sprintf("%d-%d", id, i), "value": 1}); e != nil {
								return e
							}
						}
						return nil
					})
					session.EndSession(ctx)
				}
				if err != nil {
					failed.Add(1)
					t.Errorf("transaction: %v", err)
				} else {
					completed.Add(1)
				}
			}
		})
	}
	start := time.Now()
	for id := range 14400 {
		due := start.Add(time.Duration(id) * time.Second / 240)
		if d := time.Until(due); d > 0 {
			time.Sleep(d)
		}
		jobs <- id
	}
	close(jobs)
	wg.Wait()
	elapsed := time.Since(start)
	count, err := coll.CountDocuments(ctx, bson.M{"value": 1})
	if err != nil {
		t.Fatal(err)
	}
	result := struct {
		Completed, Failed, Documents int64
		ElapsedSeconds, TPS          float64
	}{completed.Load(), failed.Load(), count, elapsed.Seconds(), float64(completed.Load()) / elapsed.Seconds()}
	data, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(output, append(data, '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	t.Logf("committed=%d failed=%d docs=%d tps=%.2f", result.Completed, result.Failed, count, result.TPS)
	if result.Failed != 0 || count != result.Completed*2 {
		t.Error("Mongo probe verification failed")
	}
}

// probeMongo 是 TestRealMongoCommitProbe 用的隔离库（唯一库名，用后删除）。
// 历史：realRevn06s5Mongo（cross_process_real_integration_test.go，第 4 批随 Mongo 协调器用例删除）。
func realRevn06s5Mongo(t *testing.T) (*driver.Client, string) {
	t.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		t.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set; source ~/.roost-it/roost-dataengine-it/env.sh")
	}
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	database := fmt.Sprintf("roost_sagaprobe_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
		_ = client.Close(context.Background())
	})
	return client, database
}

// ---- 正式实现的 saga 压测一档（scripts/perf/saga-js-scan.sh，设计 §12）----

// TestRealSagaJSScanLevel 跑压测的一档，结果写进 ROOST_SAGA_SCAN_OUTPUT（JSON）。直接运行（没有这个变量）时跳过。
//
// 口径（设计 §12）：私有三节点 R3（ROOST_NATS_CLUSTER_URL）、本机隔离 Mongo（ROOST_DATAENGINE_IT_MONGO_URI，唯一库、用后删）、
// 闭环并发（每个 worker：StartSaga → WaitTerminal → 下一笔）、稳态窗口 [warmup, seconds)。协调器是正式 Engine（P=16、G=4、
// R3）；原生参与方是正式路径：真实 Nest + nestwal WAL + DataEngine 投影进 Mongo（8 通道）+ 实体内回执（DAO 形状同生成 DAO）+
// SubscribeNativeStep；Mongo 步骤是 SubscribeMongoStep + MongoStepInbox；准入档用 LoadPublisher 发投影积压。
//
// 场景（ROOST_SAGA_SCAN_SCENARIO）：native-forward、native-compensate（每笔 credit 被拒，全补偿）、native-pipelined（步骤
// handler 用 DurabilityPipelined）、mongo-forward、admission（准入开，高 5000 / 低 2500）、snapshot（每 2000 事件快照）、
// replay（合成 ROOST_SAGA_SCAN_REPLAY_MESSAGES 条日志后量一次全量重放的启动时长）。
//
// 正确性（硬门槛，写进结果、不达标用例失败）：每笔终态符合预期（正向 Completed、拒绝的 Compensated）；原生档 Nest 内存与
// Mongo 里每个账户的余额 = 按终态算出的预期；Mongo 档每个账户余额同样核对；发布结果未知 0；停止加压后 30s 内投影积压归零。
func TestRealSagaJSScanLevel(t *testing.T) {
	output := os.Getenv("ROOST_SAGA_SCAN_OUTPUT")
	if output == "" {
		t.Skip("set ROOST_SAGA_SCAN_OUTPUT (scripts/perf/saga-js-scan.sh runs this level by level)")
	}
	integer := func(key string, fallback, lo, hi int) int {
		t.Helper()
		n := fallback
		if raw := os.Getenv(key); raw != "" {
			var err error
			if n, err = strconv.Atoi(raw); err != nil {
				t.Fatalf("invalid %s", key)
			}
		}
		if n < lo || n > hi {
			t.Fatalf("%s=%d out of [%d, %d]", key, n, lo, hi)
		}
		return n
	}
	scenario := os.Getenv("ROOST_SAGA_SCAN_SCENARIO")
	concurrency := integer("ROOST_SAGA_SCAN_CONCURRENCY", 16, 1, 512)
	seconds := integer("ROOST_SAGA_SCAN_SECONDS", 30, 1, 120)
	warmup := integer("ROOST_SAGA_SCAN_WARMUP", min(5, seconds/2), 0, seconds-1)
	url := realJSURL(t)
	mongo := ptMongoClient(t)
	database := fmt.Sprintf("roost_sagajs_scan_%d_%d", os.Getpid(), time.Now().UnixNano())
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		if err := mongo.Database(database).Drop(ctx); err != nil {
			t.Errorf("drop %s: %v", database, err)
		}
	})
	layout := realLayout(t, url, 16, 4)
	conn, js := ptJetStream(t, url)
	result := scanResult{Scenario: scenario, Concurrency: concurrency, Seconds: seconds, Warmup: warmup, GOMAXPROCS: runtime.GOMAXPROCS(0)}
	options := realEngineOptions(layout, allPartitions(layout.Partitions))
	options.PublishUnknownBudget, options.DuplicateWindow = 30*time.Second, 2*time.Minute
	options.Admission.Enabled = false
	switch scenario {
	case "admission":
		options.Admission = AdmissionOptions{Enabled: true, HighRecords: 5000, LowRecords: 2500, StaleAfter: 2 * time.Second}
	case "snapshot":
		options.SnapshotEveryEvents, options.SnapshotInterval = 2000, 30*time.Second
	case "replay":
		result.Replay = scanReplay(t, conn, options, concurrency, integer("ROOST_SAGA_SCAN_REPLAY_MESSAGES", 10_000, 100, 1_000_000))
		writeScanResult(t, output, result)
		return
	case "native-forward", "native-compensate", "native-pipelined", "mongo-forward":
	default:
		t.Fatalf("unknown ROOST_SAGA_SCAN_SCENARIO %q", scenario)
	}
	engine := startRealEngineWith(t, conn, options, ptDefinition(), scanMongoDefinition())
	const accounts = 256
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(seconds)*time.Second+5*time.Minute)
	defer cancel()
	sagaType := "pt"
	var participant *ptParticipant
	var accountsColl fmongo.ICollection
	if scenario == "mongo-forward" {
		sagaType = "mt"
		accountsColl = scanMongoParticipant(t, ctx, mongo, database, js, layout, accounts)
	} else {
		durability := nest.DurabilityStrict
		if scenario == "native-pipelined" {
			durability = nest.DurabilityPipelined
		}
		var err error
		participant, err = openPtParticipantWith(t.TempDir(), mongo, database, accounts, durability)
		if err != nil {
			t.Fatal(err)
		}
		if err := participant.subscribe(ctx, js, layout); err != nil {
			t.Fatal(err)
		}
		if scenario == "admission" {
			kv, err := conn.log.KeyValue(ctx, loadBucketConfig(layout, options.Replicas, options.Admission.withDefaults()))
			if err != nil {
				t.Fatal(err)
			}
			publisher, err := NewLoadPublisher(kv, "scan.1", 200*time.Millisecond, func() uint64 { return participant.projector.Stats().WALUnacked })
			if err != nil {
				t.Fatal(err)
			}
			runCtx, stop := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() { defer close(done); publisher.Run(runCtx) }()
			defer func() { stop(); <-done }()
		}
	}
	backlog := func() uint64 {
		if participant == nil {
			return 0
		}
		return participant.projector.Stats().WALUnacked
	}
	var peak atomic.Uint64
	sampleDone := make(chan struct{})
	sampleCtx, stopSampling := context.WithCancel(ctx)
	go func() {
		defer close(sampleDone)
		for sampleCtx.Err() == nil {
			if b := backlog(); b > peak.Load() {
				peak.Store(b)
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	type finished struct {
		bk       string
		transfer ptTransfer
		status   Status
		at       time.Time
		latency  time.Duration
	}
	var mu sync.Mutex
	var done []finished
	var seq atomic.Int64
	var rejected atomic.Int64
	start := time.Now()
	deadline := start.Add(time.Duration(seconds) * time.Second)
	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			for time.Now().Before(deadline) {
				i := seq.Add(1)
				transfer := ptTransfer{From: rand.Int64N(accounts), Amount: 1, Refuse: scenario == "native-compensate"}
				transfer.To = (transfer.From + 1 + rand.Int64N(accounts-1)) % accounts
				bk := "s-" + strconv.FormatInt(i, 10)
				data, _ := json.Marshal(transfer)
				began := time.Now()
				for {
					startCtx, startCancel := context.WithTimeout(ctx, 30*time.Second)
					_, err := engine.StartSaga(startCtx, StartRequest{Type: sagaType, BusinessKey: bk, DefinitionVersion: 1, Data: data})
					startCancel()
					if err == nil || errors.Is(err, ErrStartUnconfirmed) {
						break
					}
					if !errors.Is(err, ErrBackpressure) {
						t.Errorf("start %s: %v", bk, err)
						return
					}
					rejected.Add(1)
					time.Sleep(20 * time.Millisecond)
				}
				waitCtx, waitCancel := context.WithTimeout(ctx, 90*time.Second)
				final, err := engine.WaitTerminal(waitCtx, IDOf(sagaType, bk))
				waitCancel()
				if err != nil {
					t.Errorf("wait %s: %v", bk, err)
					return
				}
				mu.Lock()
				done = append(done, finished{bk: bk, transfer: transfer, status: final.Status, at: time.Now(), latency: time.Since(began)})
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	loadEnded := time.Now()
	for backlog() > 0 && time.Since(loadEnded) < 30*time.Second {
		time.Sleep(50 * time.Millisecond)
	}
	result.DrainSeconds = time.Since(loadEnded).Seconds()
	result.BacklogAfterDrain = backlog()
	stopSampling()
	<-sampleDone

	// 吞吐与延迟：稳态窗口里到终态的笔数。
	windowStart, windowEnd := start.Add(time.Duration(warmup)*time.Second), deadline
	var latencies []time.Duration
	want := make([]int64, accounts)
	for i := range want {
		want[i] = ptInitialBalance
	}
	for _, f := range done {
		if !f.at.Before(windowStart) && f.at.Before(windowEnd) {
			latencies = append(latencies, f.latency)
		}
		switch {
		case f.status == StatusCompleted && !f.transfer.Refuse:
			want[f.transfer.From] -= f.transfer.Amount
			want[f.transfer.To] += f.transfer.Amount
		case f.status == StatusCompensated && f.transfer.Refuse:
		default:
			result.Unexpected++
		}
	}
	slices.Sort(latencies)
	result.Sagas = len(done)
	result.SteadySagas = len(latencies)
	result.PerSecond = float64(len(latencies)) / windowEnd.Sub(windowStart).Seconds()
	if n := len(latencies); n > 0 {
		result.P50Ms = float64(latencies[n/2]) / float64(time.Millisecond)
		result.P99Ms = float64(latencies[min(n-1, n*99/100)]) / float64(time.Millisecond)
	}
	stats := engine.Stats()
	result.PublishUnknown, result.AdmissionRejected, result.StartRetries, result.BacklogPeak = stats.PublishUnknown, stats.AdmissionRejected, rejected.Load(), peak.Load()
	result.Snapshots, result.Purges = stats.Snapshots, stats.Purges

	// 余额逐账户核对：Nest 内存（原生）与 Mongo（投影完之后 / Mongo 步骤）。
	if participant != nil {
		for n := range int64(accounts) {
			if got := participant.balance(t, n); got != want[n] {
				result.BalanceMismatches++
				t.Errorf("account %d: Nest balance %d, want %d", n, got, want[n])
			}
		}
		if err := participant.close(); err != nil {
			t.Fatal(err)
		}
		accountsColl = mongo.Database(database).Collection("account")
	}
	var docs []ptAccountDoc
	if err := accountsColl.Find(ctx, bson.M{}, &docs); err != nil {
		t.Fatal(err)
	}
	seen := map[int64]int64{}
	for _, d := range docs {
		seen[d.ID] = d.Balance
	}
	for n := range int64(accounts) {
		if got := seen[ptAccountID(n)]; got != want[n] {
			result.BalanceMismatches++
			t.Errorf("account %d: Mongo balance %d, want %d", n, got, want[n])
		}
	}
	writeScanResult(t, output, result)
	if result.Unexpected != 0 || result.PublishUnknown != 0 || result.BalanceMismatches != 0 || result.BacklogAfterDrain != 0 {
		t.Fatalf("correctness gate failed: %+v", result)
	}
}

// scanResult 是一档的结果（脚本汇总它）。
type scanResult struct {
	Scenario          string            `json:"scenario"`
	Concurrency       int               `json:"concurrency"`
	Seconds           int               `json:"seconds"`
	Warmup            int               `json:"warmup"`
	GOMAXPROCS        int               `json:"gomaxprocs"`
	Sagas             int               `json:"sagas"`
	SteadySagas       int               `json:"steady_sagas"`
	PerSecond         float64           `json:"per_second"`
	P50Ms             float64           `json:"p50_ms"`
	P99Ms             float64           `json:"p99_ms"`
	Unexpected        int               `json:"unexpected_terminals"`
	BalanceMismatches int               `json:"balance_mismatches"`
	PublishUnknown    uint64            `json:"publish_unknown"`
	AdmissionRejected uint64            `json:"admission_rejected"`
	StartRetries      int64             `json:"start_retries"`
	BacklogPeak       uint64            `json:"backlog_peak_records"`
	BacklogAfterDrain uint64            `json:"backlog_after_drain"`
	DrainSeconds      float64           `json:"drain_seconds"`
	Snapshots         uint64            `json:"snapshots"`
	Purges            uint64            `json:"purges"`
	Replay            *scanReplayResult `json:"replay,omitempty"`
}

type scanReplayResult struct {
	Messages     uint64  `json:"messages"`
	Sagas        int     `json:"sagas"`
	StartSeconds float64 `json:"start_seconds"`
	Replayed     uint64  `json:"replayed_events"`
}

func writeScanResult(t *testing.T, path string, result scanResult) {
	t.Helper()
	raw, _ := json.MarshalIndent(result, "", "  ")
	if err := os.WriteFile(path, append(raw, '\n'), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("%s c%d: %.1f saga/s p50 %.1fms p99 %.1fms (%d sagas)", result.Scenario, result.Concurrency, result.PerSecond, result.P50Ms, result.P99Ms, result.Sagas)
}

// scanMongoDefinition 是 Mongo 步骤的转账 saga（与 pt 同形，主题 mt.*）。
func scanMongoDefinition() Definition {
	d := ptDefinition()
	d.Type = "mt"
	for i := range d.Steps {
		d.Steps[i].ForwardTopic = "mt." + d.Steps[i].Name
		d.Steps[i].CompensateTopic = "mt." + d.Steps[i].Name + ".cancel"
	}
	return d
}

// scanMongoParticipant 订阅 Mongo 步骤：handler 在步骤事务里改 <database>.mt_account 的余额（MongoStepInbox 的回执与业务写
// 同一事务）。返回账户集合。
func scanMongoParticipant(t *testing.T, ctx context.Context, mongo fmongo.IMongo, database string, js fnats.IJetStream, layout Layout, accounts int) fmongo.ICollection {
	t.Helper()
	coll := mongo.Database(database).Collection("mt_account")
	for n := range int64(accounts) {
		if _, err := coll.InsertOne(ctx, ptAccountDoc{ID: ptAccountID(n), Balance: ptInitialBalance}); err != nil {
			t.Fatal(err)
		}
	}
	inbox, err := NewMongoStepInbox(mongo, database, "_mt_step_inbox", time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	handler := func(ctx context.Context, command Command) (Completion, error) {
		transfer := ptPayload(command)
		account, delta := transfer.From, -transfer.Amount
		switch command.Topic {
		case "mt.debit.cancel":
			delta = transfer.Amount
		case "mt.credit":
			account, delta = transfer.To, transfer.Amount
			if transfer.Refuse {
				return Completion{Error: "credit refused"}, nil
			}
		case "mt.credit.cancel":
			account = transfer.To
		}
		if _, err := coll.UpdateOne(ctx, bson.M{"_id": ptAccountID(account)}, bson.M{"$inc": bson.M{"bal": delta}}); err != nil {
			return Completion{}, err
		}
		return Completion{Success: true}, nil
	}
	var subs []fnats.IJetStreamSubscription
	for _, topic := range []string{"mt.debit", "mt.debit.cancel", "mt.credit", "mt.credit.cancel"} {
		got, err := SubscribeMongoStep(ctx, js, layout, inbox, StepConsumerConfig{Durable: "mt-" + strings.ReplaceAll(topic, ".", "-"), Topic: topic,
			AckWait: 10 * time.Second, MongoWorkers: 16, NakBackoffMin: 50 * time.Millisecond, NakBackoffMax: 200 * time.Millisecond}, handler)
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, got...)
	}
	t.Cleanup(func() {
		for _, sub := range subs {
			sub.Drain()
		}
		for _, sub := range subs {
			select {
			case <-sub.Closed():
			case <-time.After(20 * time.Second):
			}
		}
	})
	return coll
}

// scanReplay 用简化参与方（同进程、回执在内存）快速合成约 messages 条分区日志（不做快照），停掉协调器，再量一个新协调器从头
// 重放到开始服务（Start 返回）的时长。
func scanReplay(t *testing.T, conn realJSConn, options Options, concurrency, messages int) *scanReplayResult {
	t.Helper()
	options.SnapshotEveryEvents, options.SnapshotInterval = 1<<30, time.Hour
	first := startRealEngineWith(t, conn, options, giftDefinition())
	startParticipant(t, conn.log, first, options.Layout, giftDecide)
	sagas := max(messages/6, 1) // 一笔两步正向约 6 条（3 条输入 + 3 条事件）
	var next atomic.Int64
	var wg sync.WaitGroup
	for range concurrency {
		wg.Go(func() {
			for {
				i := next.Add(1)
				if i > int64(sagas) {
					return
				}
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				bk := "r-" + strconv.FormatInt(i, 10)
				if _, err := first.StartSaga(ctx, StartRequest{Type: "gift", BusinessKey: bk, DefinitionVersion: 1}); err != nil {
					t.Errorf("start %s: %v", bk, err)
				} else if _, err := first.WaitTerminal(ctx, IDOf("gift", bk)); err != nil {
					t.Errorf("wait %s: %v", bk, err)
				}
				cancel()
			}
		})
	}
	wg.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	if err := first.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	options.Writer = ""
	second, err := NewEngine(conn.log, conn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Register(giftDefinition()); err != nil {
		t.Fatal(err)
	}
	began := time.Now()
	if err := second.Start(ctx); err != nil {
		t.Fatal(err)
	}
	took := time.Since(began)
	defer func() { _ = second.Stop(ctx) }()
	return &scanReplayResult{Messages: scanStreamMessages(t, conn, options.Layout), Sagas: sagas, StartSeconds: took.Seconds(), Replayed: second.Stats().Replayed}
}

// scanStreamMessages 是布局全部流里现存的消息数。
func scanStreamMessages(t *testing.T, conn realJSConn, layout Layout) uint64 {
	t.Helper()
	nc, err := gonats.Connect(os.Getenv("ROOST_NATS_CLUSTER_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, err := gojs.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	var total uint64
	for _, name := range layout.Streams() {
		stream, err := js.Stream(ctx, name)
		if err != nil {
			t.Fatal(err)
		}
		info, err := stream.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		total += info.State.Msgs
	}
	return total
}
