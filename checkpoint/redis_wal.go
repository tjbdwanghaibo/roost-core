package checkpoint

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/cube-core/health"
	"github.com/tjbdwanghaibo/cube-core/obs"
	fredis "github.com/tjbdwanghaibo/cube-core/redis"
)

const (
	defaultRedisSnapshotWALPrefix          = "cube:checkpoint:wal"
	defaultRedisSnapshotWALShards          = 16
	defaultRedisSnapshotWALWorkerCount     = 4
	defaultRedisSnapshotWALQueueCap        = 4096
	defaultRedisSnapshotWALReplayBatchSize = 200
)

const redisSnapshotWALWriteScript = `
local function decimal_greater(left, right)
    return string.len(left) > string.len(right) or
           (string.len(left) == string.len(right) and left > right)
end
local current = redis.call('HGET', KEYS[3], ARGV[1])
if current then
    local current_version = string.match(current, '^[sd]:(%d+):')
    local incoming_version = ARGV[6]
    if current_version then
        if decimal_greater(current_version, incoming_version) then
            return 0
        end
        if current_version == incoming_version then
            local current_fence = string.match(current, '^[sd]:%d+:(%d+):') or '0'
            local incoming_fence = ARGV[7]
            if decimal_greater(current_fence, incoming_fence) then
                return 0
            end
            if current_fence == incoming_fence and string.sub(current, 1, 1) == 'd' and string.sub(ARGV[3], 1, 1) ~= 'd' then
                return 0
            end
        end
    end
end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[2])
redis.call('HSET', KEYS[3], ARGV[1], ARGV[3])
redis.call('ZADD', KEYS[2], ARGV[4], ARGV[1])
local ttl = tonumber(ARGV[5])
if ttl ~= nil and ttl > 0 then
    redis.call('PEXPIRE', KEYS[1], ttl)
    redis.call('PEXPIRE', KEYS[2], ttl)
    redis.call('PEXPIRE', KEYS[3], ttl)
end
return 1
`

const redisSnapshotWALAckScript = `
local cleaned = 0
for i = 1, #ARGV, 2 do
    local target = ARGV[i]
    local expected = ARGV[i + 1]
    if redis.call('HGET', KEYS[3], target) == expected then
        redis.call('HDEL', KEYS[1], target)
        redis.call('HDEL', KEYS[3], target)
        redis.call('ZREM', KEYS[2], target)
        cleaned = cleaned + 1
    end
end
return cleaned
`

// RedisSnapshotWALConfig configures the Redis snapshot WAL.
type RedisSnapshotWALConfig struct {
	Prefix          string
	Shards          int
	WorkerCount     int
	QueueCap        int
	TTL             time.Duration
	ReplayBatchSize int
	RequireAOF      bool
	AOFLocal        int
	AOFReplicas     int
	AOFTimeout      time.Duration
}

func (c RedisSnapshotWALConfig) normalize() RedisSnapshotWALConfig {
	c.Prefix = strings.TrimRight(strings.TrimSpace(c.Prefix), ":")
	if c.Prefix == "" {
		c.Prefix = defaultRedisSnapshotWALPrefix
	}
	if c.Shards <= 0 {
		c.Shards = defaultRedisSnapshotWALShards
	}
	if c.WorkerCount <= 0 {
		c.WorkerCount = defaultRedisSnapshotWALWorkerCount
	}
	if c.QueueCap <= 0 {
		c.QueueCap = defaultRedisSnapshotWALQueueCap
	}
	if c.ReplayBatchSize <= 0 {
		c.ReplayBatchSize = defaultRedisSnapshotWALReplayBatchSize
	}
	if c.AOFLocal < 0 || c.AOFLocal > 1 {
		c.AOFLocal = 0
	}
	if c.AOFReplicas < 0 {
		c.AOFReplicas = 0
	}
	if c.RequireAOF && c.AOFLocal == 0 && c.AOFReplicas == 0 {
		c.AOFLocal = 1
	}
	if c.RequireAOF && c.AOFTimeout <= 0 {
		c.AOFTimeout = 3 * time.Second
	}
	return c
}

type SnapshotWALStats struct {
	Submitted int64
	Written   int64
	Acked     int64
	Dropped   int64
	Errors    int64
	Replayed  int64
	Cleaned   int64
}

