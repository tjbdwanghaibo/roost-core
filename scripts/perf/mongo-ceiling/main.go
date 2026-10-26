// mongo-ceiling 测本机到某个 Mongo 副本集的原始往返和单文档事务的闭环并发吞吐，作为 Saga 的天花板参照。
// 不启动 Saga / NATS；与 Saga 相同的持久口径（写 majority + j:true，读 majority，主节点）。
//
// 连接串只从环境变量读（-uri-env 指定变量名），不进命令行、不打印；输出里不含主机名。
// 只在 -db-prefix 指定的唯一新库里读写，结束删库；-snapshot 模式只读 serverStatus 的几个计数，不建库。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// latency 是一组成功操作的分位（毫秒）。
type latency struct {
	Samples                            int
	MeanMS, P50MS, P95MS, P99MS, MaxMS float64
}

type row struct {
	Operation      string
	Concurrency    int
	Failed         int
	FirstError     string `json:",omitempty"`
	ElapsedSeconds float64
	OpsPerSecond   float64
	Latency        latency
	// Server 是同一窗口 serverStatus.opLatencies 的服务端平均耗时（不含网络）；没有权限时为空。
	Server   *serverDelta `json:",omitempty"`
	Verified bool
}

// serverDelta 是两次 serverStatus 之间的差值：服务端处理写 / 命令 / 事务的平均微秒数与次数。
type serverDelta struct {
	WriteOps, CommandOps, TxnCommitted int64
	WriteMeanMicros, CommandMeanMicros float64
	WTLogSyncs                         int64
	WTLogSyncMeanMicros                float64
}

type output struct {
	Version     string
	ReplicaSet  bool
	Members     int
	Rows        []row
	ServerError string `json:",omitempty"`
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	uriEnv := flag.String("uri-env", "ROOST_DATAENGINE_IT_MONGO_URI", "environment variable holding the Mongo URI")
	prefix := flag.String("db-prefix", "", "unique database name prefix, must start with roost_sagaperf_")
	levels := flag.String("concurrency", "1,4,16,32,64,128", "comma separated closed-loop concurrency levels")
	duration := flag.Duration("duration", 15*time.Second, "duration per operation/level")
	pings := flag.Int("pings", 200, "sequential ping round trips")
	out := flag.String("output", "", "result JSON path")
	snapshot := flag.Bool("snapshot", false, "only print a serverStatus counter snapshot (JSON) and exit")
	leftover := flag.Bool("leftover", false, "only list databases named with -db-prefix (and drop them with -drop), then exit")
	drop := flag.Bool("drop", false, "with -leftover: drop the listed databases")
	flag.Parse()
	uri := os.Getenv(*uriEnv)
	if uri == "" {
		return fmt.Errorf("environment variable %s is empty", *uriEnv)
	}
	journal := true
	wc := writeconcern.Majority()
	wc.Journal = &journal
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetReadConcern(readconcern.Majority()).SetWriteConcern(wc).
		SetReadPreference(readpref.Primary()).SetMaxPoolSize(256).SetMinPoolSize(8).SetConnectTimeout(10 * time.Second).SetServerSelectionTimeout(10 * time.Second))
	if err != nil {
		return errors.New("Mongo configuration rejected")
	}
	defer client.Disconnect(context.Background())
	if *snapshot {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		raw, err := serverCounters(ctx, client)
		if err != nil {
			return fmt.Errorf("serverStatus unavailable (%T)", err)
		}
		return json.NewEncoder(os.Stdout).Encode(raw)
	}
	if *leftover {
		// 只列出 / 删除本轮前缀的库（authorizedDatabases 让没有全局 listDatabases 权限的账号也能列自己可见的库）。
		if !strings.HasPrefix(*prefix, "roost_sagaperf_") {
			return errors.New("-leftover requires -db-prefix roost_sagaperf_*")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		names, err := client.ListDatabaseNames(ctx, bson.M{"name": bson.M{"$regex": "^" + *prefix}}, options.ListDatabases().SetAuthorizedDatabases(true))
		if err != nil {
			return fmt.Errorf("listDatabases failed (%T)", err)
		}
		for _, name := range names {
			if !strings.HasPrefix(name, *prefix) {
				continue
			}
			status := "kept"
			if *drop {
				status = "dropped"
				if err := client.Database(name).Drop(ctx); err != nil {
					status = fmt.Sprintf("drop failed (%T)", err)
				}
			}
			fmt.Println(name, status)
		}
		fmt.Printf("leftover=%d\n", len(names))
		return nil
	}
	if !strings.HasPrefix(*prefix, "roost_sagaperf_") || *out == "" || *duration < time.Second {
		return errors.New("-db-prefix roost_sagaperf_*, -output and -duration >= 1s are required")
	}
	var concurrency []int
	for _, field := range strings.Split(*levels, ",") {
		n, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || n < 1 || n > 256 {
			return fmt.Errorf("invalid concurrency level %q", field)
		}
		concurrency = append(concurrency, n)
	}
	result := output{}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	var hello bson.M
	err = client.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello)
	cancel()
	if err != nil {
		return fmt.Errorf("hello failed (%T)", err)
	}
	_, result.ReplicaSet = hello["setName"]
	if hosts, ok := hello["hosts"].(bson.A); ok {
		result.Members = len(hosts)
	}
	ctx, cancel = context.WithTimeout(context.Background(), 15*time.Second)
	var build bson.M
	if err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "buildInfo", Value: 1}}).Decode(&build); err == nil {
		result.Version, _ = build["version"].(string)
	}
	cancel()
	db := client.Database(fmt.Sprintf("%s_ceil_%d", *prefix, os.Getpid()))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := db.Drop(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "temporary database cleanup failed:", db.Name())
		}
	}()
	save := func() error {
		raw, _ := json.MarshalIndent(result, "", "  ")
		return os.WriteFile(*out, append(raw, '\n'), 0o600)
	}
	// 1. 原始往返：顺序 ping（admin 上的 ping 命令，不触盘）。
	pingRow := row{Operation: "ping", Concurrency: 1}
	var samples []time.Duration
	begin := time.Now()
	for range *pings {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		start := time.Now()
		err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "ping", Value: 1}}).Err()
		elapsed := time.Since(start)
		cancel()
		if err != nil {
			pingRow.Failed++
			continue
		}
		samples = append(samples, elapsed)
	}
	pingRow.ElapsedSeconds = time.Since(begin).Seconds()
	pingRow.Latency = summarize(samples)
	pingRow.OpsPerSecond = float64(len(samples)) / pingRow.ElapsedSeconds
	pingRow.Verified = pingRow.Failed == 0
	result.Rows = append(result.Rows, pingRow)
	printRow(pingRow)
	if err := save(); err != nil {
		return err
	}
	// 2. 闭环并发：单文档更新（无事务）与单文档事务（一次 $inc + commit），每个 worker 只写自己的文档，避免热点。
	for _, operation := range []string{"update", "txn1"} {
		for _, n := range concurrency {
			r, err := measure(client, db, operation, n, *duration)
			if err != nil {
				return err
			}
			if r.Server == nil && result.ServerError == "" {
				result.ServerError = "serverStatus not permitted or unavailable"
			}
			result.Rows = append(result.Rows, r)
			printRow(r)
			if err := save(); err != nil {
				return err
			}
		}
	}
	return nil
}

