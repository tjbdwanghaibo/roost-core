package entity

import (
	"context"
	"errors"
	"fmt"
)

// Mirror 只读契约（PLAN-REMOTE-POLICY-MIRROR 第 1 步，docs/feature/MIRROR-STEPS-1-3-2026-10-06.md）。
//
// 只读方（另一个服务读 owner 发布的摘要）拿到的唯一能力是“按完整 key 读不可变快照”：
// RemoteSnapshotReadOnly。它没有写、提交、发布或所有权能力；读结果带观察 token（RemoteObservation），
// 业务可以把 token 带给 owner 的命令，或作为下一次读的最低要求。
//
// 只读方不注册 entity kind、不注册全局快照解码器：owner 与 consumer 在同一进程、同一身份时，身份只由
// owner 注册一次，consumer 用 RemoteMirrorReader 表达“我要读它”，所以两者不冲突。
//
// 迁移：旧的 remote=mirror 声明只是元数据（不订阅、不加载、不强制只读），把它当成可写 Entity 使用是
// 方案明确不支持的用法；新代码用 RemoteMirrorReader 读 DTO。生成器对 remote=mirror 报迁移错误，只读 DTO
// 用 //roost:mirror 标记生成 spec / 解码 / reader（Mirror 第 5 步，docs/feature/MIRROR-STEP-5-2026-10-06.md）。

var (
	// ErrRemoteObservationIncomparable 表示快照与最低观察 token 的 epoch 一个更新、一个更旧（混合），
	// 两者没有先后。和 L1 / L2 的准入规则一致：混合 epoch 两个方向都拒绝。调用方应重新读取并以新的
	// token 为准，不能把“数字更大”当成更新。
	ErrRemoteObservationIncomparable = errors.New("remote snapshot: observation token is not comparable with the snapshot's epochs")
	// ErrRemoteReadUnsupported 表示这个读者不提供请求的一致性（Linearizable 只在权威 loader 声明了
	// 线性化能力时开放）。不静默退化成较弱的读。
	ErrRemoteReadUnsupported = errors.New("remote snapshot: read consistency not supported by this reader")
)

// RemoteObservation 是观察 token：一次读看到的权威代际与内容版本。版本比较全用整数。
//
// 排序规则与快照准入（remoteSnapshotStale、L2 CAS 脚本）相同：MarkerEpoch 与 RouteEpoch 都不旧且至少一个
// 更新的快照比 token 新，不论版本；epoch 相同时比版本；epoch 一新一旧没有先后。
// 两个 epoch 都为零的 token 只约束版本（与旧的 minVersion 参数等价）。
type RemoteObservation struct {
	MarkerEpoch  uint64
	RouteEpoch   uint64
	StateVersion uint64
}

// Observation 返回快照的观察 token。
func (s RemoteSnapshotEnvelope) Observation() RemoteObservation {
	return RemoteObservation{MarkerEpoch: s.MarkerEpoch, RouteEpoch: s.RouteEpoch, StateVersion: s.StateVersion}
}

// IsZero 报告 token 不带任何约束。
func (o RemoteObservation) IsZero() bool { return o == RemoteObservation{} }

// versionOnly 报告 token 只约束版本（旧的 minVersion）。
func (o RemoteObservation) versionOnly() bool { return o.MarkerEpoch == 0 && o.RouteEpoch == 0 }

// Covers 报告在 o 观察到的快照是否不早于 min。epoch 一新一旧时返回 ErrRemoteObservationIncomparable。
// 这是所有读出口共用的最低要求判定（Cached / Monotonic / Linearizable / 直接加载）。
func (o RemoteObservation) Covers(min RemoteObservation) (bool, error) {
	if min.versionOnly() {
		return o.StateVersion >= min.StateVersion, nil
	}
	switch {
	case o.MarkerEpoch == min.MarkerEpoch && o.RouteEpoch == min.RouteEpoch:
		return o.StateVersion >= min.StateVersion, nil
	case o.MarkerEpoch >= min.MarkerEpoch && o.RouteEpoch >= min.RouteEpoch:
		return true, nil
	case o.MarkerEpoch <= min.MarkerEpoch && o.RouteEpoch <= min.RouteEpoch:
		return false, nil
	default:
		return false, fmt.Errorf("%w: snapshot marker=%d route=%d, token marker=%d route=%d",
			ErrRemoteObservationIncomparable, o.MarkerEpoch, o.RouteEpoch, min.MarkerEpoch, min.RouteEpoch)
	}
}

