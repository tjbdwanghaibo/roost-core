package dataengine

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// LeaseFenceReceiptNamespace is reserved for transaction preconditions. The
// record is carried in the existing WAL receipt section but is never exposed
// as a business receipt or staged in the receipt collection.
const LeaseFenceReceiptNamespace = "__dataengine_lease_fence_v1"

var ErrInvalidLeaseFence = errors.New("dataengine: invalid lease fence")

// ErrFencedEntityPending 表示该实体上有一笔本进程已准入、投影结果尚未确定的原生 saga 步骤
// （本地 mutation + lease fence receipt）。写同一实体的其他事务在 WAL 准入处被拒绝：没有写 WAL，
// Nest 在 Guard 内整体回滚，调用方拿到的错误同时满足 errors.Is(err, nest.ErrCommitRejected)。
//
// 这是可重试错误，不是业务拒绝。屏障在该步骤投影成功，或因租约失效被跳过且内存实体已驱逐后解除；
// 正常只持续一次投影（毫秒级），Mongo 变慢或中断时与投影积压同量级。原因：被跳过的步骤已经改了
// 内存，若允许后续事务以该内存为基础准入，它们投影时必然是版本冲突。
// 历史：RR-20260926-30。
var ErrFencedEntityPending = errors.New("dataengine: entity has a lease-fenced step awaiting projection; retry later")

// LeaseFence 指向的协调文档字段名，以及允许 fenced 事务生效的唯一状态。
//
// 投影器查询该文档、租约持有方写该文档，二者在不同包，必须共用这组常量。
// 任一侧单独改名会让 fence 永远匹配不上；匹配失败与租约正常过期无法区分，
// 都会被当作“跳过的事务”确认，既无错误也无指标。
const (
	LeaseFenceFieldOwner      = "owner"
	LeaseFenceFieldToken      = "lease_token"
	LeaseFenceFieldDigest     = "digest"
	LeaseFenceFieldStatus     = "status"
	LeaseFenceFieldLeaseUntil = "lease_until"
	// LeaseFenceFieldUpdatedAt 是投影事务确认租约时写入的字段。确认必须是一次写：
	// 只读的校验与另一事务里的租约接管互不冲突，到期附近两边都会提交。
	LeaseFenceFieldUpdatedAt = "updated_at"

	LeaseFenceStatusPending = "pending"
)

// LeaseFence 要求投影 WAL 记录时，指定的 Mongo 协调文档仍属于同一 owner/token。
// fence 失效时整条记录作为幂等空操作确认，不是致命的投影冲突。
type LeaseFence struct {
	Database   string `json:"database"`
	Resource   string `json:"resource"`
	DocumentID string `json:"document_id"`
	Owner      string `json:"owner"`
	Token      uint64 `json:"token"`
	Digest     []byte `json:"digest"`
}

func (fence LeaseFence) Validate() error {
	if strings.TrimSpace(fence.Database) == "" || strings.TrimSpace(fence.Resource) == "" || strings.TrimSpace(fence.DocumentID) == "" || strings.TrimSpace(fence.Owner) == "" || fence.Token == 0 || len(fence.Digest) != sha256.Size {
		return ErrInvalidLeaseFence
	}
	return nil
}

// Predicate 返回 fence 成立时必须仍存在的文档条件：同 owner、同租约 token、
// 同命令摘要、状态仍为 pending、且租约在 now 时未过期。投影器必须直接使用它，
// 不能自行拼过滤条件（原因见上方字段常量）。
func (fence LeaseFence) Predicate(now time.Time) bson.M {
	return bson.M{
		"_id":                     fence.DocumentID,
		LeaseFenceFieldOwner:      fence.Owner,
		LeaseFenceFieldToken:      fence.Token,
		LeaseFenceFieldDigest:     fence.Digest,
		LeaseFenceFieldStatus:     LeaseFenceStatusPending,
		LeaseFenceFieldLeaseUntil: bson.M{"$gt": now},
	}
}

// Confirmation 返回投影器在应用 fenced 记录的同一 Mongo 事务里，对 Predicate
// 命中文档执行的更新。正是这次写让投影与并发的租约接管串行化：两个事务写同一
// 文档不能同时提交，要么接管胜出（投影重试后不再匹配而跳过），要么投影胜出
// （接管重试后看到回执）。
func (fence LeaseFence) Confirmation(now time.Time) bson.M {
	return bson.M{"$set": bson.M{LeaseFenceFieldUpdatedAt: now}}
}

// NewLeaseFenceReceipt 把 fence 编码成保留命名空间下的 WAL 回执，ID 为
// database/resource/document_id，Digest 为 payload 的 SHA-256。
func NewLeaseFenceReceipt(fence LeaseFence) (Receipt, error) {
	if err := fence.Validate(); err != nil {
		return Receipt{}, err
	}
	payload, err := json.Marshal(fence)
	if err != nil {
		return Receipt{}, err
	}
	digest := sha256.Sum256(payload)
	return Receipt{Namespace: LeaseFenceReceiptNamespace, ID: fence.Database + "/" + fence.Resource + "/" + fence.DocumentID, Digest: digest[:], Payload: payload}, nil
}

// DecodeLeaseFenceReceipt 解析 lease fence 回执。第二个返回值表示该回执是否属于
// fence 保留命名空间；属于但内容、ID 或摘要不一致时返回 ErrInvalidLeaseFence。
func DecodeLeaseFenceReceipt(receipt Receipt) (LeaseFence, bool, error) {
	if receipt.Namespace != LeaseFenceReceiptNamespace {
		return LeaseFence{}, false, nil
	}
	var fence LeaseFence
	if receipt.ID == "" || len(receipt.Payload) == 0 || json.Unmarshal(receipt.Payload, &fence) != nil {
		return LeaseFence{}, true, ErrInvalidLeaseFence
	}
	if err := fence.Validate(); err != nil {
		return LeaseFence{}, true, err
	}
	expectedID := fence.Database + "/" + fence.Resource + "/" + fence.DocumentID
	expectedDigest := sha256.Sum256(receipt.Payload)
	if receipt.ID != expectedID || !bytes.Equal(receipt.Digest, expectedDigest[:]) {
		return LeaseFence{}, true, ErrInvalidLeaseFence
	}
	return fence, true, nil
}