func printRow(r row) {
	fmt.Printf("%-6s c=%-4d ops/s=%9.1f p50=%7.2fms p99=%7.2fms max=%8.2fms failed=%d verified=%v",
		r.Operation, r.Concurrency, r.OpsPerSecond, r.Latency.P50MS, r.Latency.P99MS, r.Latency.MaxMS, r.Failed, r.Verified)
	if r.Server != nil {
		fmt.Printf(" server_write_mean=%.2fms server_cmd_mean=%.2fms wt_log_sync_mean=%.2fms syncs=%d",
			r.Server.WriteMeanMicros/1000, r.Server.CommandMeanMicros/1000, r.Server.WTLogSyncMeanMicros/1000, r.Server.WTLogSyncs)
	}
	fmt.Println()
}

func measure(client *mongo.Client, db *mongo.Database, operation string, workers int, duration time.Duration) (row, error) {
	coll := db.Collection(fmt.Sprintf("%s_%d", operation, workers))
	setup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	docs := make([]any, workers)
	for i := range docs {
		docs[i] = bson.M{"_id": i, "value": int64(0), "payload": strings.Repeat("x", 256)}
	}
	_, err := coll.InsertMany(setup, docs)
	cancel()
	if err != nil {
		return row{}, fmt.Errorf("setup failed (%T)", err)
	}
	before, beforeErr := serverCounters(context.Background(), client)
	var mu sync.Mutex
	var all []time.Duration
	failed, firstError := 0, ""
	var wg sync.WaitGroup
	begin := time.Now()
	until := begin.Add(duration)
	for worker := range workers {
		wg.Go(func() {
			var local []time.Duration
			bad, firstBad := 0, ""
			for time.Now().Before(until) {
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				start := time.Now()
				var err error
				switch operation {
				case "update":
					var r *mongo.UpdateResult
					r, err = coll.UpdateOne(ctx, bson.M{"_id": worker}, bson.M{"$inc": bson.M{"value": 1}})
					if err == nil && r.MatchedCount != 1 {
						err = errors.New("missing update target")
					}
				case "txn1":
					var session *mongo.Session
					session, err = client.StartSession()
					if err == nil {
						_, err = session.WithTransaction(ctx, func(tx context.Context) (any, error) {
							r, err := coll.UpdateOne(tx, bson.M{"_id": worker}, bson.M{"$inc": bson.M{"value": 1}})
							if err == nil && r.MatchedCount != 1 {
								err = errors.New("missing update target")
							}
							return nil, err
						})
						session.EndSession(context.Background())
					}
				}
				elapsed := time.Since(start)
				cancel()
				if err != nil {
					bad++
					if firstBad == "" {
						firstBad = fmt.Sprintf("%T", err)
					}
					continue
				}
				local = append(local, elapsed)
			}
			mu.Lock()
			all = append(all, local...)
			failed += bad
			if firstError == "" {
				firstError = firstBad
			}
			mu.Unlock()
		})
	}
	wg.Wait()
	elapsed := time.Since(begin).Seconds()
	r := row{Operation: operation, Concurrency: workers, Failed: failed, FirstError: firstError, ElapsedSeconds: elapsed,
		OpsPerSecond: float64(len(all)) / elapsed, Latency: summarize(all)}
	if beforeErr == nil {
		if after, err := serverCounters(context.Background(), client); err == nil {
			r.Server = delta(before, after)
		}
	}
	// 核对：每个 worker 文档的累计增量之和等于成功次数（WithTransaction 内部重试不会多加）。
	verify, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cursor, err := coll.Find(verify, bson.M{})
	if err != nil {
		return r, fmt.Errorf("verification read failed (%T)", err)
	}
	var values []struct {
		Value int64 `bson:"value"`
	}
	if err := cursor.All(verify, &values); err != nil {
		return r, fmt.Errorf("verification decode failed (%T)", err)
	}
	var sum int64
	for _, v := range values {
		sum += v.Value
	}
	r.Verified = failed == 0 && len(values) == workers && sum == int64(len(all))
	return r, nil
}