// RemoteSnapshotRead 是一次只读请求。
type RemoteSnapshotRead struct {
	Key RemoteSnapshotKey
	// Consistency 零值按 RemoteReadCached。
	Consistency RemoteReadConsistency
	// After 是最低观察 token；零值不限。Cached 读不因它回源：不满足就是未找到（Cached 只读缓存）。
	After RemoteObservation
}

// RemoteSnapshotReadOnly 是只读方的唯一能力。实现（remoteentity.SnapshotClient、Manager）保证返回的
// 快照满足：完整 key 与请求一致；未过自身 ExpiresAt；非线性读在陈旧上限内被共享 L2 或权威确认过；
// 满足 After。字节是冻结的，调用方只能读副本。
type RemoteSnapshotReadOnly interface {
	ReadSnapshot(context.Context, RemoteSnapshotRead) (RemoteSnapshotEnvelope, bool, error)
}

// RemoteMirrorSpec 是一个只读视图的身份：完整 key 去掉 EntityID 的部分，加上期望的 schema / codec。
type RemoteMirrorSpec struct {
	Tenant uint32
	Kind   EntityKind
	Scope  uint32
	Policy uint32
	// Schema 必须非零；读到别的 schema 返回 ErrRemoteSnapshotSchemaMismatch，不交给解码器。
	Schema uint32
	Codec  uint16
}

// RemoteMirrorValue 是一次 DTO 读的结果。Value 由独立的字节副本解码，修改它不影响缓存。
type RemoteMirrorValue[T any] struct {
	Value       T
	Observation RemoteObservation
	// ExpiresAt 是快照自身的有效期（Unix 纳秒，0 表示不限），透传自 envelope。
	ExpiresAt int64
}

// RemoteMirrorReader 把只读能力和一个解码函数绑成按 EntityID 读 DTO 的 reader。它不注册任何进程级
// 表（kind、解码器），同一进程可以同时是同一身份的 owner。
type RemoteMirrorReader[T any] struct {
	source RemoteSnapshotReadOnly
	spec   RemoteMirrorSpec
	decode func([]byte) (T, error)
}

// NewRemoteMirrorReader 校验 spec 并返回 reader。decode 拿到的是字节副本，可以直接保留它。
func NewRemoteMirrorReader[T any](source RemoteSnapshotReadOnly, spec RemoteMirrorSpec, decode func([]byte) (T, error)) (*RemoteMirrorReader[T], error) {
	if source == nil || decode == nil {
		return nil, fmt.Errorf("remote mirror: source and decode are required")
	}
	if spec.Kind == EntityKindNone || spec.Schema == 0 {
		return nil, fmt.Errorf("remote mirror: spec needs a kind and a schema")
	}
	return &RemoteMirrorReader[T]{source: source, spec: spec, decode: decode}, nil
}

// Key 返回 entityID 在这个视图下的完整快照 key。
func (r *RemoteMirrorReader[T]) Key(entityID int64) RemoteSnapshotKey {
	return RemoteSnapshotKey{Tenant: r.spec.Tenant, EntityID: entityID, Kind: r.spec.Kind, Scope: r.spec.Scope, Policy: r.spec.Policy}
}

// Read 读 entityID 的 DTO。未找到返回 found=false；身份、schema、codec 不符或解码失败返回错误。
func (r *RemoteMirrorReader[T]) Read(ctx context.Context, entityID int64, consistency RemoteReadConsistency, after RemoteObservation) (RemoteMirrorValue[T], bool, error) {
	key := r.Key(entityID)
	if !key.Valid() {
		return RemoteMirrorValue[T]{}, false, fmt.Errorf("remote mirror: entity %d is not a kind %d id", entityID, r.spec.Kind)
	}
	snapshot, found, err := r.source.ReadSnapshot(ctx, RemoteSnapshotRead{Key: key, Consistency: consistency, After: after})
	if err != nil || !found {
		return RemoteMirrorValue[T]{}, false, err
	}
	if snapshot.Key != key {
		return RemoteMirrorValue[T]{}, false, fmt.Errorf("remote mirror: read returned a snapshot for another key")
	}
	if snapshot.Schema != r.spec.Schema || snapshot.Codec != r.spec.Codec {
		return RemoteMirrorValue[T]{}, false, fmt.Errorf("%w: got schema=%d codec=%d, view expects schema=%d codec=%d",
			ErrRemoteSnapshotSchemaMismatch, snapshot.Schema, snapshot.Codec, r.spec.Schema, r.spec.Codec)
	}
	value, err := r.decode(snapshot.Payload.BytesCopy())
	if err != nil {
		return RemoteMirrorValue[T]{}, false, err
	}
	return RemoteMirrorValue[T]{Value: value, Observation: snapshot.Observation(), ExpiresAt: snapshot.ExpiresAt}, true, nil
}
