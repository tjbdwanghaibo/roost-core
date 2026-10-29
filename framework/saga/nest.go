package saga

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/nest"
)

// nest.go 是 saga 在 Nest 一侧的入口：启动意图作为 Nest 事务性 effect，与业务的实体修改同一条 WAL 记录；协调器进程的启动中继
// （SubscribeNestStarts）把它转发到分区。原生步骤的结果不经 effect（参与方在 WAL 落盘后直接发回分区，step_consumer.go）。

// StartEffectTopic 是启动意图 effect 的主题（DataEngine 发布到 <dataengine.effects 前缀>.saga.start）。
const StartEffectTopic = "saga.start"

type startEffectPayload struct {
	Version uint16       `json:"version"`
	Start   StartRequest `json:"start"`
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
	if request.Type == "" || len(request.Type) > 128 || request.DefinitionVersion == 0 || request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > startDataLimit(request.Type) {
		return nest.Effect{}, ErrInvalidRecord
	}
	// Now 是进程内的测试 / 恢复钩子，不属于持久的业务意图；初始调度时间由投递时间决定。
	request.Now = time.Time{}
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	payload, err := json.Marshal(startEffectPayload{Version: WireVersion, Start: request})
	if err != nil {
		return nest.Effect{}, err
	}
	// effect ID 由完整的规范化意图派生：精确的 WAL 重放保持同一 ID，而用同一业务键配不同载荷的编程错误不会被 JetStream 的
	// MsgID 去重掩盖（分区所有者按启动摘要判定为身份冲突）。
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
	request.Now = time.Time{}
	request.DeadlineAt = canonicalDeadline(request.DeadlineAt)
	if request.Type == "" || len(request.Type) > 128 || request.DefinitionVersion == 0 || request.BusinessKey == "" || len(request.BusinessKey) > 512 || len(request.Data) > maxStartWireDataBytes {
		return StartRequest{}, ErrInvalidRecord
	}
	return request, nil
}
