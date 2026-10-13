package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

const StartEffectTopic = "saga.start"
const CompletionEffectTopicPrefix = "saga.result."
const StepReceiptNamespace = "saga-step"
const WireVersion uint16 = 1

type startEffectPayload struct {
	Version uint16       `json:"version"`
	Start   StartRequest `json:"start"`
}

type completionEffectPayload struct {
	Version    uint16     `json:"version"`
	Completion Completion `json:"completion"`
}

// maxStartWireDataBytes 是启动意图 Data 在线上的硬上限（与 Options.MaxPayloadBytes 的上界相同）。
const maxStartWireDataBytes = 4 << 20

// startDataLimits 记本进程里注册了某个 saga 类型的协调器接受的 StartRequest.Data 上限（Options.MaxPayloadBytes），
// 由 Engine.Register 写入、NewStartEffect 读取。同一类型被多个 Engine 注册时取最小值：本进程任何一个
// 协调器会拒绝的 Data 都不放进 WAL。条目数以 saga 类型数为界。
//
// 为什么是按类型的进程表而不是 Engine 方法：EmitStart 在 Nest handler 里调用，handler 拿不到 Engine（生成的
// <saga>.EmitStart 也是包级函数）；而按类型查表只是一次内存读，不做 I/O、不等待，可以留在业务池的 Nest 事务里。
var startDataLimits struct {
	sync.RWMutex
	byType map[string]int
}

func registerStartDataLimit(sagaType string, limit int) {
	startDataLimits.Lock()
	defer startDataLimits.Unlock()
	if startDataLimits.byType == nil {
		startDataLimits.byType = make(map[string]int)
	}
	if current, ok := startDataLimits.byType[sagaType]; !ok || limit < current {
		startDataLimits.byType[sagaType] = limit
	}
}

// configuredStartDataLimit 是本进程配置的 saga.max_payload_bytes：发起方与协调器共用这个配置键，
// kit/nest 的 Mod 在 Init 里写入（每个调 EmitStart 的进程都有 Nest），协调器不在本进程时 EmitStart 也按它拒绝。0 表示没配置。
var configuredStartDataLimit atomic.Int64

// SetStartDataLimit 设置本进程 EmitStart 的 Data 上限（saga.max_payload_bytes，发起方与协调器共用的配置键）。
// limit ≤ 0 清除配置，只剩本进程协调器注册的上限与线上硬上限。
func SetStartDataLimit(limit int) {
	configuredStartDataLimit.Store(int64(max(limit, 0)))
}

// startDataLimit 返回这个类型的启动 Data 上限：线上硬上限、配置的 saga.max_payload_bytes、本进程协调器
// 为它注册的 MaxPayloadBytes三者取小。都没有时只有线上硬上限（未经 kit 装配的进程；意图若被协调器拒绝，
// 由启动消费者 Term 并告警）。
func startDataLimit(sagaType string) int {
	limit := maxStartWireDataBytes
	if configured := int(configuredStartDataLimit.Load()); configured > 0 {
		limit = min(limit, configured)
	}
	startDataLimits.RLock()
	defer startDataLimits.RUnlock()
	if registered, ok := startDataLimits.byType[sagaType]; ok {
		limit = min(limit, registered)
	}
	return limit
}

// NewStartEffect 生成稳定的 Nest 事务性 outbox 启动意图；外层 handler 的实体修改与这份意图共用一条 WAL 记录。
//
// Data 的上限是 startDataLimit（配置的 saga.max_payload_bytes 与本进程协调器注册的 MaxPayloadBytes 取小）：协调器必定
// 拒绝的意图在 Nest 事务里就以 ErrInvalidRecord 拒绝，handler 拿到错误，实体修改随事务回滚，而不是随事务提交后被
// 协调器拒绝、启动消费者 nak 到 MaxDeliver、saga 静默不创建。
func NewStartEffect(request StartRequest) (nest.Effect, error) {
	request.Type = strings.TrimSpace(request.Type)
	request.BusinessKey = strings.TrimSpace(request.BusinessKey)
	request.ID = strings.TrimSpace(request.ID)
	if request.Type == "" || len(request.Type) > 128 || request.DefinitionVersion == 0 || request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > startDataLimit(request.Type) || (request.ID != "" && !validSubjectToken(request.ID, 128)) {
		return nest.Effect{}, ErrInvalidRecord
	}
	// Now 是进程内的测试 / 恢复钩子，不属于持久的业务意图；初始调度时间由投递时间决定。
	request.Now = time.Time{}
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	payload, err := json.Marshal(startEffectPayload{Version: WireVersion, Start: request})
	if err != nil {
		return nest.Effect{}, err
	}
	// 带上完整的规范化意图：精确的 WAL 重放保持同一 ID，而用同一业务键配不同载荷的编程错误不会被 JetStream 的
	// MsgID 去重掩盖。
	digest := sha256.Sum256(payload)
	return nest.Effect{ID: "saga-start:" + hex.EncodeToString(digest[:16]), Topic: StartEffectTopic, Key: request.BusinessKey, Payload: payload, Headers: map[string]string{"saga-type": request.Type}}, nil
}

