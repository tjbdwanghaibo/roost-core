// saga-claim-explain 在指定积压下对 Saga 协调器的两条领取查询做 explain("executionStats") 并实测单次领取耗时，
// 对照现行索引与候选索引 / 排序键。查询形状照抄 framework/saga/mongo_store.go 的 ClaimDue / ClaimOutbox
// （filter + sort + update，findAndModify 形式），索引照抄 MongoStore.EnsureInfrastructure。只读写 -db-prefix 下的新库，
// 结束删库；连接串只从环境变量读，不打印，输出不含主机名。
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
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// variant 是一种“索引 + 排序”组合。Extra 是在现行索引之外再建的候选索引。
type variant struct {
	Name  string
	Query string // "sagas" 或 "outbox"
	Extra bson.D
	Sort  bson.D
}

type result struct {
	Variant, Query                       string
	Backlog                              int
	HasSort, HasSortMerge                bool
	WinningIndex                         string
	Stages                               []string
	NReturned                            int64
	TotalKeysExamined, TotalDocsExamined int64
	ExecutionTimeMillis                  int64
	// ClaimMeanMS / ClaimP50MS / ClaimP99MS：真实 FindOneAndUpdate 领取（含网络往返）连续 Claims 次的耗时。
	Claims                              int
	ClaimMeanMS, ClaimP50MS, ClaimP99MS float64
}

const (
	statusPending      = 1
	statusWaiting      = 2
	statusCompensating = 3
	statusCompleted    = 4
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	uriEnv := flag.String("uri-env", "ROOST_DATAENGINE_IT_MONGO_URI", "environment variable holding the Mongo URI")
	prefix := flag.String("db-prefix", "", "unique database name prefix, must start with roost_sagaperf_")
	backlogs := flag.String("backlog", "1000,10000", "comma separated due-backlog sizes")
	claims := flag.Int("claims", 200, "real claims timed per case")
	out := flag.String("output", "", "result JSON path")
	flag.Parse()
	uri := os.Getenv(*uriEnv)
	if uri == "" || !strings.HasPrefix(*prefix, "roost_sagaperf_") || *out == "" {
		return errors.New("URI environment, -db-prefix roost_sagaperf_* and -output are required")
	}
	journal := true
	wc := writeconcern.Majority()
	wc.Journal = &journal
	client, err := mongo.Connect(options.Client().ApplyURI(uri).SetReadConcern(readconcern.Majority()).SetWriteConcern(wc).
		SetReadPreference(readpref.Primary()).SetConnectTimeout(10 * time.Second).SetServerSelectionTimeout(10 * time.Second))
	if err != nil {
		return errors.New("Mongo configuration rejected")
	}
	defer client.Disconnect(context.Background())
	db := client.Database(fmt.Sprintf("%s_claimx_%d", *prefix, os.Getpid()))
	defer func() {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		if err := db.Drop(ctx); err != nil {
			fmt.Fprintln(os.Stderr, "temporary database cleanup failed:", db.Name())
		}
	}()
	variants := []variant{
		{Name: "current", Query: "sagas", Sort: bson.D{{Key: "next_run_at", Value: 1}, {Key: "_id", Value: 1}}},
		{Name: "index_status_next_id", Query: "sagas", Extra: bson.D{{Key: "status", Value: 1}, {Key: "next_run_at", Value: 1}, {Key: "_id", Value: 1}}, Sort: bson.D{{Key: "next_run_at", Value: 1}, {Key: "_id", Value: 1}}},
		{Name: "sort_without_id", Query: "sagas", Sort: bson.D{{Key: "next_run_at", Value: 1}}},
		{Name: "current", Query: "outbox", Sort: bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "_id", Value: 1}}},
		{Name: "index_next_id", Query: "outbox", Extra: bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "_id", Value: 1}}, Sort: bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "_id", Value: 1}}},
		{Name: "sort_without_id", Query: "outbox", Sort: bson.D{{Key: "next_attempt_at", Value: 1}}},
	}
	var rows []result
	for _, field := range strings.Split(*backlogs, ",") {
		backlog, err := strconv.Atoi(strings.TrimSpace(field))
		if err != nil || backlog < 1 || backlog > 200000 {
			return fmt.Errorf("invalid backlog %q", field)
		}
		for _, v := range variants {
			row, err := measure(db, v, backlog, *claims)
			if err != nil {
				return fmt.Errorf("%s/%s/%d: %w", v.Query, v.Name, backlog, err)
			}
			rows = append(rows, row)
			fmt.Printf("%-6s %-22s backlog=%-6d sort=%-5v sort_merge=%-5v index=%-22s keys=%-6d docs=%-6d exec_ms=%-4d claim_mean=%.2fms p50=%.2fms p99=%.2fms\n",
				row.Query, row.Variant, row.Backlog, row.HasSort, row.HasSortMerge, row.WinningIndex, row.TotalKeysExamined, row.TotalDocsExamined, row.ExecutionTimeMillis, row.ClaimMeanMS, row.ClaimP50MS, row.ClaimP99MS)
			raw, _ := json.MarshalIndent(rows, "", "  ")
			if err := os.WriteFile(*out, append(raw, '\n'), 0o600); err != nil {
				return err
			}
		}
	}
	return nil
}

