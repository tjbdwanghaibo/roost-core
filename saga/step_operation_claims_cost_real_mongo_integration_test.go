//go:build integration

package saga

// RR-20261006-15 / -16 的服务端代价：单个操作实例累积 1000 / 4000 / 10000 份 claim 时，Reserve 与 operationSuccess
// 按操作查询的耗时与执行计划（explain executionStats）。只在 ROOST_SAGA_CLAIM_COST=1 时运行，结果打印成表，
// 数字记在 docs/bugfix/RR-20261006-15.md。
//
//	source <私有副本集 env.sh>
//	ROOST_SAGA_CLAIM_COST=1 GOWORK=off go test -tags integration -count=1 -v -run '^TestRealMongoOperationClaimsQueryCost$' ./saga/
//
// 夹具：一份真实 Handle 写的可重试失败 claim 复制成 N 份（分摊在五生里），即 RR-15 的累积形态；legacy 一组去掉
// outcome，是升级前写的形态（RR-16，只能全部读出来，最多 8192 份）。可选的 extra 索引用来对照是否值得加索引。

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"testing"
	"time"

	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

func TestRealMongoOperationClaimsQueryCost(t *testing.T) {
	if os.Getenv("ROOST_SAGA_CLAIM_COST") != "1" {
		t.Skip("ROOST_SAGA_CLAIM_COST=1 runs the measurement")
	}
	client, database := realSagadirMongo(t)
	ctx := context.Background()
	raw, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Disconnect(ctx)

	type fixture struct {
		name       string
		claims     int
		legacy     bool
		background int  // 同一集合里其他操作实例的 claim 数（生产集合不只一个操作）
		index      bool // 有 by_operation_decision；false 时删掉它，即 RR-16 修复之前的索引
	}
	var fixtures []fixture
	for _, index := range []bool{false, true} {
		for _, background := range []int{0, 50000} {
			for _, n := range []int{1000, 4000, 10000} {
				fixtures = append(fixtures, fixture{"outcome", n, false, background, index})
			}
			for _, n := range []int{1000, 4097} {
				fixtures = append(fixtures, fixture{"legacy", n, true, background, index})
			}
		}
	}
	const iterations = 60
	t.Logf("%-8s %5s %6s %6s | %-60s | %6s %6s %5s | %8s %8s | %8s %8s | %8s %8s", "fixture", "index", "bg", "claims", "plan (reserve query)", "keys", "docs", "ret",
		"reserve50", "p90", "opSucc50", "p90", "query50", "pre-fix")
	for i, f := range fixtures {
		collection := fmt.Sprintf("cost%d", i)
		inbox, err := NewMongoCommandInbox(client, database, collection)
		if err != nil {
			t.Fatal(err)
		}
		if err := inbox.EnsureInfrastructure(ctx); err != nil {
			t.Fatal(err)
		}
		claims := client.Database(database).Collection(collection + mongoInboxClaimSuffix)
		rawClaims := raw.Database(database).Collection(collection + mongoInboxClaimSuffix)
		if !f.index {
			if err := rawClaims.Indexes().DropOne(ctx, "by_operation_decision"); err != nil {
				t.Fatal(err)
			}
		}
		operation := "gift-1:1:0"
		command := func(incarnation, attempt uint32) Command {
			c := mongoStepCommand(operation, attempt, time.Now().Add(time.Hour))
			c.ID = commandID(operation, incarnation, attempt)
			return c
		}
		retryable := func(context.Context, Command) (Completion, error) {
			return Completion{Retryable: true, Error: "dependency down"}, nil
		}
		if _, _, err := inbox.Handle(ctx, command(0, 1), retryable); err != nil {
			t.Fatal(err)
		}
		var template bson.M
		if err := claims.FindOne(ctx, bson.M{"_id": stepClaimID(command(0, 1).ID)}, &template); err != nil {
			t.Fatal(err)
		}
		if f.legacy {
			delete(template, "outcome")
			if _, err := claims.UpdateOne(ctx, bson.M{"_id": template["_id"]}, bson.M{"$unset": bson.M{"outcome": ""}}); err != nil {
				t.Fatal(err)
			}
		}
		// 其他操作实例：每个 25 份，可重试失败、成功、被接替、pending 混在一起。
		for start := 0; start < f.background; start += 5000 {
			batch := []any{}
			for n := start; n < f.background && n < start+5000; n++ {
				other := fmt.Sprintf("gift-%d:1:0", 1000+n/25)
				id := commandID(other, 0, uint32(n%25)+1)
				clone := bson.M{}
				for key, value := range template {
					clone[key] = value
				}
				clone["_id"], clone["command_id"], clone["operation_key"] = stepClaimID(id), id, other
				switch n % 25 {
				case 0:
					clone["status"], clone["outcome"] = claimStatusCompleted, claimOutcomeSuccess
				case 1:
					clone["status"] = claimStatusPending
					delete(clone, "outcome")
				case 2, 3:
					clone["status"] = claimStatusSuperseded
					delete(clone, "outcome")
				}
				batch = append(batch, clone)
			}
			if _, err := claims.InsertMany(ctx, batch); err != nil {
				t.Fatal(err)
			}
		}
		for start := 1; start < f.claims; start += 1000 {
			batch := []any{}
			for n := start; n < f.claims && n < start+1000; n++ {
				incarnation, attempt := uint32(n/2000), uint32(n%2000)+1
				id := commandID(operation, incarnation, attempt)
				clone := bson.M{}
				for key, value := range template {
					clone[key] = value
				}
				clone["_id"], clone["command_id"], clone["incarnation"] = stepClaimID(id), id, incarnation
				batch = append(batch, clone)
			}
			if _, err := claims.InsertMany(ctx, batch); err != nil {
				t.Fatal(err)
			}
		}

		// Reserve：每次一个新一生的新命令走完整的 reserve 事务（守卫 + 按操作查询 + 写自己的 claim），量完把它标成
		// 可重试失败，下一次 Reserve 面对的仍是同样的形态（不计时）。
		next := uint32(100)
		var reserveTimes, successTimes []time.Duration
		for iteration := 0; iteration < iterations; iteration++ {
			c := command(next, uint32(iteration+1))
			digest, err := commandDigest(c)
			if err != nil {
				t.Fatal(err)
			}
			began := time.Now()
			reservation, err := inbox.reserve(ctx, c, digest)
			reserveTimes = append(reserveTimes, time.Since(began))
			if err != nil || reservation.Duplicate {
				t.Fatalf("%s/%d reserve %s: %+v err=%v", f.name, f.claims, c.ID, reservation, err)
			}
			if err := inbox.markCompleted(ctx, c.ID, Completion{CommandID: c.ID, IdempotencyKey: operation, SagaID: c.SagaID, Retryable: true, Error: "x"}); err != nil {
				t.Fatal(err)
			}
			began = time.Now()
			if _, found, err := inbox.operationSuccess(ctx, command(next, 9999)); err != nil || found {
				t.Fatalf("operationSuccess: found=%v err=%v", found, err)
			}
			successTimes = append(successTimes, time.Since(began))
		}

		// 只量查询本身：修后的过滤与修前的“全部 claim、Limit 4097”（修前 Reserve 的查询）在同一份数据上对照。
		var newQuery, oldQuery []time.Duration
		for iteration := 0; iteration < iterations; iteration++ {
			var found []stepClaim
			began := time.Now()
			if err := claims.Find(ctx, operationClaimsFilter(operation, true, next), &found, fmongo.FindOption{Limit: maxDecisiveOperationClaims + maxLegacyOperationClaims + 1}); err != nil {
				t.Fatal(err)
			}
			newQuery = append(newQuery, time.Since(began))
			found = nil
			began = time.Now()
			if err := claims.Find(ctx, bson.M{"namespace": stepClaimNamespace, "operation_key": operation}, &found, fmongo.FindOption{Limit: 4097}); err != nil {
				t.Fatal(err)
			}
			oldQuery = append(oldQuery, time.Since(began))
		}

		stats, plan := explainOperationClaims(t, raw.Database(database), collection+mongoInboxClaimSuffix, operationClaimsFilter(operation, true, next))
		t.Logf("%-8s %5v %6d %6d | %-60s | %6v %6v %5v | %8s %8s | %8s %8s | %8s %8s", f.name, f.index, f.background, f.claims, plan, stats["totalKeysExamined"],
			stats["totalDocsExamined"], stats["nReturned"], pct(reserveTimes, 50), pct(reserveTimes, 90), pct(successTimes, 50), pct(successTimes, 90),
			pct(newQuery, 50), pct(oldQuery, 50))
	}
}

