//go:build integration

package saga

// Mongo 步骤延迟分析（docs/feature/SAGA-MONGO-STEP-LATENCY-2026-10-06.md）。三组基准，都只在设置了
// ROOST_DATAENGINE_IT_MONGO_URI 的真实副本集上运行，库名 roost_mongolat_<pid>_<ns>，用后删除：
//
//   - BenchmarkRealMongoStepLatencyBreakdown：顺序单协程，经计时装饰器统计每次 Handle 的事务数、回调重跑数、
//     每类命令（按集合区分）的次数与客户端耗时；提交耗时 = 事务总耗时 − 回调耗时。服务端耗时另用 slowms=0 的
//     诊断日志看（见文档“测量方法”）。
//   - BenchmarkRealMongoStepThroughput/g=N：N 个协程并发处理新命令，报告 ops/s 与单次 Handle 的 p50 / p99。
//   - BenchmarkRealMongoCommitWriteConcern：裸驱动的单文档事务，按写关注区分提交耗时（评估“Reserve 降写关注”用）。
//
// 文件只依赖 Handle 的公开签名，可以原样复制到修前（9669d181^）的源码树上同口径对照：
//
//	source ~/.roost-it/roost-dataengine-it/env.sh   # 或私有副本集的 URI
//	GOWORK=off go test -tags integration -run '^$' -bench 'BenchmarkRealMongoStep' -benchtime 300x -count 6 ./saga/

import (
	"context"
	"fmt"
	"os"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

func openMongoLatencyDatabase(b *testing.B) (fmongo.IMongo, string, string) {
	b.Helper()
	uri := os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")
	if uri == "" {
		b.Skip("ROOST_DATAENGINE_IT_MONGO_URI is not set")
	}
	client, err := driver.NewClient(fmongo.DefaultConfig(uri), driver.IndexMigrationPolicy{})
	if err != nil {
		b.Fatal(err)
	}
	database := fmt.Sprintf("roost_mongolat_%d_%d", os.Getpid(), time.Now().UnixNano())
	b.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		if err := client.Database(database).Drop(ctx); err != nil {
			b.Errorf("drop %s: %v", database, err)
		}
		_ = client.Close(context.Background())
	})
	return client, database, uri
}

func latencyStepCommand(run string, index int) Command {
	now := time.Now().UTC()
	operation := fmt.Sprintf("lat-%s-%d:1:0", run, index)
	return Command{
		ID: operation + ":1", IdempotencyKey: operation, SagaID: "lat-" + run, SagaType: "gift", DefinitionVersion: 1,
		BusinessKey: "g-1", StepName: "deliver", Phase: PhaseForward, Attempt: 1,
		Topic: "gift.deliver", Payload: []byte("state"), CreatedAt: now, DeadlineAt: now.Add(time.Minute),
	}
}

func latencyInbox(b *testing.B, client fmongo.IMongo, database string) (*MongoCommandInbox, StepHandler) {
	b.Helper()
	inbox, err := NewMongoCommandInbox(client, database, "steps")
	if err != nil {
		b.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(context.Background()); err != nil {
		b.Fatal(err)
	}
	business := client.Database(database).Collection("business")
	handler := func(txCtx context.Context, command Command) (Completion, error) {
		if _, err := business.InsertOne(txCtx, bson.M{"_id": command.ID}); err != nil {
			return Completion{}, err
		}
		return Completion{Success: true}, nil
	}
	return inbox, handler
}

// latencyProbe 记录经过装饰器的每条命令：标签是“所在事务序号 / 命令 / 集合”，事务序号按每次 Handle 重新从 1 数，
// 事务外的命令记为 tx0。只给顺序单协程用。
type latencyProbe struct {
	mu       sync.Mutex
	txIndex  int
	inTx     bool
	counts   map[string]int64
	duration map[string]time.Duration
}

func newLatencyProbe() *latencyProbe {
	return &latencyProbe{counts: map[string]int64{}, duration: map[string]time.Duration{}}
}

func (p *latencyProbe) observe(label string, elapsed time.Duration) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !strings.HasPrefix(label, "tx") {
		tx := 0
		if p.inTx {
			tx = p.txIndex
		}
		label = fmt.Sprintf("tx%d %s", tx, label)
	}
	p.counts[label]++
	p.duration[label] += elapsed
}

func (p *latencyProbe) beginOperation() {
	p.mu.Lock()
	p.txIndex, p.inTx = 0, false
	p.mu.Unlock()
}

type probedMongo struct {
	fmongo.IMongo
	probe *latencyProbe
}

func (m probedMongo) Database(name string) fmongo.IDatabase {
	return probedDatabase{IDatabase: m.IMongo.Database(name), probe: m.probe}
}

func (m probedMongo) StartSession(ctx context.Context) (fmongo.ISession, error) {
	session, err := m.IMongo.StartSession(ctx)
	if err != nil {
		return nil, err
	}
	return probedSession{ISession: session, probe: m.probe}, nil
}

