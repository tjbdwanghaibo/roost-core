package entity

import (
	"errors"
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

// RemoteSnapshotReadAt 返回读取时刻（毫秒）：选项给出的当前时间，否则取系统时间。
func RemoteSnapshotReadAt(option RemoteReadOption) int64 {
	option = NormalizeRemoteReadOption(option)
	if option.NowMillis > 0 {
		return option.NowMillis
	}
	return time.Now().UnixMilli()
}