// explainOperationClaims 返回按操作查询的 executionStats 与压成一行的 winningPlan。
func explainOperationClaims(t *testing.T, database *mongo.Database, collection string, filter bson.M) (map[string]any, string) {
	t.Helper()
	var explainRaw bson.Raw
	cmd := bson.D{{Key: "explain", Value: bson.D{{Key: "find", Value: collection}, {Key: "filter", Value: filter},
		{Key: "limit", Value: maxDecisiveOperationClaims + maxLegacyOperationClaims + 1}}}, {Key: "verbosity", Value: "executionStats"}}
	if err := database.RunCommand(context.Background(), cmd).Decode(&explainRaw); err != nil {
		t.Fatal(err)
	}
	// 嵌套文档按扩展 JSON 转成 map，免得逐层区分 bson.D / bson.M。
	relaxed, err := bson.MarshalExtJSON(explainRaw, false, false)
	if err != nil {
		t.Fatal(err)
	}
	var explain map[string]any
	if err := json.Unmarshal(relaxed, &explain); err != nil {
		t.Fatal(err)
	}
	return explain["executionStats"].(map[string]any), planSummary(explain["queryPlanner"].(map[string]any)["winningPlan"].(map[string]any))
}

// RR-20261006-15 复核补修：按操作的查询在服务端也只碰有影响的 claim。只有 by_operation 时服务端取出这个操作的
// 全部 claim 逐个过滤，检查的文档数随累积的可重试失败、被接替的尝试线性增长（RR-15 没采用“分页扫全部”的理由
// 同样适用于服务端）；by_operation_decision 让 $or 的每个分支都落在索引前缀上。
func TestRealMongoOperationClaimsQueryExaminesOnlyDecisiveClaims(t *testing.T) {
	client, database := realSagadirMongo(t)
	ctx := context.Background()
	raw, err := mongo.Connect(options.Client().ApplyURI(os.Getenv("ROOST_DATAENGINE_IT_MONGO_URI")))
	if err != nil {
		t.Fatal(err)
	}
	defer raw.Disconnect(ctx)
	inbox, err := NewMongoCommandInbox(client, database, "steps")
	if err != nil {
		t.Fatal(err)
	}
	if err := inbox.EnsureInfrastructure(ctx); err != nil {
		t.Fatal(err)
	}
	operation := "gift-1:1:0"
	const accumulated = 2000
	docs := make([]any, 0, accumulated+3)
	for n := 0; n < accumulated; n++ {
		incarnation, attempt := uint32(n/1000), uint32(n%1000)+1
		id := commandID(operation, incarnation, attempt)
		doc := bson.M{"_id": stepClaimID(id), "command_id": id, "namespace": stepClaimNamespace, "operation_key": operation,
			"incarnation": incarnation, "status": claimStatusCompleted, "outcome": claimOutcomeRetryable}
		if n%3 == 0 {
			doc["status"] = claimStatusSuperseded
			delete(doc, "outcome")
		}
		docs = append(docs, doc)
	}
	docs = append(docs,
		bson.M{"_id": "pending", "command_id": "pending", "namespace": stepClaimNamespace, "operation_key": operation, "incarnation": uint32(2), "status": claimStatusPending},
		bson.M{"_id": "refused-r1", "command_id": "refused-r1", "namespace": stepClaimNamespace, "operation_key": operation, "incarnation": uint32(1), "status": claimStatusCompleted, "outcome": claimOutcomeRefused},
		bson.M{"_id": "refused-r2", "command_id": "refused-r2", "namespace": stepClaimNamespace, "operation_key": operation, "incarnation": uint32(2), "status": claimStatusCompleted, "outcome": claimOutcomeRefused},
	)
	collection := "steps" + mongoInboxClaimSuffix
	if _, err := client.Database(database).Collection(collection).InsertMany(ctx, docs); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name     string
		filter   bson.M
		returned float64
	}{
		{"reserve, life 2", operationClaimsFilter(operation, true, 2), 2},
		{"reserve, life 0", operationClaimsFilter(operation, true, 0), 1},
		{"operationSuccess", operationClaimsFilter(operation, false, 0), 1},
	} {
		stats, plan := explainOperationClaims(t, raw.Database(database), collection, tc.filter)
		keys, docs, returned := stats["totalKeysExamined"].(float64), stats["totalDocsExamined"].(float64), stats["nReturned"].(float64)
		if returned != tc.returned || docs > tc.returned+2 || keys > tc.returned+8 {
			t.Errorf("%s over %d accumulated claims: examined %v keys and %v documents to return %v (plan %s), want only the decisive claims touched",
				tc.name, accumulated, keys, docs, returned, plan)
		}
	}
}

func pct(samples []time.Duration, p int) string {
	sorted := slices.Clone(samples)
	slices.Sort(sorted)
	return sorted[(len(sorted)-1)*p/100].Round(10 * time.Microsecond).String()
}

// planSummary 把 winningPlan 压成 “STAGE(index)<-STAGE…” 一行。
func planSummary(plan map[string]any) string {
	if inner, ok := plan["queryPlan"].(map[string]any); ok { // SBE 计划包一层 queryPlan
		plan = inner
	}
	out := fmt.Sprint(plan["stage"])
	if name, ok := plan["indexName"]; ok {
		out += "(" + fmt.Sprint(name) + ")"
	}
	if child, ok := plan["inputStage"].(map[string]any); ok {
		out += "<-" + planSummary(child)
	}
	if children, ok := plan["inputStages"].([]any); ok {
		out += "<-["
		for i, child := range children {
			if i > 0 {
				out += ","
			}
			out += planSummary(child.(map[string]any))
		}
		out += "]"
	}
	return out
}