type probedSession struct {
	fmongo.ISession
	probe *latencyProbe
}

func (s probedSession) WithTransaction(ctx context.Context, fn func(context.Context) error) error {
	p := s.probe
	p.mu.Lock()
	p.txIndex++
	p.inTx = true
	tx := p.txIndex
	p.mu.Unlock()
	var callbacks time.Duration
	runs := 0
	start := time.Now()
	err := s.ISession.WithTransaction(ctx, func(txCtx context.Context) error {
		runs++
		callbackStart := time.Now()
		defer func() { callbacks += time.Since(callbackStart) }()
		return fn(txCtx)
	})
	total := time.Since(start)
	p.mu.Lock()
	p.inTx = false
	p.mu.Unlock()
	p.observe(fmt.Sprintf("tx%d total", tx), total)
	p.observe(fmt.Sprintf("tx%d commit(total-callback)", tx), total-callbacks)
	for range runs {
		p.observe(fmt.Sprintf("tx%d callback-run", tx), 0)
	}
	return err
}

type probedDatabase struct {
	fmongo.IDatabase
	probe *latencyProbe
}

func (d probedDatabase) Collection(name string) fmongo.ICollection {
	return probedCollection{ICollection: d.IDatabase.Collection(name), probe: d.probe, name: name}
}

type probedCollection struct {
	fmongo.ICollection
	probe *latencyProbe
	name  string
}

func (c probedCollection) record(op string, start time.Time) {
	c.probe.observe(op+" "+c.name, time.Since(start))
}

func (c probedCollection) InsertOne(ctx context.Context, doc any) (string, error) {
	defer c.record("insert", time.Now())
	return c.ICollection.InsertOne(ctx, doc)
}

func (c probedCollection) FindOne(ctx context.Context, filter any, result any) error {
	defer c.record("findOne", time.Now())
	return c.ICollection.FindOne(ctx, filter, result)
}

func (c probedCollection) Find(ctx context.Context, filter any, results any, opts ...fmongo.FindOption) error {
	defer c.record("find", time.Now())
	return c.ICollection.Find(ctx, filter, results, opts...)
}

func (c probedCollection) UpdateOne(ctx context.Context, filter any, update any) (*fmongo.UpdateResult, error) {
	defer c.record("update", time.Now())
	return c.ICollection.UpdateOne(ctx, filter, update)
}

func (c probedCollection) FindOneAndUpdate(ctx context.Context, filter any, update any, result any, opts ...fmongo.FindOneAndUpdateOption) error {
	defer c.record("findAndModify", time.Now())
	return c.ICollection.FindOneAndUpdate(ctx, filter, update, result, opts...)
}

// BenchmarkRealMongoStepLatencyBreakdown：每次 Handle 的命令构成与耗时分解（顺序单协程、每次新命令）。
// 自定义指标：tx/op（事务数）、cmd/op（经装饰器的数据命令数，不含提交）、commit-ms/op、cmd-ms/op。
func BenchmarkRealMongoStepLatencyBreakdown(b *testing.B) {
	raw, database, _ := openMongoLatencyDatabase(b)
	probe := newLatencyProbe()
	client := probedMongo{IMongo: raw, probe: probe}
	inbox, handler := latencyInbox(b, client, database)
	ctx := context.Background()
	run := fmt.Sprintf("%d", time.Now().UnixNano())
	// 预热：建集合、建守卫路径，不计入。
	for index := range 20 {
		if _, _, err := inbox.Handle(ctx, latencyStepCommand(run+"w", index), handler); err != nil {
			b.Fatal(err)
		}
	}
	probe.mu.Lock()
	probe.counts, probe.duration = map[string]int64{}, map[string]time.Duration{}
	probe.mu.Unlock()
	b.ReportAllocs()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		probe.beginOperation()
		if _, duplicate, err := inbox.Handle(ctx, latencyStepCommand(run, index), handler); err != nil || duplicate {
			b.Fatalf("handle: duplicate=%v err=%v", duplicate, err)
		}
	}
	b.StopTimer()
	probe.mu.Lock()
	defer probe.mu.Unlock()
	n := float64(b.N)
	labels := make([]string, 0, len(probe.counts))
	for label := range probe.counts {
		labels = append(labels, label)
	}
	sort.Strings(labels)
	var txs, commands float64
	var commit, commandTime time.Duration
	lines := []string{fmt.Sprintf("%-44s %8s %10s %10s", "label", "per-op", "mean-us", "us/op")}
	for _, label := range labels {
		count, elapsed := probe.counts[label], probe.duration[label]
		mean := 0.0
		if count > 0 {
			mean = float64(elapsed.Microseconds()) / float64(count)
		}
		lines = append(lines, fmt.Sprintf("%-44s %8.2f %10.1f %10.1f", label, float64(count)/n, mean, float64(elapsed.Microseconds())/n))
		switch {
		case strings.HasSuffix(label, " total"):
			txs += float64(count)
		case strings.HasSuffix(label, "commit(total-callback)"):
			commit += elapsed
		case strings.HasSuffix(label, "callback-run"):
		default:
			commands += float64(count)
			commandTime += elapsed
		}
	}
	b.Logf("breakdown over %d operations:\n%s", b.N, strings.Join(lines, "\n"))
	b.ReportMetric(txs/n, "tx/op")
	b.ReportMetric(commands/n, "cmd/op")
	b.ReportMetric(float64(commit.Microseconds())/1000/n, "commit-ms/op")
	b.ReportMetric(float64(commandTime.Microseconds())/1000/n, "cmd-ms/op")
}

