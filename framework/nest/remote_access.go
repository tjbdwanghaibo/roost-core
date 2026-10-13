package nest

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
	"github.com/tjbdwanghaibo/roost-core/infra/base/fctx"
)

// RemoteAcquireMode 是 RemoteAccess 未指定一致性时的读取方式（只读或允许缓存）。
type RemoteAcquireMode = entity.RemoteAcquireMode

const (
	RemoteAcquireReadOnly = entity.RemoteAcquireReadOnly
	RemoteAcquireCache    = entity.RemoteAcquireCache
)

// Remote 快照读取的错误：别名重复、快照缺失、类型不符、身份（实体 / kind / scope / 路由代际）不符。
var (
	ErrRemoteAccessAliasDuplicate = errors.New("nest: duplicate remote access alias")
	ErrRemoteAccessMissing        = errors.New("nest: remote snapshot missing")
	ErrRemoteSnapshotTypeMismatch = errors.New("nest: remote snapshot type mismatch")
	ErrRemoteSnapshotIdentity     = errors.New("nest: remote snapshot identity mismatch")
)

// RemoteAccess 声明 handler 需要的一份 Remote 实体只读快照：I/O 阶段按它读取并校验，handler 内经 Remote(key) 按
// Alias 取用。Required 为 true 时读取失败、身份不符或版本过旧会让消息失败，否则只是不提供该快照。
type RemoteAccess struct {
	Alias          string
	Ref            entity.RemoteViewRef
	Mode           RemoteAcquireMode
	Scope          uint64
	MinVersion     uint64
	AllowStale     bool
	CacheTTLMillis int64
	Required       bool
	Tenant         uint32
	Policy         uint32
	Consistency    entity.RemoteReadConsistency
}

// RemoteAccessProvider 由消息参数实现，声明本消息需要的 Remote 快照。
type RemoteAccessProvider interface {
	RemoteAccess() []RemoteAccess
}

// RemoteSnapshotResolver 是可替换的快照来源；未装配时经 IRemoteEntityManager.ReadRemoteSnapshot 读取。
type RemoteSnapshotResolver interface {
	ResolveRemoteSnapshot(access RemoteAccess) (entity.RemoteSnapshot, error)
}

type remoteSnapshotCtxKey struct{}

// prepareRemoteSnapshots 在派发准备阶段读取消息参数声明的全部 Remote 快照，校验后按别名存入当前请求上下文。
func prepareRemoteSnapshots(msg *Msg, resolver RemoteSnapshotResolver, manager entity.IRemoteEntityManager) error {
	if msg == nil {
		return nil
	}
	accesses := collectRemoteAccess(msg.Params)
	if len(accesses) == 0 {
		return nil
	}
	snapshots := make(map[string]entity.RemoteSnapshot, len(accesses))
	for _, access := range accesses {
		if access.Alias == "" {
			return fmt.Errorf("%w: empty alias", ErrRemoteAccessMissing)
		}
		if _, exists := snapshots[access.Alias]; exists {
			return fmt.Errorf("%w: alias=%s", ErrRemoteAccessAliasDuplicate, access.Alias)
		}
		if !access.Ref.Valid() {
			return fmt.Errorf("nest: remote access %s has invalid ref", access.Alias)
		}
		if access.Consistency == 0 && access.Mode != RemoteAcquireReadOnly && access.Mode != RemoteAcquireCache {
			return fmt.Errorf("nest: remote access %s has invalid read mode %d", access.Alias, access.Mode)
		}
		if access.Consistency > entity.RemoteReadLinearizable {
			return fmt.Errorf("nest: remote access %s has invalid consistency %d", access.Alias, access.Consistency)
		}
		snapshot, err := resolveAcceptedSnapshot(access, resolver, manager)
		if err != nil {
			if access.Required {
				return err
			}
			continue
		}
		snapshots[access.Alias] = snapshot
	}
	if len(snapshots) == 0 {
		return nil
	}
	c := fctx.CurrentContext()
	if c == nil {
		return nil
	}
	c.Set(remoteSnapshotCtxKey{}, snapshots)
	return nil
}

// resolveAcceptedSnapshot 读取 access 声明的快照，并确认它的身份与声明一致、版本满足读取要求。
func resolveAcceptedSnapshot(access RemoteAccess, resolver RemoteSnapshotResolver, manager entity.IRemoteEntityManager) (entity.RemoteSnapshot, error) {
	snapshot, err := resolveRemoteSnapshot(access, resolver, manager)
	if err != nil {
		return entity.RemoteSnapshot{}, fmt.Errorf("nest: remote access %s: %w", access.Alias, err)
	}
	if snapshot.EntityID != access.Ref.EntityID || snapshot.Kind != access.Ref.Kind || snapshot.Scope != access.Scope || snapshot.RouteEpoch != access.Ref.RouteEpoch {
		return entity.RemoteSnapshot{}, fmt.Errorf("%w: alias=%s", ErrRemoteSnapshotIdentity, access.Alias)
	}
	if !snapshot.Accepts(remoteReadOption(access)) {
		return entity.RemoteSnapshot{}, fmt.Errorf("nest: remote access %s stale: version=%d min=%d", access.Alias, snapshot.Version, access.MinVersion)
	}
	return snapshot, nil
}