type redisSnapshotWALPayload struct {
	Operation  string        `json:"operation,omitempty"`
	Db         string        `json:"db,omitempty"`
	DbScope    DatabaseScope `json:"db_scope,omitempty"`
	Collection string        `json:"collection"`
	ID         int64         `json:"id"`
	Version    uint64        `json:"version"`
	Fence      uint64        `json:"fence,omitempty"`
	OwnerSid   int32         `json:"owner_sid,omitempty"`
	Shared     bool          `json:"shared,omitempty"`
	Mask       uint64        `json:"mask,omitempty"`
	Mode       SaveMode      `json:"mode"`
	Data       []byte        `json:"data"`
	CreatedAt  int64         `json:"created_at"`
}

const (
	redisSnapshotWALOperationSave   = "save"
	redisSnapshotWALOperationDelete = "delete"
)

func (p redisSnapshotWALPayload) saveOp() SaveOp {
	return SaveOp{
		Db:         p.Db,
		DbScope:    p.DbScope,
		Collection: p.Collection,
		ID:         p.ID,
		Version:    p.Version,
		Fence:      p.Fence,
		OwnerSid:   p.OwnerSid,
		Shared:     p.Shared,
		Mask:       p.Mask,
		Mode:       SaveModeFull,
		Data:       append([]byte(nil), p.Data...),
	}
}

func (p redisSnapshotWALPayload) removeOp() RemoveOp {
	return RemoveOp{Db: p.Db, DbScope: p.DbScope, Collection: p.Collection, Items: []RemoveItem{{ID: p.ID, Version: p.Version, Fence: p.Fence, OwnerSid: p.OwnerSid, Shared: p.Shared}}}
}

type redisSnapshotWALTaskKind uint8

const (
	redisSnapshotWALTaskWrite redisSnapshotWALTaskKind = iota + 1
	redisSnapshotWALTaskAck
)

func (k redisSnapshotWALTaskKind) metricValue() string {
	switch k {
	case redisSnapshotWALTaskWrite:
		return "write"
	case redisSnapshotWALTaskAck:
		return "ack"
	default:
		return "unknown"
	}
}

type redisSnapshotWALTask struct {
	kind    redisSnapshotWALTaskKind
	target  string
	token   string
	shard   int
	payload redisSnapshotWALPayload
}

// RedisSnapshotWAL is a Redis-backed snapshot buffer for checkpoint.
// RequireAOF turns accepted writes into fail-closed durable admissions.
type RedisSnapshotWAL struct {
	redis fredis.IRedis
	cfg   RedisSnapshotWALConfig

	mu      sync.RWMutex
	queues  []chan redisSnapshotWALTask
	wg      sync.WaitGroup
	running atomic.Bool
	ctx     context.Context
	cancel  context.CancelFunc

	submitted atomic.Int64
	written   atomic.Int64
	acked     atomic.Int64
	dropped   atomic.Int64
	errs      atomic.Int64
	replayed  atomic.Int64
	cleaned   atomic.Int64
}

type RedisSnapshotWALHealthPolicy struct {
	MaxDropped int64
	MaxErrors  int64
}

func NewRedisSnapshotWAL(redis fredis.IRedis, cfg RedisSnapshotWALConfig) *RedisSnapshotWAL {
	return &RedisSnapshotWAL{
		redis: redis,
		cfg:   cfg.normalize(),
	}
}

func (w *RedisSnapshotWAL) Start() {
	if w == nil || w.redis == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.running.Load() {
		return
	}
	w.queues = make([]chan redisSnapshotWALTask, w.cfg.WorkerCount)
	w.ctx, w.cancel = context.WithCancel(context.Background())
	perWorkerCap := w.cfg.QueueCap / w.cfg.WorkerCount
	if perWorkerCap <= 0 {
		perWorkerCap = 1
	}
	for i := range w.queues {
		ch := make(chan redisSnapshotWALTask, perWorkerCap)
		w.queues[i] = ch
		w.wg.Add(1)
		go w.worker(w.ctx, ch)
	}
	w.running.Store(true)
}

