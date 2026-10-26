package saga

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
	fredis "github.com/tjbdwanghaibo/roost-core/infra/storage/redis"
)

// RedisStoreOptions 配置协调器的权威 Redis 存储。零值字段取缺省（括号内）。
type RedisStoreOptions struct {
	// KeyPrefix 是全部键的前缀（saga.redis.key_prefix，必填）；同一个 Redis 上的不同部署必须不同。
	KeyPrefix string
	// Partitions 是投影分区数（16）：Store Service 按分区并行投影、每个分区一条有序日志。
	Partitions int
	// WaitAOFTimeout 是每次写脚本后同连接 WAITAOF 的等待上限（1s）。
	WaitAOFTimeout time.Duration
	// TerminalRetention 是终态记录投影之后在 Redis 里再驻留多久（15m）。
	TerminalRetention time.Duration
	// BusinessKeyGuard 是业务键守卫在记录驱逐后的保留期（10m），Create 的 Mongo 读到脚本执行不能超过它的一半。
	BusinessKeyGuard time.Duration
	// MaxProjectionBacklog 是每个分区未投影日志的上限（20000），超过时 Create 返回 ErrBackpressure。
	MaxProjectionBacklog int64
}

// DefaultRedisStoreOptions 返回缺省值（KeyPrefix 为空，必须由调用方给出）。
func DefaultRedisStoreOptions() RedisStoreOptions {
	return RedisStoreOptions{Partitions: 16, WaitAOFTimeout: time.Second, TerminalRetention: 15 * time.Minute, BusinessKeyGuard: 10 * time.Minute, MaxProjectionBacklog: 20000}
}

// RedisStore 是协调器的权威存储：记录、领取租约、命令 outbox、completion 回执、操作 tombstone 与迟到告警标记都在单主 Redis
// 上，每次状态变化是一个 Lua 脚本，与同连接的 WAITAOF 一起提交（appendfsync always 下脚本回复时已 fsync，WAITAOF local=1
// 在每次提交上核对部署满足持久契约）。Mongo 是冷副本：Store Service 把每个分区的日志投影进去；Redis 里没有的记录从冷副本读。
//
// 结果分类：
//   - 脚本的业务结论（冲突、重复、身份冲突、需要装回、积压）按结果码映射为错误值，记录没有被改；
//   - 驱动返回“结果未知”（连接中断、超时）时原样返回，不在这里重试：Apply 的重试靠请求摘要（la）认出已应用，
//     领取 / 发布靠租约过期接手，与 Mongo 下 commit 结果未知的处理一致；
//   - WAITAOF 没确认（local=0 或被服务端拒绝）：脚本已执行、持久未确认。返回 ErrDurabilityUnconfirmed、不报成功也不回滚，
//     计 saga.redis.waitaof_unconfirmed_total，并向 RuntimeFailure 报告让进程 fail-stop（部署违反了持久契约）；
//   - 脚本错误回复（校验之后的写失败）按部分写入处理：计 saga.redis.script_partial_total、记 ERROR、fail-stop。
type RedisStore struct {
	redis   fredis.IRedis
	scripts fredis.DurableScriptBatcher
	hashes  fredis.HashFieldsReader
	copy    *MongoStore
	failure *app.RuntimeFailure
	opts    RedisStoreOptions
	keys    redisKeys
	lua     map[string]*fredis.LuaScript
}