// BenchmarkRealMongoStepThroughput：g 个协程并发处理新命令（各自不同的操作实例），b.N 是总次数。
// ns/op 是墙钟 / 总次数（即吞吐的倒数）；另报 ops/s 与单次 Handle 延迟的 p50 / p99。
func BenchmarkRealMongoStepThroughput(b *testing.B) {
	for _, workers := range []int{1, 8, 32} {
		b.Run(fmt.Sprintf("g=%d", workers), func(b *testing.B) {
			client, database, _ := openMongoLatencyDatabase(b)
			inbox, handler := latencyInbox(b, client, database)
			ctx := context.Background()
			run := fmt.Sprintf("%d", time.Now().UnixNano())
			for index := range 20 {
				if _, _, err := inbox.Handle(ctx, latencyStepCommand(run+"w", index), handler); err != nil {
					b.Fatal(err)
				}
			}
			latencies := make([]time.Duration, b.N)
			var next atomic.Int64
			var failed atomic.Value
			var wg sync.WaitGroup
			b.ResetTimer()
			start := time.Now()
			for range workers {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for {
						index := int(next.Add(1) - 1)
						if index >= b.N {
							return
						}
						opStart := time.Now()
						if _, duplicate, err := inbox.Handle(ctx, latencyStepCommand(run, index), handler); err != nil || duplicate {
							failed.Store(fmt.Sprintf("handle %d: duplicate=%v err=%v", index, duplicate, err))
							return
						}
						latencies[index] = time.Since(opStart)
					}
				}()
			}
			wg.Wait()
			elapsed := time.Since(start)
			b.StopTimer()
			if message := failed.Load(); message != nil {
				b.Fatal(message)
			}
			slices.Sort(latencies)
			percentile := func(q float64) float64 {
				return float64(latencies[int(q*float64(len(latencies)-1))].Microseconds()) / 1000
			}
			b.ReportMetric(float64(b.N)/elapsed.Seconds(), "ops/s")
			b.ReportMetric(percentile(0.50), "p50-ms")
			b.ReportMetric(percentile(0.99), "p99-ms")
		})
	}
}

// BenchmarkRealMongoCommitWriteConcern：裸驱动，一个事务插一份文档后提交，按事务写关注区分。
// 读关注 snapshot、读主，与 mongo/driver 的事务设置相同，只换写关注。
func BenchmarkRealMongoCommitWriteConcern(b *testing.B) {
	_, database, uri := openMongoLatencyDatabase(b)
	cli, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = cli.Disconnect(context.Background()) })
	journal, noJournal := true, false
	cases := []struct {
		name    string
		concern *writeconcern.WriteConcern
	}{
		{"w=majority,j=true", &writeconcern.WriteConcern{W: "majority", Journal: &journal}},
		{"w=majority,j=false", &writeconcern.WriteConcern{W: "majority", Journal: &noJournal}},
		{"w=1,j=true", &writeconcern.WriteConcern{W: 1, Journal: &journal}},
		{"w=1,j=false", &writeconcern.WriteConcern{W: 1, Journal: &noJournal}},
	}
	collection := cli.Database(database).Collection("wc")
	ctx := context.Background()
	if _, err := collection.InsertOne(ctx, bson.M{"_id": "warm"}); err != nil {
		b.Fatal(err)
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			options := options.Transaction().SetReadConcern(readconcern.Snapshot()).SetReadPreference(readpref.Primary()).SetWriteConcern(tc.concern)
			run := time.Now().UnixNano()
			var commit time.Duration
			b.ResetTimer()
			for index := 0; index < b.N; index++ {
				session, err := cli.StartSession()
				if err != nil {
					b.Fatal(err)
				}
				if err := session.StartTransaction(options); err != nil {
					b.Fatal(err)
				}
				sctx := mongo.NewSessionContext(ctx, session)
				if _, err := collection.InsertOne(sctx, bson.M{"_id": fmt.Sprintf("%s-%d-%d", tc.name, run, index)}); err != nil {
					b.Fatal(err)
				}
				commitStart := time.Now()
				if err := session.CommitTransaction(ctx); err != nil {
					b.Fatal(err)
				}
				commit += time.Since(commitStart)
				session.EndSession(ctx)
			}
			b.ReportMetric(float64(commit.Microseconds())/1000/float64(b.N), "commit-ms/op")
		})
	}
}