func measure(db *mongo.Database, v variant, backlog, claims int) (result, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	coll := db.Collection(fmt.Sprintf("%s_%s_%d", v.Query, v.Name, backlog))
	// 现行索引：照抄 MongoStore.EnsureInfrastructure（mongo_store.go:82～94）。
	var indexes []mongo.IndexModel
	if v.Query == "sagas" {
		indexes = []mongo.IndexModel{
			{Keys: bson.D{{Key: "type", Value: 1}, {Key: "business_key", Value: 1}}, Options: options.Index().SetName("uniq_type_business").SetUnique(true)},
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "next_run_at", Value: 1}, {Key: "lease_until", Value: 1}}, Options: options.Index().SetName("claim_due")},
			{Keys: bson.D{{Key: "status", Value: 1}, {Key: "updated_at", Value: -1}}, Options: options.Index().SetName("operations")},
			{Keys: bson.D{{Key: "type", Value: 1}, {Key: "updated_at", Value: -1}}, Options: options.Index().SetName("operations_by_type")},
			{Keys: bson.D{{Key: "type", Value: 1}, {Key: "definition_version", Value: 1}, {Key: "status", Value: 1}, {Key: "updated_at", Value: -1}}, Options: options.Index().SetName("operations_by_definition")},
		}
	} else {
		indexes = []mongo.IndexModel{
			{Keys: bson.D{{Key: "next_attempt_at", Value: 1}, {Key: "lease_until", Value: 1}}, Options: options.Index().SetName("claim_due")},
			{Keys: bson.D{{Key: "saga_id", Value: 1}, {Key: "created_at", Value: 1}}, Options: options.Index().SetName("by_saga")},
			{Keys: bson.D{{Key: "command.idempotency_key", Value: 1}}, Options: options.Index().SetName("by_operation")},
		}
	}
	if v.Extra != nil {
		indexes = append(indexes, mongo.IndexModel{Keys: v.Extra, Options: options.Index().SetName("candidate")})
	}
	if _, err := coll.Indexes().CreateMany(ctx, indexes); err != nil {
		return result{}, fmt.Errorf("create indexes (%T)", err)
	}
	// 积压：backlog 条到期待领取（三种非终态轮流，到期时刻错开），另加同样多的已终态 / 已领取记录作为历史。
	now := time.Now().UTC()
	epoch := time.Unix(0, 0).UTC()
	batch := make([]any, 0, 1000)
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		_, err := coll.InsertMany(ctx, batch)
		batch = batch[:0]
		return err
	}
	for i := range 2 * backlog {
		due := i < backlog
		at := now.Add(-time.Duration(backlog-i%backlog) * time.Millisecond)
		var doc bson.M
		if v.Query == "sagas" {
			status := []int{statusPending, statusWaiting, statusCompensating}[i%3]
			lease := epoch
			if !due {
				status = statusCompleted
			}
			doc = bson.M{"_id": fmt.Sprintf("saga-%08d", i), "type": "explain", "business_key": strconv.Itoa(i), "definition_version": 1, "status": status,
				"next_run_at": at, "lease_until": lease, "updated_at": now, "payload": strings.Repeat("x", 200)}
		} else {
			lease := epoch
			if !due {
				lease = now.Add(time.Hour) // 已被领取、未 ack 的记录
			}
			doc = bson.M{"_id": fmt.Sprintf("cmd-%08d", i), "saga_id": fmt.Sprintf("saga-%08d", i), "created_at": now, "next_attempt_at": at, "lease_until": lease,
				"command": bson.M{"idempotency_key": fmt.Sprintf("op-%08d", i), "payload": strings.Repeat("x", 200)}}
		}
		batch = append(batch, doc)
		if len(batch) == cap(batch) {
			if err := flush(); err != nil {
				return result{}, fmt.Errorf("seed (%T)", err)
			}
		}
	}
	if err := flush(); err != nil {
		return result{}, fmt.Errorf("seed (%T)", err)
	}
	claimNow := time.Now().UTC()
	var filter bson.M
	if v.Query == "sagas" {
		filter = bson.M{"status": bson.M{"$in": []int{statusPending, statusWaiting, statusCompensating}}, "next_run_at": bson.M{"$lte": claimNow}, "lease_until": bson.M{"$lte": claimNow}}
	} else {
		filter = bson.M{"next_attempt_at": bson.M{"$lte": claimNow}, "lease_until": bson.M{"$lte": claimNow}}
	}
	update := bson.M{"$set": bson.M{"lease_owner": "explain", "lease_until": claimNow.Add(time.Minute)}, "$inc": bson.M{"lease_token": 1}}
	explain := bson.D{{Key: "explain", Value: bson.D{{Key: "findAndModify", Value: coll.Name()}, {Key: "query", Value: filter}, {Key: "sort", Value: v.Sort}, {Key: "update", Value: update}, {Key: "new", Value: true}}},
		{Key: "verbosity", Value: "executionStats"}}
	raw, err := db.RunCommand(ctx, explain).Raw()
	if err != nil {
		return result{}, fmt.Errorf("explain (%T: %v)", err, err)
	}
	row := result{Variant: v.Name, Query: v.Query, Backlog: backlog}
	if plan, err := raw.LookupErr("queryPlanner", "winningPlan"); err == nil {
		walk(plan, &row)
	}
	lookup := func(path ...string) int64 {
		value, err := raw.LookupErr(path...)
		if err != nil {
			return -1
		}
		if n, ok := value.AsInt64OK(); ok {
			return n
		}
		return -1
	}
	row.NReturned = lookup("executionStats", "nReturned")
	row.TotalKeysExamined = lookup("executionStats", "totalKeysExamined")
	row.TotalDocsExamined = lookup("executionStats", "totalDocsExamined")
	row.ExecutionTimeMillis = lookup("executionStats", "executionTimeMillis")
	// 真实领取：同一 filter / sort / update，每次一条，与 ClaimDue 循环相同；被领取的记录不再到期，积压逐次减一。
	var samples []time.Duration
	for range min(claims, backlog) {
		start := time.Now()
		var doc bson.M
		err := coll.FindOneAndUpdate(ctx, filter, update, options.FindOneAndUpdate().SetSort(v.Sort).SetReturnDocument(options.After)).Decode(&doc)
		if err != nil {
			return row, fmt.Errorf("claim (%T)", err)
		}
		samples = append(samples, time.Since(start))
	}
	if len(samples) > 0 {
		slices.Sort(samples)
		var sum time.Duration
		for _, s := range samples {
			sum += s
		}
		ms := func(d time.Duration) float64 { return float64(d) / float64(time.Millisecond) }
		row.Claims = len(samples)
		row.ClaimMeanMS = ms(sum) / float64(len(samples))
		row.ClaimP50MS = ms(samples[(len(samples)-1)/2])
		row.ClaimP99MS = ms(samples[min(len(samples)-1, int(float64(len(samples)-1)*.99+0.5))])
	}
	if err := coll.Drop(ctx); err != nil {
		return row, fmt.Errorf("drop collection (%T)", err)
	}
	return row, nil
}

// walk 记录胜出计划的阶段名，是否有阻塞的 SORT（内存排序）或 SORT_MERGE（多段索引按序归并，不是内存排序），以及所用索引。
func walk(value bson.RawValue, row *result) {
	doc, ok := value.DocumentOK()
	if !ok {
		return
	}
	if stage, ok := doc.Lookup("stage").StringValueOK(); ok {
		row.Stages = append(row.Stages, stage)
		switch stage {
		case "SORT":
			row.HasSort = true
		case "SORT_MERGE":
			row.HasSortMerge = true
		}
	}
	if name, ok := doc.Lookup("indexName").StringValueOK(); ok && row.WinningIndex == "" {
		row.WinningIndex = name
	}
	for _, key := range []string{"inputStage", "queryPlan"} {
		if child, err := doc.LookupErr(key); err == nil {
			walk(child, row)
		}
	}
	if children, err := doc.LookupErr("inputStages"); err == nil {
		if array, ok := children.ArrayOK(); ok {
			values, _ := array.Values()
			for _, child := range values {
				walk(child, row)
			}
		}
	}
}
