package saga

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/storeservice"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// RedisProjection 是 saga 在 Store Service 上的投影：把权威 Redis 每个分区的日志写进 Mongo 冷副本，并在进度推进之后
// 驱逐已投影、过了保留期的终态记录。
//
// 写法（每批最多三次 BulkWrite，不用 Mongo 事务）：同一 saga 只写最高版本的记录主体，版本只升不降
// （$replaceWith + $cond 比较 version）；回执 $setOnInsert，已存在不改；tombstone 的 closure 只升不降（已是 result 保持），
// 迟到告警只写第一次。冷副本不是权威，读它的场景都发生在“该记录已投影”（驱逐的前提）之后或明确最终一致（List），一批里
// 部分写成功后崩溃，重放会补齐，单调条件保证不倒退。
type RedisProjection struct {
	redis   fredis.IRedis
	scripts fredis.DurableScriptBatcher
	copy    *MongoStore
	opts    RedisStoreOptions
	keys    redisKeys
	evict   *fredis.LuaScript
	// terminal 是各分区驻留的终态记录数（ZCARD evict:<p>），汇总成 saga.redis.resident_records{state="terminal"}。
	terminal []atomic.Int64
}

// evictBatch 是一次驱逐脚本最多处理的成员数；evictDelay 是还没投影到时推迟再看的时间。
const (
	evictBatch = 256
	evictDelay = time.Second
)

// NewRedisProjection 构造 saga 投影。options 与协调器的 RedisStoreOptions 相同（同一份 saga.redis.* 配置）。
func NewRedisProjection(authority fredis.IRedis, copy *MongoStore, options RedisStoreOptions) (*RedisProjection, error) {
	store, err := NewRedisStore(authority, copy, nil, options)
	if err != nil {
		return nil, err
	}
	return &RedisProjection{redis: authority, scripts: store.scripts, copy: copy, opts: store.opts, keys: store.keys, evict: store.lua["evict"],
		terminal: make([]atomic.Int64, store.opts.Partitions)}, nil
}

func (p *RedisProjection) Name() string      { return "saga" }
func (p *RedisProjection) KeyPrefix() string { return p.keys.prefix }
func (p *RedisProjection) Partitions() int   { return p.opts.Partitions }

// projectedTombstone 是一批条目合并后一个操作要写的 tombstone 与告警。
type projectedTombstone struct {
	sagaID  string
	closure string
	created int64
	alarms  map[uint32]int64
}

// Apply 把一批日志条目写进 Mongo 冷副本（见 RedisProjection）。唯一业务键冲突、文档校验失败是非暂时错误（Permanent），
// Store Service 暂停该分区、不跳过；其他错误按暂时错误重试同一批。
func (p *RedisProjection) Apply(ctx context.Context, partition int, entries []storeservice.Entry) error {
	type latest struct {
		version uint64
		doc     string
	}
	records := map[string]latest{}
	var order []string
	var receipts []fmongo.WriteModel
	tombstones := map[string]*projectedTombstone{}
	var operations []string
	for _, entry := range entries {
		f := entry.Fields
		id := f["id"]
		version, err := strconv.ParseUint(f["v"], 10, 64)
		if id == "" || err != nil || f["doc"] == "" {
			return storeservice.Permanent(fmt.Errorf("saga projection: malformed log entry %s in partition %d: %v", entry.ID, partition, f))
		}
		if current, ok := records[id]; !ok || version > current.version {
			if !ok {
				order = append(order, id)
			}
			records[id] = latest{version: version, doc: f["doc"]}
		}
		if command := f["c"]; command != "" {
			digest, err := hex.DecodeString(f["cd"])
			if err != nil {
				return storeservice.Permanent(fmt.Errorf("saga projection: entry %s receipt digest: %v", entry.ID, err))
			}
			receipts = append(receipts, fmongo.NewUpdateOneModel(bson.M{"_id": command},
				bson.M{"$setOnInsert": bson.M{"digest": digest, "created_at": fromMillis(parseInt64(f["ct"]))}}, true))
		}
		tombstone := func(operation string) *projectedTombstone {
			t := tombstones[operation]
			if t == nil {
				t = &projectedTombstone{sagaID: id, alarms: map[uint32]int64{}}
				tombstones[operation] = t
				operations = append(operations, operation)
			}
			return t
		}
		if operation := f["o"]; operation != "" {
			t := tombstone(operation)
			if t.closure != operationClosureResult {
				t.closure = f["oc"]
			}
			if t.created == 0 {
				t.created = parseInt64(f["ot"])
			}
		}
		if operation := f["a"]; operation != "" {
			incarnation, err := strconv.ParseUint(f["ai"], 10, 32)
			if err != nil {
				return storeservice.Permanent(fmt.Errorf("saga projection: entry %s alarm incarnation: %v", entry.ID, err))
			}
			t := tombstone(operation)
			if _, seen := t.alarms[uint32(incarnation)]; !seen {
				t.alarms[uint32(incarnation)] = parseInt64(f["at"])
			}
		}
	}
	sagas := make([]fmongo.WriteModel, 0, len(order))
	for _, id := range order {
		record, err := decodeRecordDoc(records[id].doc, Lease{})
		if err != nil {
			return storeservice.Permanent(fmt.Errorf("saga projection: %w", err))
		}
		doc := toRecordDoc(record)
		sagas = append(sagas, fmongo.NewUpdateOneModel(bson.M{"_id": id}, bson.A{bson.M{"$replaceWith": bson.M{"$cond": bson.A{
			bson.M{"$lt": bson.A{bson.M{"$ifNull": bson.A{"$version", 0}}, int64(record.Version)}},
			bson.M{"$literal": doc}, "$$ROOT"}}}}, true))
	}
	ops := make([]fmongo.WriteModel, 0, len(operations))
	for _, operation := range operations {
		t := tombstones[operation]
		set := bson.M{}
		if t.closure != "" {
			set["saga_id"] = bson.M{"$ifNull": bson.A{"$saga_id", bson.M{"$literal": t.sagaID}}}
			set["created_at"] = bson.M{"$ifNull": bson.A{"$created_at", fromMillis(t.created)}}
			set["closure"] = bson.M{"$cond": bson.A{bson.M{"$eq": bson.A{"$closure", operationClosureResult}}, operationClosureResult, bson.M{"$literal": t.closure}}}
		}
		for incarnation, at := range t.alarms {
			field := lateAlarmField(incarnation)
			set[field] = bson.M{"$ifNull": bson.A{"$" + field, fromMillis(at)}}
		}
		ops = append(ops, fmongo.NewUpdateOneModel(bson.M{"_id": operation}, bson.A{bson.M{"$set": set}}, true))
	}
	for _, write := range []struct {
		collection string
		models     []fmongo.WriteModel
		target     fmongo.ICollection
	}{{"sagas", sagas, p.copy.sagas()}, {"completions", receipts, p.copy.completions()}, {"operations", ops, p.copy.operations()}} {
		if len(write.models) == 0 {
			continue
		}
		metrics.IncCounter("store_service.mongo_ops_total", metrics.Labels{"projection": "saga", "collection": write.collection}, 1)
		if _, err := write.target.BulkWrite(ctx, write.models); err != nil {
			return classifyProjectionError(write.collection, err)
		}
	}
	return nil
}