func (w *RedisSnapshotWAL) Stop(ctx context.Context) error {
	if w == nil || !w.running.Load() {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	w.mu.Lock()
	if !w.running.Load() {
		w.mu.Unlock()
		return nil
	}
	w.running.Store(false)
	queues := w.queues
	cancel := w.cancel
	w.queues = nil
	w.ctx = nil
	w.cancel = nil
	for _, ch := range queues {
		close(ch)
	}
	w.mu.Unlock()
	if err := ctx.Err(); err != nil {
		if cancel != nil {
			cancel()
		}
		return err
	}

	done := make(chan struct{})
	go func() {
		w.wg.Wait()
		close(done)
	}()
	select {
	case <-done:
		if cancel != nil {
			cancel()
		}
		return nil
	case <-ctx.Done():
		if cancel != nil {
			cancel()
		}
		return ctx.Err()
	}
}

func (w *RedisSnapshotWAL) Submit(items []SaveItem) bool {
	if w == nil || w.redis == nil || len(items) == 0 {
		return true
	}
	ok := true
	for _, item := range items {
		task, valid := w.writeTask(item)
		if !valid {
			continue
		}
		if !w.enqueue(task) {
			ok = false
		}
	}
	return ok
}

func (w *RedisSnapshotWAL) SubmitDelete(items []SaveItem) bool {
	if w == nil || w.redis == nil || len(items) == 0 {
		return true
	}
	ok := true
	for _, item := range items {
		task, valid := w.deleteTask(item)
		if valid && !w.enqueue(task) {
			ok = false
		}
	}
	return ok
}

func (w *RedisSnapshotWAL) SubmitDurable(ctx context.Context, items []SaveItem) bool {
	if w == nil || w.redis == nil || len(items) == 0 {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tasks := make([]redisSnapshotWALTask, 0, len(items))
	for _, item := range items {
		task, valid := w.writeTask(item)
		if !valid {
			continue
		}
		w.submitted.Add(1)
		obs.IncCounter("checkpoint_redis_wal_submit_total", obs.Labels{"kind": task.kind.metricValue(), "result": "ok"}, 1)
		tasks = append(tasks, task)
	}
	if err := w.writeSnapshotsDurable(ctx, tasks); err != nil {
		w.errs.Add(1)
		w.recordTaskError(redisSnapshotWALTaskWrite)
		slog.Warn("checkpoint redis wal durable submit failed", "items", len(tasks), "err", err)
		return false
	}
	return true
}

func (w *RedisSnapshotWAL) SubmitDeleteDurable(ctx context.Context, items []SaveItem) bool {
	if w == nil || w.redis == nil || len(items) == 0 {
		return true
	}
	if ctx == nil {
		ctx = context.Background()
	}
	tasks := make([]redisSnapshotWALTask, 0, len(items))
	for _, item := range items {
		task, valid := w.deleteTask(item)
		if !valid {
			continue
		}
		w.submitted.Add(1)
		obs.IncCounter("checkpoint_redis_wal_submit_total", obs.Labels{"kind": task.kind.metricValue(), "result": "ok"}, 1)
		tasks = append(tasks, task)
	}
	if err := w.writeSnapshotsDurable(ctx, tasks); err != nil {
		w.errs.Add(1)
		w.recordTaskError(redisSnapshotWALTaskWrite)
		slog.Warn("checkpoint redis wal durable delete submit failed", "items", len(tasks), "err", err)
		return false
	}
	return true
}

func (w *RedisSnapshotWAL) Ack(ctx context.Context, items []SaveItem) error {
	if w == nil || w.redis == nil || len(items) == 0 {
		return nil
	}
	tasks := make([]redisSnapshotWALTask, 0, len(items))
	for _, item := range items {
		task, valid := w.ackTask(item)
		if !valid {
			continue
		}
		tasks = append(tasks, task)
	}
	if len(tasks) == 0 {
		return nil
	}
	if w.running.Load() {
		for _, task := range tasks {
			if !w.enqueue(task) {
				return errors.New("checkpoint redis wal: ack queue is full")
			}
		}
		return nil
	}
	return w.ackTasks(ctx, tasks)
}

func (w *RedisSnapshotWAL) Replay(ctx context.Context, backend StorageBackend) error {
	if w == nil || w.redis == nil || backend == nil {
		return nil
	}
	for shard := 0; shard < w.cfg.Shards; shard++ {
		if err := w.replayShard(ctx, backend, shard); err != nil {
			return err
		}
	}
	return nil
}

func (w *RedisSnapshotWAL) Stats() SnapshotWALStats {
	if w == nil {
		return SnapshotWALStats{}
	}
	return SnapshotWALStats{
		Submitted: w.submitted.Load(),
		Written:   w.written.Load(),
		Acked:     w.acked.Load(),
		Dropped:   w.dropped.Load(),
		Errors:    w.errs.Load(),
		Replayed:  w.replayed.Load(),
		Cleaned:   w.cleaned.Load(),
	}
}

func (w *RedisSnapshotWAL) CheckHealth(policy RedisSnapshotWALHealthPolicy) health.Result {
	stats := w.Stats()
	switch {
	case stats.Errors > policy.MaxErrors:
		return health.Result{Status: health.StatusFail, Message: fmt.Sprintf("errors=%d max=%d", stats.Errors, policy.MaxErrors)}
	case stats.Dropped > policy.MaxDropped:
		return health.Result{Status: health.StatusFail, Message: fmt.Sprintf("dropped=%d max=%d", stats.Dropped, policy.MaxDropped)}
	default:
		return health.Result{Status: health.StatusOK, Message: "ok"}
	}
}

func (w *RedisSnapshotWAL) worker(ctx context.Context, ch <-chan redisSnapshotWALTask) {
	defer w.wg.Done()
	for task := range ch {
		var err error
		switch task.kind {
		case redisSnapshotWALTaskWrite:
			err = w.writeSnapshot(ctx, task)
		case redisSnapshotWALTaskAck:
			err = w.ackTasks(ctx, []redisSnapshotWALTask{task})
		}
		if err != nil {
			w.errs.Add(1)
			w.recordTaskError(task.kind)
			slog.Warn("checkpoint redis wal task failed", "kind", task.kind, "target", task.target, "err", err)
		}
	}
}

func (w *RedisSnapshotWAL) enqueue(task redisSnapshotWALTask) bool {
	w.mu.RLock()
	defer w.mu.RUnlock()
	if !w.running.Load() || len(w.queues) == 0 {
		w.dropped.Add(1)
		obs.IncCounter("checkpoint_redis_wal_submit_total", obs.Labels{"kind": task.kind.metricValue(), "result": "dropped"}, 1)
		return false
	}
	idx := redisSnapshotWALWorker(task.target, len(w.queues))
	select {
	case w.queues[idx] <- task:
		w.submitted.Add(1)
		obs.IncCounter("checkpoint_redis_wal_submit_total", obs.Labels{"kind": task.kind.metricValue(), "result": "ok"}, 1)
		return true
	default:
		w.dropped.Add(1)
		obs.IncCounter("checkpoint_redis_wal_submit_total", obs.Labels{"kind": task.kind.metricValue(), "result": "dropped"}, 1)
		return false
	}
}

func (w *RedisSnapshotWAL) writeTask(item SaveItem) (redisSnapshotWALTask, bool) {
	if item.Collection == "" || item.ID == 0 || item.Version == 0 || item.Deleted {
		return redisSnapshotWALTask{}, false
	}
	data := item.Data
	if item.Mode == SaveModePatch && len(item.Patch.FullData) > 0 {
		data = item.Patch.FullData
	}
	if len(data) == 0 {
		return redisSnapshotWALTask{}, false
	}
	target := redisSnapshotWALTarget(item)
	payload := redisSnapshotWALPayload{
		Operation:  redisSnapshotWALOperationSave,
		Db:         item.Db,
		DbScope:    item.DbScope,
		Collection: item.Collection,
		ID:         item.ID,
		Version:    item.Version,
		Fence:      item.Fence,
		OwnerSid:   item.OwnerSid,
		Shared:     item.Shared,
		Mask:       item.Mask,
		Mode:       item.Mode,
		Data:       append([]byte(nil), data...),
		CreatedAt:  time.Now().UnixNano(),
	}
	return redisSnapshotWALTask{
		kind:    redisSnapshotWALTaskWrite,
		target:  target,
		token:   redisSnapshotWALToken(item),
		shard:   redisSnapshotWALShard(target, w.cfg.Shards),
		payload: payload,
	}, true
}

func (w *RedisSnapshotWAL) deleteTask(item SaveItem) (redisSnapshotWALTask, bool) {
	if item.Collection == "" || item.ID == 0 || item.Version == 0 {
		return redisSnapshotWALTask{}, false
	}
	target := redisSnapshotWALTarget(item)
	return redisSnapshotWALTask{
		kind: redisSnapshotWALTaskWrite, target: target, token: redisSnapshotWALToken(item), shard: redisSnapshotWALShard(target, w.cfg.Shards),
		payload: redisSnapshotWALPayload{
			Operation: redisSnapshotWALOperationDelete, Db: item.Db, DbScope: item.DbScope,
			Collection: item.Collection, ID: item.ID, Version: item.Version, Fence: item.Fence, OwnerSid: item.OwnerSid,
			Shared: item.Shared, CreatedAt: time.Now().UnixNano(),
		},
	}, true
}

func (w *RedisSnapshotWAL) ackTask(item SaveItem) (redisSnapshotWALTask, bool) {
	if item.Collection == "" || item.ID == 0 || item.Version == 0 {
		return redisSnapshotWALTask{}, false
	}
	target := redisSnapshotWALTarget(item)
	return redisSnapshotWALTask{
		kind:   redisSnapshotWALTaskAck,
		target: target,
		token:  redisSnapshotWALToken(item),
		shard:  redisSnapshotWALShard(target, w.cfg.Shards),
	}, true
}

func (w *RedisSnapshotWAL) writeSnapshot(ctx context.Context, task redisSnapshotWALTask) error {
	call, err := w.snapshotEval(task)
	if err != nil {
		return err
	}
	var result any
	if w.cfg.RequireAOF {
		durable, ok := w.redis.(fredis.DurableEvaler)
		if !ok {
			return errors.New("checkpoint: redis client does not support same-connection AOF durability")
		}
		var local, replicas int64
		result, local, replicas, err = durable.EvalDurable(ctx, redisSnapshotWALWriteScript, call.Keys, w.cfg.AOFLocal, w.cfg.AOFReplicas, w.cfg.AOFTimeout, call.Args...)
		if err == nil {
			err = w.verifyAOF(local, replicas)
		}
	} else {
		result, err = w.redis.Eval(ctx, redisSnapshotWALWriteScript, call.Keys, call.Args...)
	}
	if err != nil {
		return err
	}
	return w.recordWriteResult(result)
}

func (w *RedisSnapshotWAL) writeSnapshotsDurable(ctx context.Context, tasks []redisSnapshotWALTask) error {
	if len(tasks) == 0 {
		return nil
	}
	if !w.cfg.RequireAOF {
		for _, task := range tasks {
			if err := w.writeSnapshot(ctx, task); err != nil {
				return err
			}
		}
		return nil
	}
	batcher, ok := w.redis.(fredis.DurableBatchEvaler)
	if !ok {
		return errors.New("checkpoint: redis client does not support batched same-connection AOF durability")
	}
	calls := make([]fredis.EvalCall, 0, len(tasks))
	for _, task := range tasks {
		call, err := w.snapshotEval(task)
		if err != nil {
			return err
		}
		calls = append(calls, call)
	}
	results, local, replicas, err := batcher.EvalBatchDurable(ctx, redisSnapshotWALWriteScript, calls, w.cfg.AOFLocal, w.cfg.AOFReplicas, w.cfg.AOFTimeout)
	if err != nil {
		return err
	}
	if err := w.verifyAOF(local, replicas); err != nil {
		return err
	}
	if len(results) != len(calls) {
		return fmt.Errorf("checkpoint: redis durable batch returned %d results for %d calls", len(results), len(calls))
	}
	for _, result := range results {
		if err := w.recordWriteResult(result); err != nil {
			return err
		}
	}
	return nil
}

func (w *RedisSnapshotWAL) snapshotEval(task redisSnapshotWALTask) (fredis.EvalCall, error) {
	raw, err := json.Marshal(task.payload)
	if err != nil {
		return fredis.EvalCall{}, err
	}
	snapshotKey := redisSnapshotWALSnapshotKey(w.cfg.Prefix, task.shard)
	pendingKey := redisSnapshotWALPendingKey(w.cfg.Prefix, task.shard)
	tokenKey := redisSnapshotWALTokenKey(w.cfg.Prefix, task.shard)
	score := float64(task.payload.CreatedAt)
	ttlMillis := w.cfg.TTL.Milliseconds()
	if w.cfg.TTL > 0 && ttlMillis <= 0 {
		ttlMillis = 1
	}
	return fredis.EvalCall{
		Keys: []string{snapshotKey, pendingKey, tokenKey},
		Args: []any{task.target, raw, task.token, score, ttlMillis, strconv.FormatUint(task.payload.Version, 10), strconv.FormatUint(task.payload.Fence, 10)},
	}, nil
}

func (w *RedisSnapshotWAL) verifyAOF(local, replicas int64) error {
	if local < int64(w.cfg.AOFLocal) || replicas < int64(w.cfg.AOFReplicas) {
		return fmt.Errorf("checkpoint: redis AOF durability threshold not met: local=%d/%d replicas=%d/%d", local, w.cfg.AOFLocal, replicas, w.cfg.AOFReplicas)
	}
	return nil
}

func (w *RedisSnapshotWAL) recordWriteResult(result any) error {
	written, err := redisInteger(result)
	if err != nil {
		return err
	}
	if written == 0 {
		obs.IncCounter("checkpoint_redis_wal_write_total", obs.Labels{"result": "stale"}, 1)
		return nil
	}
	w.written.Add(1)
	obs.IncCounter("checkpoint_redis_wal_write_total", obs.Labels{"result": "ok"}, 1)
	return nil
}

func (w *RedisSnapshotWAL) ackTasks(ctx context.Context, tasks []redisSnapshotWALTask) error {
	if len(tasks) == 0 {
		return nil
	}
	grouped := make(map[int]map[string]string)
	for _, task := range tasks {
		if task.target == "" || task.token == "" {
			continue
		}
		if grouped[task.shard] == nil {
			grouped[task.shard] = make(map[string]string)
		}
		grouped[task.shard][task.target] = task.token
	}
	for shard, set := range grouped {
		args := make([]any, 0, len(set)*2)
		for target, token := range set {
			args = append(args, target, token)
		}
		result, err := w.redis.Eval(ctx, redisSnapshotWALAckScript, []string{
			redisSnapshotWALSnapshotKey(w.cfg.Prefix, shard),
			redisSnapshotWALPendingKey(w.cfg.Prefix, shard),
			redisSnapshotWALTokenKey(w.cfg.Prefix, shard),
		}, args...)
		if err != nil {
			obs.IncCounter("checkpoint_redis_wal_ack_total", obs.Labels{"result": "error"}, 1)
			return err
		}
		cleaned, err := redisInteger(result)
		if err != nil {
			return err
		}
		w.acked.Add(cleaned)
		w.cleaned.Add(cleaned)
		obs.IncCounter("checkpoint_redis_wal_ack_total", obs.Labels{"result": "ok"}, cleaned)
		obs.IncCounter("checkpoint_redis_wal_clean_total", obs.Labels{"result": "ok"}, cleaned)
	}
	return nil
}

func (w *RedisSnapshotWAL) replayShard(ctx context.Context, backend StorageBackend, shard int) error {
	pendingKey := redisSnapshotWALPendingKey(w.cfg.Prefix, shard)
	snapshotKey := redisSnapshotWALSnapshotKey(w.cfg.Prefix, shard)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		pending, err := w.redis.ZRangeWithScores(ctx, pendingKey, 0, int64(w.cfg.ReplayBatchSize-1))
		if err != nil {
			return err
		}
		if len(pending) == 0 {
			return nil
		}
		payloads, staleTargets, err := w.loadReplayPayloads(ctx, snapshotKey, pending)
		if err != nil {
			return err
		}
		if len(staleTargets) > 0 {
			if err := w.ackTargets(ctx, shard, staleTargets); err != nil {
				return err
			}
		}
		if len(payloads) == 0 {
			continue
		}
		if err := w.replayPayloadBatch(ctx, backend, shard, payloads); err != nil {
			return err
		}
	}
}

func (w *RedisSnapshotWAL) replayPayloadBatch(ctx context.Context, backend StorageBackend, shard int, payloads []redisSnapshotWALPayload) error {
	saveOps := make([]SaveOp, 0, len(payloads))
	saveTasks := make([]redisSnapshotWALTask, 0, len(payloads))
	deleteGroups := make(map[removeKey][]redisSnapshotWALPayload)
	for _, payload := range payloads {
		target := redisSnapshotWALPayloadTarget(payload)
		task := redisSnapshotWALTask{kind: redisSnapshotWALTaskAck, shard: shard, target: target, token: redisSnapshotWALPayloadToken(payload)}
		if payload.Operation == redisSnapshotWALOperationDelete {
			key := removeKey{db: payload.Db, dbScope: payload.DbScope, coll: payload.Collection, fence: payload.Fence, ownerSid: payload.OwnerSid, shared: payload.Shared}
			deleteGroups[key] = append(deleteGroups[key], payload)
			continue
		}
		saveOps = append(saveOps, payload.saveOp())
		saveTasks = append(saveTasks, task)
	}
	if len(saveOps) > 0 {
		results, err := backend.BulkSave(ctx, saveOps)
		if err != nil {
			return err
		}
		if len(results) != len(saveOps) {
			return fmt.Errorf("checkpoint redis wal replay: result count %d, want %d", len(results), len(saveOps))
		}
		acks := make([]redisSnapshotWALTask, 0, len(saveTasks))
		for i, result := range results {
			if !result.OK && !result.VersionConflict {
				if result.Err != nil {
					return fmt.Errorf("checkpoint redis wal replay %s/%d: %w", saveOps[i].Collection, saveOps[i].ID, result.Err)
				}
				return fmt.Errorf("checkpoint redis wal replay %s/%d failed", saveOps[i].Collection, saveOps[i].ID)
			}
			acks = append(acks, saveTasks[i])
		}
		if err := w.ackTasks(ctx, acks); err != nil {
			return err
		}
		w.recordReplayed(len(acks))
	}
	for key, group := range deleteGroups {
		acks := make([]redisSnapshotWALTask, 0, len(group))
		for _, payload := range group {
			acks = append(acks, redisSnapshotWALTask{kind: redisSnapshotWALTaskAck, shard: shard, target: redisSnapshotWALPayloadTarget(payload), token: redisSnapshotWALPayloadToken(payload)})
		}
		removeItems := make([]RemoveItem, 0, len(group))
		for _, payload := range group {
			removeItems = append(removeItems, RemoveItem{ID: payload.ID, Version: payload.Version, Fence: payload.Fence, OwnerSid: payload.OwnerSid, Shared: payload.Shared})
		}
		if err := backend.BulkRemove(ctx, RemoveOp{Db: key.db, DbScope: key.dbScope, Collection: key.coll, Items: removeItems}); err != nil {
			return err
		}
		if err := w.ackTasks(ctx, acks); err != nil {
			return err
		}
		w.recordReplayed(len(acks))
	}
	return nil
}

func (w *RedisSnapshotWAL) recordReplayed(count int) {
	if count <= 0 {
		return
	}
	w.replayed.Add(int64(count))
	obs.IncCounter("checkpoint_redis_wal_replay_total", obs.Labels{"result": "ok"}, int64(count))
}

func (w *RedisSnapshotWAL) recordTaskError(kind redisSnapshotWALTaskKind) {
	switch kind {
	case redisSnapshotWALTaskWrite:
		obs.IncCounter("checkpoint_redis_wal_write_total", obs.Labels{"result": "error"}, 1)
	case redisSnapshotWALTaskAck:
		obs.IncCounter("checkpoint_redis_wal_ack_total", obs.Labels{"result": "error"}, 1)
	}
}

func (w *RedisSnapshotWAL) loadReplayPayloads(ctx context.Context, snapshotKey string, pending []fredis.Z) ([]redisSnapshotWALPayload, []string, error) {
	payloads := make([]redisSnapshotWALPayload, 0, len(pending))
	staleTargets := make([]string, 0)
	if pipe := w.redis.Pipeline(); pipe != nil {
		futures := make(map[string]*fredis.FutureBytes, len(pending))
		for _, item := range pending {
			futures[item.Member] = pipe.HGet(ctx, snapshotKey, item.Member)
		}
		if err := pipe.Exec(ctx); err != nil {
			return nil, nil, err
		}
		for _, item := range pending {
			raw, err := futures[item.Member].Result()
			if err != nil {
				if errors.Is(err, fredis.ErrNil) {
					staleTargets = append(staleTargets, item.Member)
					continue
				}
				return nil, nil, err
			}
			payload, err := decodeRedisSnapshotWALPayload(raw)
			if err != nil {
				return nil, nil, err
			}
			payloads = append(payloads, payload)
		}
		return payloads, staleTargets, nil
	}
	for _, item := range pending {
		raw, err := w.redis.HGet(ctx, snapshotKey, item.Member)
		if err != nil {
			if errors.Is(err, fredis.ErrNil) {
				staleTargets = append(staleTargets, item.Member)
				continue
			}
			return nil, nil, err
		}
		payload, err := decodeRedisSnapshotWALPayload(raw)
		if err != nil {
			return nil, nil, err
		}
		payloads = append(payloads, payload)
	}
	return payloads, staleTargets, nil
}

func (w *RedisSnapshotWAL) ackTargets(ctx context.Context, shard int, targets []string) error {
	if len(targets) == 0 {
		return nil
	}
	if _, err := w.redis.HDel(ctx, redisSnapshotWALSnapshotKey(w.cfg.Prefix, shard), targets...); err != nil {
		return err
	}
	if _, err := w.redis.HDel(ctx, redisSnapshotWALTokenKey(w.cfg.Prefix, shard), targets...); err != nil {
		return err
	}
	members := make([]any, len(targets))
	for i := range targets {
		members[i] = targets[i]
	}
	_, err := w.redis.ZRem(ctx, redisSnapshotWALPendingKey(w.cfg.Prefix, shard), members...)
	return err
}

func decodeRedisSnapshotWALPayload(raw []byte) (redisSnapshotWALPayload, error) {
	var payload redisSnapshotWALPayload
	if err := json.Unmarshal(raw, &payload); err != nil {
		return redisSnapshotWALPayload{}, err
	}
	if payload.Collection == "" || payload.ID == 0 ||
		(payload.Operation != redisSnapshotWALOperationSave && payload.Operation != redisSnapshotWALOperationDelete) ||
		payload.Version == 0 || (payload.Operation == redisSnapshotWALOperationSave && len(payload.Data) == 0) {
		return redisSnapshotWALPayload{}, fmt.Errorf("checkpoint redis wal: invalid payload operation=%q collection=%q id=%d data=%d", payload.Operation, payload.Collection, payload.ID, len(payload.Data))
	}
	return payload, nil
}

func redisSnapshotWALPayloadTarget(payload redisSnapshotWALPayload) string {
	return redisSnapshotWALTarget(SaveItem{Db: payload.Db, DbScope: payload.DbScope, Collection: payload.Collection, ID: payload.ID})
}

func redisSnapshotWALTarget(item SaveItem) string {
	if item.DbScope == DatabaseScopeServer {
		return item.Db + "|server|" + item.Collection + "|" + strconv.FormatInt(item.ID, 10)
	}
	if item.Db != "" {
		return item.Db + "|" + item.Collection + "|" + strconv.FormatInt(item.ID, 10)
	}
	return item.Collection + "|" + strconv.FormatInt(item.ID, 10)
}

func redisSnapshotWALSnapshotKey(prefix string, shard int) string {
	return strings.TrimRight(prefix, ":") + ":{" + strconv.Itoa(shard) + "}:snapshot"
}

func redisSnapshotWALPendingKey(prefix string, shard int) string {
	return strings.TrimRight(prefix, ":") + ":{" + strconv.Itoa(shard) + "}:pending"
}

func redisSnapshotWALTokenKey(prefix string, shard int) string {
	return strings.TrimRight(prefix, ":") + ":{" + strconv.Itoa(shard) + "}:token"
}

func redisSnapshotWALToken(item SaveItem) string {
	kind := "s"
	if item.Deleted {
		kind = "d"
	}
	return kind + ":" + strconv.FormatUint(item.Version, 10) + ":" + strconv.FormatUint(item.Fence, 10) + ":" + strconv.FormatInt(int64(item.OwnerSid), 10)
}

func redisSnapshotWALPayloadToken(payload redisSnapshotWALPayload) string {
	return redisSnapshotWALToken(SaveItem{Version: payload.Version, Fence: payload.Fence, OwnerSid: payload.OwnerSid, Deleted: payload.Operation == redisSnapshotWALOperationDelete})
}

func redisInteger(value any) (int64, error) {
	switch typed := value.(type) {
	case int64:
		return typed, nil
	case int:
		return int64(typed), nil
	case string:
		return strconv.ParseInt(typed, 10, 64)
	case []byte:
		return strconv.ParseInt(string(typed), 10, 64)
	default:
		return 0, fmt.Errorf("checkpoint redis wal: unexpected integer result %T", value)
	}
}

func redisSnapshotWALShard(target string, shards int) int {
	if shards <= 1 {
		return 0
	}
	return int(redisSnapshotWALHash(target) % uint32(shards))
}

func redisSnapshotWALWorker(target string, workers int) int {
	if workers <= 1 {
		return 0
	}
	return int(redisSnapshotWALHash(target) % uint32(workers))
}

func redisSnapshotWALHash(target string) uint32 {
	h := fnv.New32a()
	_, _ = h.Write([]byte(target))
	return h.Sum32()
}