func summarize(values []time.Duration) latency {
	if len(values) == 0 {
		return latency{}
	}
	slices.Sort(values)
	ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
	at := func(p float64) float64 { return ms(values[min(len(values)-1, int(float64(len(values)-1)*p+0.5))]) }
	var sum time.Duration
	for _, v := range values {
		sum += v
	}
	return latency{Samples: len(values), MeanMS: ms(sum) / float64(len(values)), P50MS: at(.5), P95MS: at(.95), P99MS: at(.99), MaxMS: ms(values[len(values)-1])}
}

// counters 只取不含主机信息的数值计数。
type counters struct {
	WriteLatency, WriteOps, CommandLatency, CommandOps int64
	TxnCommitted                                       int64
	WTLogSyncs, WTLogSyncMicros                        int64
}

func serverCounters(ctx context.Context, client *mongo.Client) (counters, error) {
	status, err := client.Database("admin").RunCommand(ctx, bson.D{{Key: "serverStatus", Value: 1}}).Raw()
	if err != nil {
		return counters{}, err
	}
	get := func(path ...string) int64 {
		value, err := status.LookupErr(path...)
		if err != nil {
			return 0
		}
		if n, ok := value.AsInt64OK(); ok {
			return n
		}
		if f, ok := value.DoubleOK(); ok {
			return int64(f)
		}
		return 0
	}
	return counters{
		WriteLatency: get("opLatencies", "writes", "latency"), WriteOps: get("opLatencies", "writes", "ops"),
		CommandLatency: get("opLatencies", "commands", "latency"), CommandOps: get("opLatencies", "commands", "ops"),
		TxnCommitted: get("transactions", "totalCommitted"),
		WTLogSyncs:   get("wiredTiger", "log", "log sync operations"), WTLogSyncMicros: get("wiredTiger", "log", "log sync time duration (usecs)"),
	}, nil
}

func delta(before, after counters) *serverDelta {
	d := &serverDelta{WriteOps: after.WriteOps - before.WriteOps, CommandOps: after.CommandOps - before.CommandOps,
		TxnCommitted: after.TxnCommitted - before.TxnCommitted, WTLogSyncs: after.WTLogSyncs - before.WTLogSyncs}
	if d.WriteOps > 0 {
		d.WriteMeanMicros = float64(after.WriteLatency-before.WriteLatency) / float64(d.WriteOps)
	}
	if d.CommandOps > 0 {
		d.CommandMeanMicros = float64(after.CommandLatency-before.CommandLatency) / float64(d.CommandOps)
	}
	if d.WTLogSyncs > 0 {
		d.WTLogSyncMeanMicros = float64(after.WTLogSyncMicros-before.WTLogSyncMicros) / float64(d.WTLogSyncs)
	}
	return d
}