// classifyProjectionError：唯一业务键冲突（另一个 saga 已占用同一 (type, business_key)）与文档校验失败重试不会好，标记为
// 非暂时；_id 上的重复键是并发 upsert 的竞争，重试即可。
func classifyProjectionError(collection string, err error) error {
	text := err.Error()
	if (errors.Is(err, fmongo.ErrDuplicateKey) && strings.Contains(text, "uniq_type_business")) || strings.Contains(text, "Document failed validation") {
		return storeservice.Permanent(fmt.Errorf("saga projection: %s: %w", collection, err))
	}
	return fmt.Errorf("saga projection: %s: %w", collection, err)
}

// AfterAdvance 驱逐这个分区里已投影（ls 不晚于进度）、过了保留期的终态记录（evict.lua）。
func (p *RedisProjection) AfterAdvance(ctx context.Context, partition int, _ string) error {
	results, local, _, err := p.scripts.DurableScriptBatch(ctx, []fredis.ScriptCall{p.evict.Call([]string{p.keys.evict(partition), p.keys.prefix + ":proj:" + strconv.Itoa(partition)},
		evictBatch, p.opts.BusinessKeyGuard.Milliseconds(), evictDelay.Milliseconds(), p.keys.prefix)}, 1, 0, p.opts.WaitAOFTimeout)
	if err != nil {
		return err
	}
	if results[0].Err != nil {
		return results[0].Err
	}
	if local < 1 {
		return fmt.Errorf("saga projection: evict executed but WAITAOF local=%d", local)
	}
	code, items, err := fredis.ReplyCode(results[0].Value)
	if err != nil {
		return err
	}
	if code != "OK" {
		return fmt.Errorf("saga projection: evict returned %q", code)
	}
	if evicted := parseInt64(fredis.ReplyString(items, 0)); evicted > 0 {
		metrics.IncCounter("store_service.evicted_total", metrics.Labels{"projection": "saga"}, evicted)
	}
	p.observeResident(ctx, partition)
	return nil
}

// observeResident 更新驻留记录数：非终态全部在 due 里（分区 0 的循环顺带读），终态按分区读 evict:<p> 再汇总。
func (p *RedisProjection) observeResident(ctx context.Context, partition int) {
	if n, err := p.redis.ZCard(ctx, p.keys.evict(partition)); err == nil {
		p.terminal[partition].Store(n)
		var total int64
		for i := range p.terminal {
			total += p.terminal[i].Load()
		}
		metrics.SetGauge("saga.redis.resident_records", metrics.Labels{"state": "terminal"}, total)
	}
	if partition == 0 {
		if n, err := p.redis.ZCard(ctx, p.keys.due()); err == nil {
			metrics.SetGauge("saga.redis.resident_records", metrics.Labels{"state": "active"}, n)
		}
	}
}

var _ storeservice.Projection = (*RedisProjection)(nil)
