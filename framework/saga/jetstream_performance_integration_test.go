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
// handler 用 DurabilityPipelined；缺省 Nest 没开 pipelined 异步完成，worker 仍等票据）、mongo-forward、admission（准入开，高 5000 /
// 低 2500，投影每条记录多等 ROOST_SAGA_SCAN_PROJECT_DELAY_US，缺省 10ms，让积压越过高水位）、snapshot（每 2000 事件快照）、
// replay（合成 ROOST_SAGA_SCAN_REPLAY_MESSAGES 条日志后量一次全量重放的启动时长）。
//
// 其他旋钮：ROOST_SAGA_SCAN_NEST_WORKERS（参与方 Nest short 池，缺省 64 同原型；strict 提交锁内等 fsync，group commit 一批至多
// 这么多条，见 ptParticipantOptions）、ROOST_SAGA_SCAN_PARTITIONS（P，缺省 16，G 固定 4）。结果另记定位计数：WAL 记录 / fsync、
// 分区事件 / 发布次数、参与方 Nest short 通道的忙 / 等待统计（见 scanResult）；准入档另记关闭 / 重开次数、关闭占比、关闭期间的有效吞吐，并加判据：关过又重开过、积压峰值 ≤ 1.3 × 高水位、
// 关闭期间有效吞吐不为 0（设计 §12）。ROOST_SAGA_SCAN_PIPELINED_ASYNC=1 配任一原生参与方的场景（native-*、admission、
// snapshot）：步骤 handler 改为 DurabilityPipelined 并开 Nest pipelined 异步完成（ptPipelinedAsyncEnv），派发 worker 不再等票据——
// 这是原生步骤的生产推荐（维护者 2026-10-29），scripts/perf/saga-js-scan.sh 的缺省原生档就这样跑；mongo-forward / replay 没有
// Nest 参与方，设了拒绝。
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
	// 参与方 Nest short 池缺省 64，同原型压测（saga-jslog-proto perf_test.go 的 Workers: 64）：strict 提交锁内等 fsync，一次
	// group commit 至多带 short 池 worker 数条记录，8 个 worker 时 c128 封顶约 500 笔/s（2026-10-29 定位，ptParticipantOptions）。
	nestWorkers := integer("ROOST_SAGA_SCAN_NEST_WORKERS", 64, 1, 1024)
	// 准入档缺省让投影每条记录多等 10ms（8 通道约 800 条/s，低于加压时的产出），积压才会越过高水位、准入真正关闭再重开。
	projectDelayDefault := 0
	if scenario == "admission" {
		projectDelayDefault = 10_000
	}
	projectDelay := time.Duration(integer("ROOST_SAGA_SCAN_PROJECT_DELAY_US", projectDelayDefault, 0, 1_000_000)) * time.Microsecond
	pipelinedAsync := ptPipelinedAsyncFromEnv()
	if pipelinedAsync && (scenario == "mongo-forward" || scenario == "replay") {
		t.Fatalf("%s=1 only applies to scenarios with a native participant, got %q", ptPipelinedAsyncEnv, scenario)
	}
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
	partitions := integer("ROOST_SAGA_SCAN_PARTITIONS", 16, 4, 64)
	layout := realLayout(t, url, partitions, 4)
	conn, js := ptJetStream(t, url)
	result := scanResult{Partitions: partitions, Scenario: scenario, Concurrency: concurrency, Seconds: seconds, Warmup: warmup, GOMAXPROCS: runtime.GOMAXPROCS(0)}
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
	counted := &scanCountingLog{JetStreamLog: conn.log}
	engine := startScanEngine(t, counted, conn, options, ptDefinition(), scanMongoDefinition())
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
		if pipelinedAsync {
			durability = nest.DurabilityPipelined // openPtParticipantWith 按同一个环境变量改，这里只为结果记录
		}
		result.NestWorkers, result.ProjectDelayUs, result.PipelinedAsync, result.Durability = nestWorkers, projectDelay.Microseconds(), pipelinedAsync, durability.String()
		participant, err = openPtParticipantWith(t.TempDir(), mongo, database, accounts,
			ptParticipantOptions{Durability: durability, NestWorkers: nestWorkers, ProjectDelayPerRecord: projectDelay, PipelinedAsync: pipelinedAsync})
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
	// 准入采样（每 50ms 看一次本协调器的判定）：关闭的采样数、开→关与关→开的次数；closedNow 给发起方标记“在关闭期间到终态”。
	var closedNow atomic.Bool
	var closedSamples, allSamples, closures, reopens atomic.Int64
	sampleDone := make(chan struct{})
	sampleCtx, stopSampling := context.WithCancel(ctx)
	go func() {
		defer close(sampleDone)
		for sampleCtx.Err() == nil {
			if b := backlog(); b > peak.Load() {
				peak.Store(b)
			}
			if options.Admission.Enabled {
				closed := !engine.AdmissionState().Open
				switch was := closedNow.Swap(closed); {
				case closed && !was:
					closures.Add(1)
				case !closed && was:
					reopens.Add(1)
				}
				allSamples.Add(1)
				if closed {
					closedSamples.Add(1)
				}
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
	var closedTerminals atomic.Int64

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
	walStats := func() (appended, syncs uint64) {
		if participant == nil {
			return 0, 0
		}
		s := participant.wal.Stats()
		return s.Appended, s.Syncs
	}
	shortLane := func() nest.DispatchLaneStats {
		if participant == nil {
			return nest.DispatchLaneStats{}
		}
		return participant.mgr.Stats().Queue.Short
	}
	appended0, syncs0 := walStats()
	short0 := shortLane()
	events0 := engine.Stats().Events
	rounds0, roundEvents0 := counted.rounds.Load(), counted.roundEvents.Load()
	start := time.Now()
	deadline := start.Add(time.Duration(seconds) * time.Second)
	// short 通道采样（稳态窗口内每 50ms 一次）：执行中（占 worker）、就绪等 worker、等同 ID 前驱的作业数。别的实体的作业
	// 与步骤共用这个通道，执行中 / worker 数就是它们看到的 worker 占用，就绪等 worker 是它们排队的深度。
	var shortSamples, shortRunning, shortWaiting, shortBlocked atomic.Int64
	shortDone := make(chan struct{})
	go func() {
		defer close(shortDone)
		steady := start.Add(time.Duration(warmup) * time.Second)
		for participant != nil && time.Now().Before(deadline) {
			if time.Now().After(steady) {
				lane := shortLane()
				shortSamples.Add(1)
				shortRunning.Add(int64(lane.Running))
				shortWaiting.Add(int64(lane.WaitingForWorker))
				shortBlocked.Add(int64(lane.BlockedOnPredecessor))
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()
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
				if closedNow.Load() && time.Now().Before(deadline) {
					closedTerminals.Add(1)
				}
				mu.Lock()
				done = append(done, finished{bk: bk, transfer: transfer, status: final.Status, at: time.Now(), latency: time.Since(began)})
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	<-shortDone
	loadEnded := time.Now()
	appended1, syncs1 := walStats()
	result.WALAppended, result.WALSyncs = appended1-appended0, syncs1-syncs0
	if short1 := shortLane(); participant != nil {
		result.ShortStarted = short1.Started - short0.Started
		if result.ShortStarted > 0 {
			result.ShortWorkerWaitMeanUs = float64((short1.WorkerWait - short0.WorkerWait).Microseconds()) / float64(result.ShortStarted)
			result.ShortDependencyWaitMeanUs = float64((short1.DependencyWait - short0.DependencyWait).Microseconds()) / float64(result.ShortStarted)
		}
		result.ShortMaxWorkerWaitMs = float64(short1.MaxWorkerWait) / float64(time.Millisecond)
		result.ShortPeakWaiting = short1.PeakWaiting
		if n := shortSamples.Load(); n > 0 {
			result.ShortSamples = n
			result.ShortRunningAvg = float64(shortRunning.Load()) / float64(n)
			result.ShortWaitingForWorkerAvg = float64(shortWaiting.Load()) / float64(n)
			result.ShortBlockedAvg = float64(shortBlocked.Load()) / float64(n)
			result.ShortBusyFraction = result.ShortRunningAvg / float64(nestWorkers)
		}
	}
	result.CoordinatorEvents = engine.Stats().Events - events0
	result.CoordinatorPublishes, result.CoordinatorPublishedEvents = counted.rounds.Load()-rounds0, counted.roundEvents.Load()-roundEvents0
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
	if options.Admission.Enabled {
		result.AdmissionHigh, result.AdmissionClosures, result.AdmissionReopens = options.Admission.HighRecords, closures.Load(), reopens.Load()
		if n := allSamples.Load(); n > 0 {
			result.AdmissionClosedFraction = float64(closedSamples.Load()) / float64(n)
		}
		if closedSeconds := float64(closedSamples.Load()) * 0.05; closedSeconds > 0 {
			result.ClosedTerminalsPerSecond = float64(closedTerminals.Load()) / closedSeconds
		}
	}

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
	// 准入档的判据（设计 §12）：准入真的关过又重开过、积压峰值 ≤ 1.3 × 高水位、关闭期间有效吞吐不为 0。
	if options.Admission.Enabled && (result.AdmissionClosures == 0 || result.AdmissionReopens == 0 ||
		float64(result.BacklogPeak) > 1.3*float64(result.AdmissionHigh) || result.ClosedTerminalsPerSecond <= 0) {
		t.Fatalf("admission gate failed: closures %d reopens %d backlog peak %d (high %d) closed-time terminals %.1f/s",
			result.AdmissionClosures, result.AdmissionReopens, result.BacklogPeak, result.AdmissionHigh, result.ClosedTerminalsPerSecond)
	}
}

// scanResult 是一档的结果（脚本汇总它）。
type scanResult struct {
	Scenario          string            `json:"scenario"`
	Concurrency       int               `json:"concurrency"`
	Seconds           int               `json:"seconds"`
	Warmup            int               `json:"warmup"`
	GOMAXPROCS        int               `json:"gomaxprocs"`
	Partitions        int               `json:"partitions"`
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
	// 定位用计数（加压期间的增量）：参与方 Nest short 池 worker 数、WAL 追加记录与 fsync 次数（记录 / fsync 即组提交批量）、
	// 协调器提交的事件数。
	NestWorkers       int    `json:"nest_workers,omitempty"`
	Durability        string `json:"durability,omitempty"` // 原生参与方步骤 handler 的 durability
	PipelinedAsync    bool   `json:"pipelined_async,omitempty"`
	ProjectDelayUs    int64  `json:"project_delay_us,omitempty"`
	WALAppended       uint64 `json:"wal_appended,omitempty"`
	WALSyncs          uint64 `json:"wal_syncs,omitempty"`
	CoordinatorEvents uint64 `json:"coordinator_events,omitempty"`
	// 分区事件的 fenced 发布次数与条数（条数 / 次数即一轮一次发布带的事件数，设计 §4.4 的批）。
	CoordinatorPublishes       uint64 `json:"coordinator_publishes,omitempty"`
	CoordinatorPublishedEvents uint64 `json:"coordinator_published_events,omitempty"`
	// 准入档：高水位、开→关 / 关→开次数、关闭时间占比、关闭期间每秒到终态的笔数（加压结束后的不计）。
	AdmissionHigh            uint64  `json:"admission_high_records,omitempty"`
	AdmissionClosures        int64   `json:"admission_closures,omitempty"`
	AdmissionReopens         int64   `json:"admission_reopens,omitempty"`
	AdmissionClosedFraction  float64 `json:"admission_closed_fraction,omitempty"`
	ClosedTerminalsPerSecond float64 `json:"closed_terminals_per_second,omitempty"`
	// 参与方 Nest short 通道（原生档）：加压期间开始执行的作业数与每个作业平均等 worker / 等同 ID 前驱的时长（DispatchLaneStats
	// 累计值的增量）；worker 等待最大值与等待峰值是引擎启动以来的累计；稳态窗口采样的平均执行中 / 等 worker / 等前驱的作业数，
	// 忙占比 = 平均执行中 / worker 数。
	ShortStarted              uint64  `json:"short_started,omitempty"`
	ShortWorkerWaitMeanUs     float64 `json:"short_worker_wait_mean_us,omitempty"`
	ShortDependencyWaitMeanUs float64 `json:"short_dependency_wait_mean_us,omitempty"`
	ShortMaxWorkerWaitMs      float64 `json:"short_max_worker_wait_ms,omitempty"`
	ShortPeakWaiting          int     `json:"short_peak_waiting,omitempty"`
	ShortSamples              int64   `json:"short_samples,omitempty"`
	ShortRunningAvg           float64 `json:"short_running_avg,omitempty"`
	ShortWaitingForWorkerAvg  float64 `json:"short_waiting_for_worker_avg,omitempty"`
	ShortBlockedAvg           float64 `json:"short_blocked_avg,omitempty"`
	ShortBusyFraction         float64 `json:"short_busy_fraction,omitempty"`
}

// scanCountingLog 数分区事件的发布（带事件过滤的 fenced 发布；启动等输入的发布不带过滤，不计）。
type scanCountingLog struct {
	fnats.JetStreamLog
	rounds, roundEvents atomic.Uint64
}

func (l *scanCountingLog) PublishFenced(ctx context.Context, msgs []fnats.LogMsg, fence fnats.LogFence) (fnats.LogPublishAck, error) {
	if fence.Filter != "" {
		l.rounds.Add(1)
		l.roundEvents.Add(uint64(len(msgs)))
	}
	return l.JetStreamLog.PublishFenced(ctx, msgs, fence)
}

// startScanEngine 同 startRealEngineWith，日志换成 log（计数包装）。
func startScanEngine(t *testing.T, log fnats.JetStreamLog, conn realJSConn, options Options, definitions ...Definition) *Engine {
	t.Helper()
	e, err := NewEngine(log, conn.client, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range definitions {
		if err := e.Register(d); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := e.Start(ctx); err != nil {
		t.Fatalf("start: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_ = e.Stop(ctx)
	})
	return e
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
	if result.Sagas > 0 && result.WALSyncs > 0 {
		t.Logf("nest_workers=%d wal_records/fsync=%.2f wal_fsync/saga=%.3f coordinator_events/saga=%.2f events/publish=%.2f publishes/saga=%.2f backlog_peak=%d admission_rejected=%d",
			result.NestWorkers, float64(result.WALAppended)/float64(result.WALSyncs), float64(result.WALSyncs)/float64(result.Sagas),
			float64(result.CoordinatorEvents)/float64(result.Sagas), float64(result.CoordinatorPublishedEvents)/float64(max(result.CoordinatorPublishes, 1)),
			float64(result.CoordinatorPublishes)/float64(result.Sagas), result.BacklogPeak, result.AdmissionRejected)
	}
	if result.ShortSamples > 0 {
		t.Logf("pipelined_async=%v short: started=%d busy=%.2f running_avg=%.2f waiting_for_worker_avg=%.2f blocked_avg=%.2f worker_wait_mean=%.0fus dependency_wait_mean=%.0fus max_worker_wait=%.1fms peak_waiting=%d",
			result.PipelinedAsync, result.ShortStarted, result.ShortBusyFraction, result.ShortRunningAvg, result.ShortWaitingForWorkerAvg, result.ShortBlockedAvg,
			result.ShortWorkerWaitMeanUs, result.ShortDependencyWaitMeanUs, result.ShortMaxWorkerWaitMs, result.ShortPeakWaiting)
	}
	if result.AdmissionHigh > 0 {
		t.Logf("admission high=%d closures=%d reopens=%d closed=%.0f%% closed_terminals=%.1f/s backlog_peak=%d (%.2f x high) rejected=%d start_retries=%d",
			result.AdmissionHigh, result.AdmissionClosures, result.AdmissionReopens, 100*result.AdmissionClosedFraction, result.ClosedTerminalsPerSecond,
			result.BacklogPeak, float64(result.BacklogPeak)/float64(result.AdmissionHigh), result.AdmissionRejected, result.StartRetries)
	}
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