// EmitStart 把 saga 启动意图追加到当前 Nest 事务。
func EmitStart(request StartRequest) error {
	effect, err := NewStartEffect(request)
	if err != nil {
		return err
	}
	return nest.Emit(effect)
}

// BindCommand 把命令身份纳入当前 Nest 事务。调用方给出已校验的保留截止时间；TTL 的选择归 saga 策略，不归本函数。
func BindCommand(command Command, expiresAt time.Time, fences ...dataengine.LeaseFence) error {
	tx := nest.CurrentRollbackTx()
	if tx == nil {
		return nest.ErrTransactionClosed
	}
	if expiresAt.IsZero() || command.Validate() != nil {
		return ErrInvalidRecord
	}
	raw, err := json.Marshal(command)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(raw)
	for _, fence := range fences {
		receipt, err := dataengine.NewLeaseFenceReceipt(fence)
		if err != nil {
			return err
		}
		if err := tx.AddReceipt(receipt); err != nil {
			return err
		}
	}
	return tx.AddReceipt(dataengine.Receipt{
		Namespace: StepReceiptNamespace, ID: command.ID, Digest: digest[:], ExpiresAt: expiresAt.UnixNano(),
	})
}

// NewCompletionEffect 把 completion 编成发往所属 saga 结果主题的 Nest effect，effect ID 由 CommandID 派生。
func NewCompletionEffect(completion Completion) (nest.Effect, error) {
	if completion.CompletedAt.IsZero() {
		completion.CompletedAt = time.Now().UTC()
	}
	if err := completion.Validate(); err != nil {
		return nest.Effect{}, err
	}
	payload, err := json.Marshal(completionEffectPayload{Version: WireVersion, Completion: completion})
	if err != nil {
		return nest.Effect{}, err
	}
	return nest.Effect{
		ID: "saga-completion:" + completion.CommandID, Topic: CompletionEffectTopicPrefix + completion.SagaID,
		Key: completion.IdempotencyKey, Payload: payload, Headers: map[string]string{"saga-id": completion.SagaID},
	}, nil
}

// EmitCompletion 把可回放的 completion 附到已绑定的回执上，并把它的消息投递意图追加到同一条 Nest CommitRecord。
func EmitCompletion(completion Completion) error {
	tx := nest.CurrentRollbackTx()
	if tx == nil {
		return nest.ErrTransactionClosed
	}
	effect, err := NewCompletionEffect(completion)
	if err != nil {
		return err
	}
	if err := tx.SetReceiptPayload(StepReceiptNamespace, completion.CommandID, effect.Payload); err != nil {
		return fmt.Errorf("saga: bind completion receipt: %w", err)
	}
	return tx.Emit(effect)
}

// DecodeStartEffect 解码并校验启动 effect 的载荷；版本或内容不合法返回 ErrInvalidRecord。
func DecodeStartEffect(payload []byte) (StartRequest, error) {
	var envelope startEffectPayload
	if len(payload) == 0 {
		return StartRequest{}, ErrInvalidRecord
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return StartRequest{}, err
	}
	if envelope.Version != WireVersion {
		return StartRequest{}, ErrInvalidRecord
	}
	request := envelope.Start
	request.Type = strings.TrimSpace(request.Type)
	request.BusinessKey = strings.TrimSpace(request.BusinessKey)
	request.ID = strings.TrimSpace(request.ID)
	request.Now = time.Time{}
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	if request.Type == "" || len(request.Type) > 128 || request.DefinitionVersion == 0 || request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > maxStartWireDataBytes || (request.ID != "" && !validSubjectToken(request.ID, 128)) {
		return StartRequest{}, ErrInvalidRecord
	}
	return request, nil
}

// DecodeCompletionEffect 解码并校验 completion effect 的载荷；版本或内容不合法返回 ErrInvalidRecord。
func DecodeCompletionEffect(payload []byte) (Completion, error) {
	var envelope completionEffectPayload
	if len(payload) == 0 || json.Unmarshal(payload, &envelope) != nil || envelope.Version != WireVersion {
		return Completion{}, ErrInvalidRecord
	}
	if err := envelope.Completion.Validate(); err != nil {
		return Completion{}, err
	}
	return envelope.Completion, nil
}
