package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"strconv"
	"time"
)

// 权威 Redis 上的键与编码。RedisStore（写入、读取）与 saga 投影（把日志写进 Mongo 冷副本）共用这一份，键表见
// docs/framework/SAGA.md「Redis 权威存储」。

// redisKeys 由配置的前缀派生全部键名。前缀在一个 Redis 上区分部署，不能含 hash tag（单主，不支持 Cluster）。
type redisKeys struct {
	prefix     string
	partitions int
}

func (k redisKeys) meta() string               { return k.prefix + ":meta" }
func (k redisKeys) record(id string) string    { return k.prefix + ":rec:" + id }
func (k redisKeys) due() string                { return k.prefix + ":due" }
func (k redisKeys) outbox() string             { return k.prefix + ":outbox" }
func (k redisKeys) command(id string) string   { return k.prefix + ":ob:" + id }
func (k redisKeys) evict(partition int) string { return k.prefix + ":evict:" + strconv.Itoa(partition) }
func (k redisKeys) log(partition int) string   { return k.prefix + ":log:" + strconv.Itoa(partition) }

// businessKey 是业务键守卫的键名：类型带长度前缀，类型与业务键里的冒号不会让两组 (type, key) 拼出同一个键名。
func (k redisKeys) businessKey(sagaType, businessKey string) string {
	return k.prefix + ":bk:" + strconv.Itoa(len(sagaType)) + ":" + sagaType + ":" + businessKey
}

// partition 是一个 saga 的投影分区：fnv32a(id) mod P。同一 saga 的日志条目都在这个分区里，分区内有序。
func (k redisKeys) partition(id string) int {
	h := fnv.New32a()
	_, _ = h.Write([]byte(id))
	return int(h.Sum32() % uint32(k.partitions))
}

// redisRecordDoc 是记录主体（哈希字段 doc 与日志条目里的 doc）。字段名与 Mongo recordDoc 的 bson 名相同；时间是毫秒整数
// （与 Mongo 的毫秒精度一致，热读与冷读得到同一个值），零值为 0。不含租约：租约只在哈希的标量字段里。
type redisRecordDoc struct {
	ID                string `json:"_id"`
	Type              string `json:"type"`
	DefinitionVersion uint32 `json:"definition_version"`
	BusinessKey       string `json:"business_key"`
	StartDigest       string `json:"start_digest,omitempty"`
	Status            Status `json:"status"`
	Phase             Phase  `json:"phase"`
	Step              int    `json:"step"`
	CompletedSteps    int    `json:"completed_steps"`
	Attempt           uint32 `json:"attempt"`
	Incarnation       uint32 `json:"incarnation,omitempty"`
	LateStep          int    `json:"late_step,omitempty"`
	LateData          []byte `json:"late_data,omitempty"`
	Version           uint64 `json:"version"`
	Data              []byte `json:"data,omitempty"`
	LastError         string `json:"last_error,omitempty"`
	OperationKey      string `json:"operation_key,omitempty"`
	CommandID         string `json:"command_id,omitempty"`
	NextRunAt         int64  `json:"next_run_at,omitempty"`
	DeadlineAt        int64  `json:"deadline_at,omitempty"`
	CreatedAt         int64  `json:"created_at"`
	UpdatedAt         int64  `json:"updated_at"`
}

func millis(t time.Time) int64 {
	if t.IsZero() {
		return 0
	}
	return t.UnixMilli()
}