// NewRedisStore 校验能力与配置并构造存储，不访问 Redis 与 Mongo；EnsureInfrastructure 做启动探针与元数据守卫。
// authority 必须同时实现 DurableScriptBatcher、HashFieldsReader、ScriptLoader 与 ServerInspector（redis/driver.Client 满足）。
func NewRedisStore(authority fredis.IRedis, copy *MongoStore, failure *app.RuntimeFailure, options RedisStoreOptions) (*RedisStore, error) {
	if authority == nil || copy == nil {
		return nil, errors.New("saga redis: authority redis and mongo copy are required")
	}
	scripts, okScripts := authority.(fredis.DurableScriptBatcher)
	hashes, okHashes := authority.(fredis.HashFieldsReader)
	_, okLoader := authority.(fredis.ScriptLoader)
	_, okInspector := authority.(fredis.ServerInspector)
	if !okScripts || !okHashes || !okLoader || !okInspector {
		return nil, fmt.Errorf("saga redis: %T lacks durable script batches, HMGET, SCRIPT LOAD or INFO/CONFIG", authority)
	}
	options.KeyPrefix = strings.TrimSpace(options.KeyPrefix)
	if options.KeyPrefix == "" || strings.ContainsAny(options.KeyPrefix, " \t\r\n{}*?[]") {
		return nil, fmt.Errorf("saga redis: key prefix %q must be non-empty without spaces, hash tags or glob characters", options.KeyPrefix)
	}
	defaults := DefaultRedisStoreOptions()
	if options.Partitions == 0 {
		options.Partitions = defaults.Partitions
	}
	if options.WaitAOFTimeout == 0 {
		options.WaitAOFTimeout = defaults.WaitAOFTimeout
	}
	if options.TerminalRetention == 0 {
		options.TerminalRetention = defaults.TerminalRetention
	}
	if options.BusinessKeyGuard == 0 {
		options.BusinessKeyGuard = defaults.BusinessKeyGuard
	}
	if options.MaxProjectionBacklog == 0 {
		options.MaxProjectionBacklog = defaults.MaxProjectionBacklog
	}
	if options.Partitions < 1 || options.Partitions > 1024 || options.WaitAOFTimeout < time.Millisecond || options.TerminalRetention < 0 ||
		options.BusinessKeyGuard <= 0 || options.MaxProjectionBacklog < 1 {
		return nil, fmt.Errorf("saga redis: invalid options %+v", options)
	}
	lua, err := loadRedisScripts(redisLua, "redis_lua", sagaScriptNames)
	if err != nil {
		return nil, err
	}
	return &RedisStore{redis: authority, scripts: scripts, hashes: hashes, copy: copy, failure: failure, opts: options,
		keys: redisKeys{prefix: options.KeyPrefix, partitions: options.Partitions}, lua: lua}, nil
}

// Options 返回补齐缺省后的配置（Store Service 的 saga 投影用同一份前缀与分区数）。
func (s *RedisStore) Options() RedisStoreOptions { return s.opts }

// Copy 返回 Mongo 冷副本。
func (s *RedisStore) Copy() *MongoStore { return s.copy }