// resolveRemoteSnapshot 从 resolver（装配时）或 Remote Manager 读取快照。未指定一致性时按读取方式取 Cached 或 Monotonic。
func resolveRemoteSnapshot(access RemoteAccess, resolver RemoteSnapshotResolver, manager entity.IRemoteEntityManager) (entity.RemoteSnapshot, error) {
	if resolver != nil {
		return resolver.ResolveRemoteSnapshot(access)
	}
	consistency := access.Consistency
	if consistency == 0 {
		if access.Mode == RemoteAcquireCache {
			consistency = entity.RemoteReadCached
		} else {
			consistency = entity.RemoteReadMonotonic
		}
	}
	baseCtx := context.Background()
	if current := fctx.CurrentContext(); current != nil && current.Base != nil {
		baseCtx = current.Base
	}
	if manager == nil {
		return entity.RemoteSnapshot{}, fmt.Errorf("nest: Remote Entity snapshot reader is not configured")
	}
	minVersion := access.MinVersion
	if access.AllowStale && consistency == entity.RemoteReadCached {
		// AllowStale 接受低于 MinVersion 的快照（调用方的 RemoteSnapshot.Accepts 判定）。Cached 读出口对不满足
		// 最低版本的 L1 返回 ErrRemoteSnapshotStale，不能把下限交给它，否则 allow_stale 失效。
		minVersion = 0
	}
	envelope, found, err := manager.ReadRemoteSnapshot(baseCtx, entity.RemoteSnapshotKey{
		Tenant: access.Tenant, EntityID: access.Ref.EntityID, Kind: access.Ref.Kind,
		Scope: uint32(access.Scope), Policy: access.Policy,
	}, consistency, minVersion)
	if err != nil {
		return entity.RemoteSnapshot{}, err
	}
	if !found {
		return entity.RemoteSnapshot{}, ErrRemoteAccessMissing
	}
	data, err := entity.DecodeRemoteSnapshot(envelope)
	if err != nil {
		return entity.RemoteSnapshot{}, err
	}
	return entity.RemoteSnapshot{
		EntityID: envelope.Key.EntityID, Kind: envelope.Key.Kind, Scope: uint64(envelope.Key.Scope),
		Version: envelope.StateVersion, RouteEpoch: envelope.RouteEpoch,
		Source: entity.RemoteSnapshotSourceCache, ReadAt: time.Now().UnixMilli(),
		ExpiresAt: envelope.ExpiresAt / int64(time.Millisecond), Data: data,
	}, nil
}

// collectRemoteAccess 收集参数中全部 RemoteAccessProvider 声明的访问。
func collectRemoteAccess(params []any) []RemoteAccess {
	if len(params) == 0 {
		return nil
	}
	var accesses []RemoteAccess
	for _, param := range params {
		if provider, ok := param.(RemoteAccessProvider); ok && provider != nil {
			accesses = append(accesses, provider.RemoteAccess()...)
		}
	}
	return accesses
}

// remoteReadOption 把访问声明换成快照的接受条件（以当前时间判断缓存是否过期）。
func remoteReadOption(access RemoteAccess) entity.RemoteReadOption {
	return entity.RemoteReadOption{
		MinVersion:     access.MinVersion,
		AllowStale:     access.AllowStale,
		CacheTTLMillis: access.CacheTTLMillis,
		NowMillis:      time.Now().UnixMilli(),
	}
}

// RemoteKey 是按别名取用类型为 T 的 Remote 快照的键。
type RemoteKey[T any] struct {
	Alias string
}

// RemoteScopeProvider 由 Remote 视图类型实现，给出它的 scope。
type RemoteScopeProvider interface {
	RemoteScope() uint64
}

// RemoteDefaultTTLMillisProvider 由 Remote 视图类型可选实现，给出默认缓存 TTL（毫秒）。
type RemoteDefaultTTLMillisProvider interface {
	RemoteDefaultTTLMillis() int64
}

// RemoteScopeOf 返回视图类型 T 的 scope。
func RemoteScopeOf[T RemoteScopeProvider]() uint64 {
	var v T
	return v.RemoteScope()
}

// RemoteDefaultTTLMillisOf 返回视图类型 T 的默认缓存 TTL；未实现时为 0。
func RemoteDefaultTTLMillisOf[T any]() int64 {
	var v T
	provider, ok := any(v).(RemoteDefaultTTLMillisProvider)
	if !ok {
		return 0
	}
	return provider.RemoteDefaultTTLMillis()
}

// Remote 在 handler 内取当前消息准备好的 Remote 快照；没有或类型不符时返回 false。
func Remote[T any](key RemoteKey[T]) (T, bool) {
	return remoteSnapshot[T](key.Alias)
}

// MustRemote 是 Remote 的 panic 版本，缺失时以 ErrRemoteAccessMissing panic。
func MustRemote[T any](key RemoteKey[T]) T {
	v, ok := Remote(key)
	if !ok {
		panic(fmt.Errorf("%w: alias=%s", ErrRemoteAccessMissing, key.Alias))
	}
	return v
}

func remoteSnapshot[T any](alias string) (T, bool) {
	var zero T
	c := fctx.CurrentContext()
	if c == nil {
		return zero, false
	}
	raw, ok := c.Get(remoteSnapshotCtxKey{})
	if !ok {
		return zero, false
	}
	snapshots, ok := raw.(map[string]entity.RemoteSnapshot)
	if !ok {
		return zero, false
	}
	snapshot, ok := snapshots[alias]
	if !ok {
		return zero, false
	}
	data, ok := snapshot.Data.(T)
	if !ok {
		return zero, false
	}
	return data, true
}
