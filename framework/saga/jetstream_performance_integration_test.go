//go:build integration

package saga

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"go.mongodb.org/mongo-driver/v2/bson"
	"os"
	"slices"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	gonats "github.com/nats-io/nats.go"
	gojs "github.com/nats-io/nats.go/jetstream"
	fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
	ndriver "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
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