// ProjectionBacklog 是全部分区还没投影进 Mongo 冷副本的日志条目数（各分区 XLEN 之和）。合服前置检查要求它为 0。
func (s *RedisStore) ProjectionBacklog(ctx context.Context) (int64, error) {
	reader, ok := s.redis.(fredis.StreamReader)
	if !ok {
		return 0, errors.New("saga redis: the authority client cannot read streams")
	}
	var total int64
	for partition := range s.opts.Partitions {
		n, err := reader.XLen(ctx, s.keys.log(partition))
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// Ping 检查权威 Redis 与 Mongo 冷副本。
func (s *RedisStore) Ping(ctx context.Context) error {
	if s == nil {
		return errors.New("saga redis: not initialized")
	}
	if err := s.redis.Ping(ctx); err != nil {
		return fmt.Errorf("saga redis: authority redis: %w", err)
	}
	return s.copy.Ping(ctx)
}

// run 执行一个写脚本（与同连接 WAITAOF 一起），按 RedisStore 的结果分类返回结果码与其余元素。
func (s *RedisStore) run(ctx context.Context, name string, keys []string, args ...any) (string, []any, error) {
	script := s.lua[name]
	start := time.Now()
	results, local, _, err := s.scripts.DurableScriptBatch(ctx, []fredis.ScriptCall{script.Call(keys, args...)}, 1, 0, s.opts.WaitAOFTimeout)
	metrics.ObserveHistogram("saga.redis.script_seconds", metrics.Labels{"script": name}, time.Since(start))
	if err != nil && !errors.Is(err, fredis.ErrWaitAOFFailed) {
		metrics.IncCounter("saga.redis.script_total", metrics.Labels{"script": name, "result": "unknown"}, 1)
		return "", nil, err
	}
	if len(results) != 1 {
		return "", nil, fmt.Errorf("saga redis: script %s returned %d results", name, len(results))
	}
	if result := results[0]; result.Err != nil {
		if strings.HasPrefix(result.Err.Error(), "OOM") {
			// 带 #!lua 头、不声明 allow-oom 的脚本在执行前被拒：确定没执行。
			metrics.IncCounter("saga.redis.script_total", metrics.Labels{"script": name, "result": "oom"}, 1)
			return "", nil, fmt.Errorf("%w: authority redis is out of memory: %v", ErrBackpressure, result.Err)
		}
		metrics.IncCounter("saga.redis.script_total", metrics.Labels{"script": name, "result": "error"}, 1)
		metrics.IncCounter("saga.redis.script_partial_total", metrics.Labels{"script": name}, 1)
		failure := fmt.Errorf("saga redis: script %s failed after validation; its writes may be partial: %w", name, result.Err)
		slog.Error("saga redis: script failed after validation; check the keys it touches", "script", name, "keys", keys, "err", result.Err)
		s.failure.Fail(failure)
		return "", nil, failure
	}
	if err != nil || local < 1 {
		metrics.IncCounter("saga.redis.waitaof_unconfirmed_total", metrics.Labels{"script": name}, 1)
		failure := fmt.Errorf("%w: script %s executed, WAITAOF local=%d: %v", ErrDurabilityUnconfirmed, name, local, err)
		slog.Error("saga redis: write executed but not confirmed durable; the deployment breaks appendonly yes / appendfsync always", "script", name, "local", local, "err", err)
		s.failure.Fail(failure)
		return "", nil, failure
	}
	code, items, parseErr := replyCode(results[0].Value)
	if parseErr != nil {
		return "", nil, parseErr
	}
	metrics.IncCounter("saga.redis.script_total", metrics.Labels{"script": name, "result": code}, 1)
	if code == "CORRUPT" {
		slog.Error("saga redis: a key has the wrong type; repair it before the record can move", "script", name, "keys", keys)
		return code, items, fmt.Errorf("%w: script %s found a key of the wrong type", ErrInvalidRecord, name)
	}
	return code, items, nil
}

// Create 先查 Mongo 冷副本（同 id 或同业务键已有记录则 ErrAlreadyExists；业务键在冷副本里永久唯一），再执行 create.lua。
// 脚本核对 Redis 里的 id 与业务键守卫，并要求从 Mongo 读开始到脚本执行不超过 BusinessKeyGuard 的一半：驱逐后业务键守卫还保留
// 一个 guard，被驱逐的记录在那之前已投影进 Mongo，所以 Mongo 读看不到的旧记录一定还被 Redis 的守卫挡着。超时重做，三次后
// ErrConflict。分区日志积压超过 MaxProjectionBacklog 时返回 ErrBackpressure。
func (s *RedisStore) Create(ctx context.Context, record Record) error {
	if err := record.Validate(); err != nil {
		return err
	}
	doc, err := encodeRecordDoc(record)
	if err != nil {
		return err
	}
	partition := s.keys.partition(record.ID)
	terminalAt := int64(0)
	if record.Status.Terminal() {
		terminalAt = millis(record.UpdatedAt)
	}
	keys := []string{s.keys.record(record.ID), s.keys.businessKey(record.Type, record.BusinessKey), s.keys.due(), s.keys.log(partition), s.keys.evict(partition)}
	for range 3 {
		readStart := time.Now()
		exists, err := s.copy.recordExists(ctx, record.ID, record.Type, record.BusinessKey)
		if err != nil {
			return err
		}
		if exists {
			return ErrAlreadyExists
		}
		code, _, err := s.run(ctx, "create", keys, record.ID, record.Version, int(record.Status), millis(record.NextRunAt),
			record.Lease.Owner, record.Lease.Token, millis(record.Lease.Until), terminalAt, doc, keys[1], partition,
			readStart.UnixMilli(), s.opts.BusinessKeyGuard.Milliseconds(), s.opts.MaxProjectionBacklog, s.opts.TerminalRetention.Milliseconds())
		if err != nil {
			return err
		}
		switch code {
		case "OK":
			return nil
		case "EXISTS_ID", "EXISTS_BK":
			return ErrAlreadyExists
		case "STALE_GUARD":
			continue
		case "BACKPRESSURE":
			metrics.IncCounter("saga.redis.backpressure_total", nil, 1)
			return fmt.Errorf("%w: projection backlog of partition %d reached %d", ErrBackpressure, partition, s.opts.MaxProjectionBacklog)
		default:
			return fmt.Errorf("saga redis: create returned %q", code)
		}
	}
	return fmt.Errorf("%w: the Mongo business-key read kept exceeding half of the business key guard", ErrConflict)
}

// recordFields 是 Get 读的哈希字段（不读回执 / tombstone）。
var recordFields = []string{"v", "lo", "lt", "lu", "doc"}

// Get 读驻留记录；Redis 里没有时读 Mongo 冷副本（计 saga.redis.cold_read_total{op="get"}）。
func (s *RedisStore) Get(ctx context.Context, id string) (Record, error) {
	record, found, err := s.resident(ctx, id)
	if err != nil || found {
		return record, err
	}
	metrics.IncCounter("saga.redis.cold_read_total", metrics.Labels{"op": "get"}, 1)
	return s.copy.Get(ctx, id)
}

// resident 读一条驻留记录；found=false 表示 Redis 里没有。
func (s *RedisStore) resident(ctx context.Context, id string) (Record, bool, error) {
	values, err := s.hashes.HMGet(ctx, s.keys.record(id), recordFields...)
	if err != nil {
		return Record{}, false, err
	}
	if values[4] == nil {
		return Record{}, false, nil
	}
	lease, err := leaseFromFields(string(values[1]), string(values[2]), string(values[3]))
	if err != nil {
		return Record{}, true, err
	}
	record, err := decodeRecordDoc(string(values[4]), lease)
	if err != nil {
		return Record{}, true, err
	}
	if strconv.FormatUint(record.Version, 10) != string(values[0]) {
		return Record{}, true, fmt.Errorf("%w: record %q version field %q differs from its body %d", ErrInvalidRecord, id, values[0], record.Version)
	}
	return record, true, nil
}

// GetByBusinessKey 经业务键守卫找到 id 再 Get；守卫不在时读冷副本。
func (s *RedisStore) GetByBusinessKey(ctx context.Context, sagaType, businessKey string) (Record, error) {
	id, err := s.redis.Get(ctx, s.keys.businessKey(sagaType, businessKey))
	if errors.Is(err, fredis.ErrNil) {
		metrics.IncCounter("saga.redis.cold_read_total", metrics.Labels{"op": "business_key"}, 1)
		return s.copy.GetByBusinessKey(ctx, sagaType, businessKey)
	}
	if err != nil {
		return Record{}, err
	}
	return s.Get(ctx, string(id))
}

// List 是运维查询：驻留记录（非终态全部在 due 里，驻留的终态在各分区的 evict 里）逐条读出过滤，再并上 Mongo 冷副本里的
// 记录（冷副本落后于投影积压，终态部分是最终一致的），同一 id 取版本高的一份，按 UpdatedAt 倒序截到 Limit。
func (s *RedisStore) List(ctx context.Context, query Query) ([]Record, error) {
	if query.Limit <= 0 || query.Limit > 1000 {
		return nil, ErrInvalidRecord
	}
	wantTerminal, wantActive := len(query.Statuses) == 0, len(query.Statuses) == 0
	for _, status := range query.Statuses {
		if status.Terminal() {
			wantTerminal = true
		} else {
			wantActive = true
		}
	}
	var ids []string
	if wantActive {
		active, err := s.redis.ZRangeWithScores(ctx, s.keys.due(), 0, -1)
		if err != nil {
			return nil, err
		}
		for _, member := range active {
			ids = append(ids, member.Member)
		}
	}
	if wantTerminal {
		for partition := range s.opts.Partitions {
			terminal, err := s.redis.ZRangeWithScores(ctx, s.keys.evict(partition), 0, -1)
			if err != nil {
				return nil, err
			}
			for _, member := range terminal {
				ids = append(ids, member.Member)
			}
		}
	}
	merged := make(map[string]Record)
	for _, id := range ids {
		record, found, err := s.resident(ctx, id)
		if err != nil {
			reportCorruptRecord("list", id, err)
			continue
		}
		if found && matchesQuery(record, query) {
			merged[id] = record
		}
	}
	if wantTerminal {
		cold, err := s.copy.List(ctx, query)
		if err != nil {
			return nil, err
		}
		for _, record := range cold {
			if current, ok := merged[record.ID]; !ok || current.Version < record.Version {
				merged[record.ID] = record
			}
		}
	}
	out := make([]Record, 0, len(merged))
	for _, record := range merged {
		out = append(out, record)
	}
	slices.SortFunc(out, func(a, b Record) int {
		if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
			return c
		}
		return strings.Compare(a.ID, b.ID)
	})
	if len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func matchesQuery(record Record, query Query) bool {
	if query.Type != "" && record.Type != query.Type {
		return false
	}
	if query.DefinitionVersion != 0 && record.DefinitionVersion != query.DefinitionVersion {
		return false
	}
	if len(query.Statuses) > 0 && !slices.Contains(query.Statuses, record.Status) {
		return false
	}
	return query.UpdatedBefore.IsZero() || record.UpdatedAt.Before(query.UpdatedBefore)
}

// CompletionRecorded 与 CompletionHistory 相同口径。
func (s *RedisStore) CompletionRecorded(ctx context.Context, completion Completion) (bool, error) {
	history, err := s.CompletionHistory(ctx, completion)
	return history.Recorded, err
}

// CompletionHistory 先读驻留记录上的回执字段与 tombstone 字段；记录不驻留时读冷副本；驻留但两者都没有时也回查冷副本
// （装回的记录不带回执，回执留在 Mongo）。身份检查与冷副本相同：回执摘要不同、或操作不属于这笔 saga 是身份冲突。
func (s *RedisStore) CompletionHistory(ctx context.Context, completion Completion) (CompletionHistory, error) {
	if err := completion.Validate(); err != nil {
		return CompletionHistory{}, ErrInvalidRecord
	}
	if !strings.HasPrefix(completion.IdempotencyKey, completion.SagaID+":") {
		return CompletionHistory{}, ErrIdentityConflict
	}
	values, err := s.hashes.HMGet(ctx, s.keys.record(completion.SagaID), "v", "c:"+completion.CommandID, "o:"+completion.IdempotencyKey)
	if err != nil {
		return CompletionHistory{}, err
	}
	if values[0] == nil {
		metrics.IncCounter("saga.redis.cold_read_total", metrics.Labels{"op": "history"}, 1)
		return s.copy.CompletionHistory(ctx, completion)
	}
	if values[1] != nil {
		digest, err := receiptDigestHex(completion)
		if err != nil {
			return CompletionHistory{}, err
		}
		if string(values[1]) != digest {
			return CompletionHistory{}, ErrIdentityConflict
		}
		return CompletionHistory{Recorded: true, Receipt: true}, nil
	}
	if values[2] != nil {
		closure, _, _ := strings.Cut(string(values[2]), "|")
		return CompletionHistory{Recorded: true, Closure: operationDoc{Closure: closure}.closure()}, nil
	}
	metrics.IncCounter("saga.redis.cold_read_total", metrics.Labels{"op": "history"}, 1)
	return s.copy.CompletionHistory(ctx, completion)
}

// MarkLateSuccessAlarm 在 tombstone 上原子地记下（操作，代际）的迟到告警；没有 tombstone 返回 false。记录已驱逐时先从冷副本装回
// （冷记录要写，先装回），再记一次。
func (s *RedisStore) MarkLateSuccessAlarm(ctx context.Context, completion Completion, incarnation uint32) (bool, error) {
	if err := completion.Validate(); err != nil {
		return false, ErrInvalidRecord
	}
	if !strings.HasPrefix(completion.IdempotencyKey, completion.SagaID+":") {
		return false, nil
	}
	partition := s.keys.partition(completion.SagaID)
	for reloaded := false; ; reloaded = true {
		code, _, err := s.run(ctx, "mark_alarm", []string{s.keys.record(completion.SagaID), s.keys.log(partition)},
			completion.IdempotencyKey, incarnation, time.Now().UnixMilli(), completion.SagaID)
		if err != nil {
			return false, err
		}
		switch code {
		case "MARKED":
			return true, nil
		case "NONE":
			return false, nil
		case "NEED_RELOAD":
			if reloaded {
				return false, ErrConflict
			}
			found, err := s.reload(ctx, completion.SagaID)
			if err != nil || !found {
				return false, err
			}
		default:
			return false, fmt.Errorf("saga redis: mark_alarm returned %q", code)
		}
	}
}

// reload 把一条已驱逐的记录从 Mongo 冷副本装回 Redis（reload.lua）：记录主体、tombstone 与告警标记；回执不装回（历史回查
// 时读冷副本）。别人先装回了（EXISTS）也算成功；冷副本里也没有时 found=false。驱逐只在“最新版本已投影”之后发生、驱逐之后
// 不会再有写（写都先装回），所以冷副本就是驱逐时的最新版本。
func (s *RedisStore) reload(ctx context.Context, id string) (bool, error) {
	record, fields, err := s.copy.loadForReload(ctx, id)
	if errors.Is(err, ErrNotFound) {
		metrics.IncCounter("saga.redis.reload_total", metrics.Labels{"result": "missing"}, 1)
		return false, nil
	}
	if err != nil {
		return false, err
	}
	record.Lease = Lease{}
	doc, err := encodeRecordDoc(record)
	if err != nil {
		return false, err
	}
	terminalAt := int64(0)
	if record.Status.Terminal() {
		terminalAt = millis(record.UpdatedAt)
	}
	partition := s.keys.partition(id)
	bk := s.keys.businessKey(record.Type, record.BusinessKey)
	args := []any{id, record.Version, int(record.Status), millis(record.NextRunAt), terminalAt, doc, bk, partition, s.opts.TerminalRetention.Milliseconds(), len(fields)}
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, name := range names {
		args = append(args, name, fields[name])
	}
	code, _, err := s.run(ctx, "reload", []string{s.keys.record(id), bk, s.keys.due(), s.keys.evict(partition)}, args...)
	if err != nil {
		return false, err
	}
	switch code {
	case "OK":
		metrics.IncCounter("saga.redis.reload_total", metrics.Labels{"result": "reloaded"}, 1)
	case "EXISTS":
		metrics.IncCounter("saga.redis.reload_total", metrics.Labels{"result": "exists"}, 1)
	default:
		return false, fmt.Errorf("saga redis: reload returned %q", code)
	}
	return true, nil
}

// ClaimDue 原子领取到期且租约过期的记录（claim_due.lua，O(log N + limit)）。校验不过的记录只跳过那一条并告警。
func (s *RedisStore) ClaimDue(ctx context.Context, request ClaimRequest) ([]Record, error) {
	if err := validateClaim(request); err != nil {
		return nil, err
	}
	_, items, err := s.run(ctx, "claim_due", []string{s.keys.due()}, request.Now.UnixMilli(), request.Owner, request.LeaseDuration.Milliseconds(), request.Limit, s.keys.prefix)
	if err != nil {
		return nil, err
	}
	out := make([]Record, 0, len(items)/4)
	for i := 0; i+3 < len(items); i += 4 {
		id, doc := replyString(items, i), replyString(items, i+1)
		lease := Lease{Owner: request.Owner, Token: uint64(parseInt64(replyString(items, i+2))), Until: fromMillis(parseInt64(replyString(items, i+3)))}
		record, err := decodeRecordDoc(doc, lease)
		if err != nil {
			reportCorruptRecord("claim", id, err)
			continue
		}
		out = append(out, record)
	}
	return out, nil
}

// Apply 执行一次记录转移（apply.lua）。脚本按“键类型 → 需要装回 → 回执 → 同一请求重试（la）→ 版本 → 租约 → 关闭操作 → 新命令”
// 的顺序全部校验后才写；不带回执的请求在回复丢失后原样重试得到“已应用”，不报冲突；带回执的按回执去重（重复）。
func (s *RedisStore) Apply(ctx context.Context, request ApplyRequest) (ApplyOutcome, error) {
	if request.ExpectedVersion == 0 || request.After.Version != request.ExpectedVersion+1 {
		return 0, ErrInvalidRecord
	}
	after := request.After
	if err := after.Validate(); err != nil {
		return 0, err
	}
	if request.CloseOperation != "" && !strings.HasPrefix(request.CloseOperation, after.ID+":") {
		return 0, ErrIdentityConflict
	}
	return s.applyScript(ctx, request)
}

// applyScript 把一次已在 Go 侧校验过的转移交给 apply.lua；脚本自己再核一遍全部条件（键类型、版本、租约、回执、
// 关闭操作的归属），不依赖调用方。
func (s *RedisStore) applyScript(ctx context.Context, request ApplyRequest) (ApplyOutcome, error) {
	after := request.After
	digest, err := applyDigest(request)
	if err != nil {
		return 0, err
	}
	doc, err := encodeRecordDoc(after)
	if err != nil {
		return 0, err
	}
	terminalAt := int64(0)
	if after.Status.Terminal() {
		terminalAt = millis(after.UpdatedAt)
	}
	var receiptID, receiptDigest string
	var receiptAt int64
	if request.Receipt != nil {
		receiptID, receiptAt = request.Receipt.CommandID, millis(request.Receipt.CompletedAt)
		if receiptDigest, err = receiptDigestHex(*request.Receipt); err != nil {
			return 0, err
		}
	}
	closure := ""
	if request.CloseOperation != "" {
		closure = operationClosureAbandoned
		if request.Receipt != nil && request.Receipt.Success {
			closure = operationClosureResult
		}
	}
	var commandID, commandOp, command string
	var commandNext, commandAttempt, commandCreated, commandLeaseToken, commandLeaseUntil int64
	var commandLeaseOwner string
	if request.Outbox != nil {
		outbox := request.Outbox
		if err := outbox.Command.Validate(); err != nil {
			return 0, err
		}
		if command, err = encodeCommand(outbox.Command); err != nil {
			return 0, err
		}
		commandID, commandOp = outbox.Command.ID, outbox.Command.IdempotencyKey
		commandNext, commandAttempt, commandCreated = millis(outbox.NextAttemptAt), int64(outbox.Attempt), millis(outbox.CreatedAt)
		commandLeaseOwner, commandLeaseToken, commandLeaseUntil = outbox.Lease.Owner, int64(outbox.Lease.Token), millis(outbox.Lease.Until)
	}
	partition := s.keys.partition(after.ID)
	keys := []string{s.keys.record(after.ID), s.keys.due(), s.keys.evict(partition), s.keys.log(partition), s.keys.outbox()}
	for reloaded := false; ; reloaded = true {
		code, _, err := s.run(ctx, "apply", keys, s.keys.prefix, after.ID, request.ExpectedVersion, request.ExpectedLease.Owner, request.ExpectedLease.Token,
			after.Version, int(after.Status), millis(after.NextRunAt), after.Lease.Owner, after.Lease.Token, millis(after.Lease.Until), terminalAt, doc, digest,
			s.opts.TerminalRetention.Milliseconds(), receiptID, receiptDigest, receiptAt, request.CloseOperation, closure, millis(after.UpdatedAt),
			commandID, commandOp, command, commandNext, commandAttempt, commandCreated, commandLeaseOwner, commandLeaseToken, commandLeaseUntil)
		if err != nil {
			return 0, err
		}
		switch code {
		case "APPLIED", "APPLIED_BEFORE":
			return ApplyApplied, nil
		case "DUPLICATE":
			return ApplyDuplicate, nil
		case "CONFLICT":
			return 0, ErrConflict
		case "IDENTITY_CONFLICT":
			return 0, ErrIdentityConflict
		case "NEED_RELOAD":
			// 记录已驱逐（只在冷副本里）：冷记录要写，先装回再重跑一次；装回之后仍不在说明刚被驱逐或从未存在。
			if reloaded {
				return 0, ErrConflict
			}
			found, err := s.reload(ctx, after.ID)
			if err != nil {
				return 0, err
			}
			if !found {
				return 0, ErrNotFound
			}
		default:
			return 0, fmt.Errorf("saga redis: apply returned %q", code)
		}
	}
}

// ClaimOutbox 原子领取到期且租约过期的命令（claim_outbox.lua）。
func (s *RedisStore) ClaimOutbox(ctx context.Context, request ClaimRequest) ([]OutboxRecord, error) {
	if err := validateClaim(request); err != nil {
		return nil, err
	}
	_, items, err := s.run(ctx, "claim_outbox", []string{s.keys.outbox()}, request.Now.UnixMilli(), request.Owner, request.LeaseDuration.Milliseconds(), request.Limit, s.keys.prefix)
	if err != nil {
		return nil, err
	}
	out := make([]OutboxRecord, 0, len(items)/7)
	for i := 0; i+6 < len(items); i += 7 {
		command, err := decodeCommand(replyString(items, i+1))
		record := OutboxRecord{Command: command, Attempt: uint32(parseInt64(replyString(items, i+2))), NextAttemptAt: fromMillis(parseInt64(replyString(items, i+3))),
			Lease:     Lease{Owner: request.Owner, Token: uint64(parseInt64(replyString(items, i+4))), Until: fromMillis(parseInt64(replyString(items, i+5)))},
			CreatedAt: fromMillis(parseInt64(replyString(items, i+6)))}
		if err != nil || record.NextAttemptAt.IsZero() || record.CreatedAt.IsZero() {
			return out, ErrInvalidRecord
		}
		out = append(out, record)
	}
	return out, nil
}

// AckOutbox 删除已发布的命令。命令已被关闭操作或同一操作的新尝试删掉时返回 nil（等价于已确认，不计冲突）；
// 租约不符返回 ErrConflict。
func (s *RedisStore) AckOutbox(ctx context.Context, id string, lease Lease) error {
	code, _, err := s.run(ctx, "ack_outbox", []string{s.keys.command(id), s.keys.outbox()}, id, lease.Owner, lease.Token, s.keys.prefix)
	return outboxResult("ack_outbox", code, err)
}

// NackOutbox 清租约、attempt 加一、next 之后再发布。命令已不在时返回 nil；租约不符返回 ErrConflict。
func (s *RedisStore) NackOutbox(ctx context.Context, id string, lease Lease, next time.Time, lastError string) error {
	code, _, err := s.run(ctx, "nack_outbox", []string{s.keys.command(id), s.keys.outbox()}, id, lease.Owner, lease.Token, millis(next), lastError)
	return outboxResult("nack_outbox", code, err)
}

func outboxResult(name, code string, err error) error {
	if err != nil {
		return err
	}
	switch code {
	case "OK", "GONE":
		return nil
	case "CONFLICT":
		return ErrConflict
	default:
		return fmt.Errorf("saga redis: %s returned %q", name, code)
	}
}

var (
	_ Store                  = (*RedisStore)(nil)
	_ CompletionHistoryStore = (*RedisStore)(nil)
	_ LateSuccessAlarmStore  = (*RedisStore)(nil)
)
