package entity

import (
	"errors"
	"fmt"
	"time"
)

// ErrRemoteSnapshotStale 表示快照低于要求的最低版本。
var ErrRemoteSnapshotStale = errors.New("remote snapshot stale")

// RemoteAcquireMode 描述不可变快照的读取方式。Remote 写入是 Nest 的派发目标，总是走受保护的 RemoteWriteBatch 路径。
type RemoteAcquireMode uint8

const (
	RemoteAcquireReadOnly RemoteAcquireMode = iota + 1
	RemoteAcquireCache
)

// RemoteReadOption 配置一次读取。它有意不保存在业务对象里；长期使用方只保存 RemoteViewRef。
type RemoteReadOption struct {
	MinVersion     uint64
	AllowStale     bool
	CacheTTLMillis int64
	NowMillis      int64
}

// NormalizeRemoteReadOption 把负的 TTL 与当前时间归零。
func NormalizeRemoteReadOption(option RemoteReadOption) RemoteReadOption {
	if option.CacheTTLMillis < 0 {
		option.CacheTTLMillis = 0
	}
	if option.NowMillis < 0 {
		option.NowMillis = 0
	}
	return option
}

// Accepts 报告 version 是否满足最低版本要求（允许陈旧或未要求时总是满足）。
func (o RemoteReadOption) Accepts(version int64) bool {
	if o.AllowStale || o.MinVersion == 0 {
		return true
	}
	return version >= 0 && uint64(version) >= o.MinVersion
}

// RemoteSnapshotSource 是快照的来源。
type RemoteSnapshotSource uint8

const (
	RemoteSnapshotSourceUnknown RemoteSnapshotSource = iota
	RemoteSnapshotSourceLocal
	RemoteSnapshotSourceLoaded
	RemoteSnapshotSourceCache
)

func (s RemoteSnapshotSource) String() string {
	switch s {
	case RemoteSnapshotSourceLocal:
		return "local"
	case RemoteSnapshotSourceLoaded:
		return "loaded"
	case RemoteSnapshotSourceCache:
		return "cache"
	default:
		return "unknown"
	}
}

// RemoteSnapshot 是 Remote 只读 / 缓存路径的不可变读取结果。它有意是值语义：只读 Remote 访问不能交给调用方可变的实体、
// 组件或 DAO 指针。
type RemoteSnapshot struct {
	EntityID   int64
	Kind       EntityKind
	Scope      uint64
	Version    uint64
	RouteEpoch uint64
	Source     RemoteSnapshotSource
	ReadAt     int64
	ExpiresAt  int64
	Data       any
}

// Accepts 报告快照是否未过期且满足最低版本要求。
func (s RemoteSnapshot) Accepts(option RemoteReadOption) bool {
	if s.expiredByOption(option) {
		return false
	}
	return option.AllowStale || option.MinVersion == 0 || s.Version >= option.MinVersion
}

// Expired 报告快照在 now（毫秒）时是否已过期。
func (s RemoteSnapshot) Expired(now int64) bool {
	return s.ExpiresAt > 0 && now >= s.ExpiresAt
}

func (s RemoteSnapshot) expiredByOption(option RemoteReadOption) bool {
	if option.NowMillis <= 0 {
		return false
	}
	return s.Expired(option.NowMillis)
}

// AsString 返回字符串形式的 Data；不是字符串时为空。
func (s RemoteSnapshot) AsString() string {
	if v, ok := s.Data.(string); ok {
		return v
	}
	return ""
}

// RemoteSnapshotRequest 是一次按 scope 读取快照的请求。
type RemoteSnapshotRequest struct {
	Scope      uint64
	RouteEpoch uint64
	Option     RemoteReadOption
}

// RemoteSnapshotProvider 按 scope 提供快照视图。
type RemoteSnapshotProvider interface {
	RemoteSnapshot(scope uint64) (any, bool)
}

// RemoteSnapshotResolveRequest 是按引用解析快照的请求。
type RemoteSnapshotResolveRequest struct {
	Ref        RemoteViewRef
	Mode       RemoteAcquireMode
	Scope      uint64
	RouteEpoch uint64
	Option     RemoteReadOption
}

// RemoteViewRef 是对服务端读模型的通用持久引用：说明消费的是哪个 Remote 实体、观察到的是哪个版本。
type RemoteViewRef struct {
	EntityID   int64
	Kind       EntityKind
	Version    uint64
	RouteEpoch uint64
}

// Valid 报告引用的 ID 是完整、可解析且 kind 相符的 EntityID。
func (r RemoteViewRef) Valid() bool {
	if r.EntityID == 0 || r.Kind == EntityKindNone {
		return false
	}
	meta := ResolveEntityID(r.EntityID)
	return meta.FullID == r.EntityID && meta.Kind == r.Kind
}

// UniqueID 返回引用实体的唯一号；引用无效时为 0。
func (r RemoteViewRef) UniqueID() int64 {
	if !r.Valid() {
		return 0
	}
	return ResolveEntityID(r.EntityID).UniqueID
}

// NewRemoteViewRef 构造并校验引用。
func NewRemoteViewRef(entityID int64, kind EntityKind, version uint64, routeEpoch uint64) (RemoteViewRef, error) {
	ref := RemoteViewRef{EntityID: entityID, Kind: kind, Version: version, RouteEpoch: routeEpoch}
	if !ref.Valid() {
		return RemoteViewRef{}, fmt.Errorf("remote view ref: invalid entity=%d kind=%d", entityID, kind)
	}
	return ref, nil
}

// RemoteSnapshotReadAt 返回读取时刻（毫秒）：选项给出的当前时间，否则取系统时间。
func RemoteSnapshotReadAt(option RemoteReadOption) int64 {
	option = NormalizeRemoteReadOption(option)
	if option.NowMillis > 0 {
		return option.NowMillis
	}
	return time.Now().UnixMilli()
}

// RemoteSnapshotExpiresAt 返回按缓存 TTL 计算的过期时刻（毫秒）；未设 TTL 时为 0（不过期）。
func RemoteSnapshotExpiresAt(readAt int64, option RemoteReadOption) int64 {
	option = NormalizeRemoteReadOption(option)
	if option.CacheTTLMillis <= 0 {
		return 0
	}
	if readAt <= 0 {
		readAt = RemoteSnapshotReadAt(option)
	}
	return readAt + option.CacheTTLMillis
}