func fromMillis(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

// encodeRecordDoc 编码记录主体（不含租约）。
func encodeRecordDoc(r Record) (string, error) {
	raw, err := json.Marshal(redisRecordDoc{ID: r.ID, Type: r.Type, DefinitionVersion: r.DefinitionVersion, BusinessKey: r.BusinessKey, StartDigest: r.StartDigest,
		Status: r.Status, Phase: r.Phase, Step: r.Step, CompletedSteps: r.CompletedSteps, Attempt: r.Attempt, Incarnation: r.Incarnation,
		LateStep: r.LateStep, LateData: r.LateData, Version: r.Version, Data: r.Data, LastError: r.LastError, OperationKey: r.OperationKey,
		CommandID: r.CommandID, NextRunAt: millis(r.NextRunAt), DeadlineAt: millis(r.DeadlineAt), CreatedAt: millis(r.CreatedAt), UpdatedAt: millis(r.UpdatedAt)})
	if err != nil {
		return "", fmt.Errorf("%w: encode record: %v", ErrInvalidRecord, err)
	}
	return string(raw), nil
}

// decodeRecordDoc 解码记录主体并合上租约，校验不过返回错误（调用方按坏记录处理）。
func decodeRecordDoc(doc string, lease Lease) (Record, error) {
	var d redisRecordDoc
	if err := json.Unmarshal([]byte(doc), &d); err != nil {
		return Record{}, fmt.Errorf("%w: decode record: %v", ErrInvalidRecord, err)
	}
	r := Record{ID: d.ID, Type: d.Type, DefinitionVersion: d.DefinitionVersion, BusinessKey: d.BusinessKey, StartDigest: d.StartDigest,
		Status: d.Status, Phase: d.Phase, Step: d.Step, CompletedSteps: d.CompletedSteps, Attempt: d.Attempt, Incarnation: d.Incarnation,
		LateStep: d.LateStep, Version: d.Version, Data: d.Data, LastError: d.LastError, OperationKey: d.OperationKey, CommandID: d.CommandID,
		NextRunAt: fromMillis(d.NextRunAt), DeadlineAt: fromMillis(d.DeadlineAt), CreatedAt: fromMillis(d.CreatedAt), UpdatedAt: fromMillis(d.UpdatedAt), Lease: lease}
	if len(d.LateData) > 0 {
		r.LateData = d.LateData
	}
	if err := r.Validate(); err != nil {
		return Record{}, fmt.Errorf("saga redis: corrupt record %q: %w", d.ID, err)
	}
	return r, nil
}

// leaseFromFields 由哈希的 lo / lt / lu 字段合出租约；owner 为空表示没有租约。
func leaseFromFields(owner, token, until string) (Lease, error) {
	if owner == "" {
		return Lease{}, nil
	}
	t, err := strconv.ParseUint(token, 10, 64)
	if err != nil {
		return Lease{}, fmt.Errorf("%w: lease token %q", ErrInvalidRecord, token)
	}
	u, err := strconv.ParseInt(until, 10, 64)
	if err != nil {
		return Lease{}, fmt.Errorf("%w: lease until %q", ErrInvalidRecord, until)
	}
	return Lease{Owner: owner, Token: t, Until: fromMillis(u)}, nil
}

// encodeCommand 编码 outbox 里的命令（与线上信封里的 Command 同一份 JSON）。
func encodeCommand(c Command) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", fmt.Errorf("%w: encode command: %v", ErrInvalidRecord, err)
	}
	return string(raw), nil
}

func decodeCommand(raw string) (Command, error) {
	var c Command
	if err := json.Unmarshal([]byte(raw), &c); err != nil {
		return Command{}, fmt.Errorf("%w: decode command: %v", ErrInvalidRecord, err)
	}
	return c, c.Validate()
}

// applyDigest 是一次 Apply 请求的摘要（apply.lua 记为 la）：回复丢失后同一请求重试时，脚本据此认出“已经应用过”。
// 覆盖整个请求，不同请求不会撞上同一个摘要。
func applyDigest(request ApplyRequest) (string, error) {
	raw, err := json.Marshal(request)
	if err != nil {
		return "", fmt.Errorf("%w: apply digest: %v", ErrInvalidRecord, err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

// receiptDigestHex 是回执摘要的十六进制形式（与 Mongo 回执的 digest 字段同一个 SHA-256）。
func receiptDigestHex(c Completion) (string, error) {
	digest, err := completionDigest(c)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(digest), nil
}

func parseInt64(value string) int64 {
	n, _ := strconv.ParseInt(value, 10, 64)
	return n
}
