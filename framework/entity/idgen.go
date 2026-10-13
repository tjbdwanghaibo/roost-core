package entity

import (
	"errors"
	"fmt"
	"runtime"
	"sync/atomic"
)

// IDGen 生成 EntityID 中的动态唯一号部分。
//
// 布局：
//
//	unique 52 位 = [blockNo][offset]
//
// offset 默认 20 位，每取得一个块提供 1,048,576 个本地 ID。块号应全局单调分配，例如 Redis INCR 减一或 etcd 事务。
// 块 0 从 unique 1 开始，早期 ID 因此紧凑；unique 0 保留为所有完整 EntityID 的无效 / 空值。52 位唯一号的上半区
// 保留给显式的静态 ID。
const (
	IDGenOffsetBits = 20
	IDGenBlockSize  = uint64(1) << IDGenOffsetBits

	UniqueIDBits        = 52
	UniqueIDMask        = (uint64(1) << UniqueIDBits) - 1
	StaticUniqueIDBase  = uint64(1) << (UniqueIDBits - 1)
	DynamicUniqueIDMask = StaticUniqueIDBase - 1
	MaxBlockNo          = DynamicUniqueIDMask >> IDGenOffsetBits
)

// EntityID 在正 int64 中的布局：
//
//	[sign(1)=0][remote(1)][unique(52)][kind(8)][category(2)]
//
// category 两位只在本进程未注册该 kind 时由 ResolveEntityID 作为分类的兜底读取，见 makeEntityID。
const (
	EntityCategoryBits = 2
	EntityKindBits     = 8
	EntityRemoteBits   = 1
	EntityIDTagBits    = EntityCategoryBits + EntityKindBits

	EntityCategoryMask = (uint64(1) << EntityCategoryBits) - 1
	EntityKindMask     = (uint64(1) << EntityKindBits) - 1
	EntityRemoteMask   = (uint64(1) << EntityRemoteBits) - 1

	EntityKindShift    = EntityCategoryBits
	UniqueIDShift      = EntityIDTagBits
	EntityRemoteShift  = UniqueIDShift + UniqueIDBits
	EntityIDValueBits  = UniqueIDBits + EntityIDTagBits + EntityRemoteBits // 63 bits, positive int64
	EntityIDValueMask  = (uint64(1) << EntityIDValueBits) - 1
	MaxEntityKindValue = EntityKindMask
)

// ID 生成与 EntityID 解析的错误。
var (
	ErrInvalidBlockNo    = errors.New("idgen: block number exceeds 52-bit unique range")
	ErrBlockExhausted    = errors.New("idgen: current block exhausted and no block allocator configured")
	ErrNoBlockSource     = errors.New("idgen: no initial block or block allocator configured")
	ErrInvalidCategory   = errors.New("entity id: category exceeds 2-bit limit")
	ErrInvalidEntityKind = errors.New("entity id: kind exceeds 8-bit limit")
	ErrInvalidEntityID   = errors.New("entity id: invalid")
)

// IDGen 是快路径无锁的唯一号生成器，只在当前块用尽时调用 acquireBlock 取新块（同一时刻只有一个 goroutine 取块，
// 其余自旋让出等待）。
type IDGen struct {
	next         atomic.Uint64
	end          atomic.Uint64
	rotating     atomic.Int32
	acquireBlock atomic.Value // stores func() (uint64, error)
}

// NewIDGen 以 initialBlock 为当前块创建生成器；acquireBlock 为 nil 时块用尽返回 ErrBlockExhausted。
func NewIDGen(initialBlock uint64, acquireBlock func() (uint64, error)) (*IDGen, error) {
	if initialBlock > MaxBlockNo {
		return nil, ErrInvalidBlockNo
	}
	gen := &IDGen{}
	if acquireBlock != nil {
		gen.acquireBlock.Store(acquireBlock)
	}
	gen.resetBlock(initialBlock)
	return gen, nil
}

// Generate 返回下一个唯一号，当前块用尽时先取新块。
func (g *IDGen) Generate() (uint64, error) {
	for {
		next := g.next.Load()
		end := g.end.Load()
		if next < end {
			if g.next.CompareAndSwap(next, next+1) {
				return next, nil
			}
			continue
		}

		if err := g.rotateBlock(); err != nil {
			return 0, err
		}
	}
}

// rotateBlock 取一个新块：抢到轮换权的 goroutine 调用 acquireBlock 并重置块，其余等它完成后重试。
func (g *IDGen) rotateBlock() error {
	fn, _ := g.acquireBlock.Load().(func() (uint64, error))
	if fn == nil {
		return ErrBlockExhausted
	}

	if g.rotating.CompareAndSwap(0, 1) {
		defer g.rotating.Store(0)
		blockNo, err := fn()
		if err != nil {
			return err
		}
		if blockNo > MaxBlockNo {
			return ErrInvalidBlockNo
		}
		g.resetBlock(blockNo)
		return nil
	}

	for g.rotating.Load() != 0 {
		runtime.Gosched()
	}
	return nil
}

// resetBlock 把当前块设为 blockNo（块 0 从 1 开始，跳过保留的 0）。
func (g *IDGen) resetBlock(blockNo uint64) {
	start := blockNo << IDGenOffsetBits
	if blockNo == 0 {
		start = 1
	}
	g.next.Store(start)
	g.end.Store((blockNo << IDGenOffsetBits) + IDGenBlockSize)
}

// ParseUniqueID 把唯一号拆成块号与块内偏移。
func ParseUniqueID(id uint64) (blockNo uint64, offset uint64) {
	id &= UniqueIDMask
	return id >> IDGenOffsetBits, id & (IDGenBlockSize - 1)
}

// BuildEntityID 由唯一号与已注册的 kind 构造完整 EntityID（kind 为 Remote 可用时置 Remote 位）。
func BuildEntityID(uniqueID int64, kind EntityKind) (int64, error) {
	category, err := ResolveEntityKindCategory(kind)
	if err != nil {
		return 0, err
	}
	return buildEntityIDWithCategory(uniqueID, category, kind)
}

// buildEntityIDWithCategory 校验唯一号、分类与 kind 后构造完整 EntityID。
func buildEntityIDWithCategory(uniqueID int64, category EntityCategory, kind EntityKind) (int64, error) {
	if uniqueID <= 0 || uint64(uniqueID) > UniqueIDMask {
		return 0, fmt.Errorf("%w: unique id %d outside 1..%d", ErrInvalidEntityID, uniqueID, UniqueIDMask)
	}
	if category == EntityCategoryNone {
		return 0, fmt.Errorf("%w: category is none", ErrInvalidEntityID)
	}
	if kind == EntityKindNone {
		return 0, fmt.Errorf("%w: kind is none", ErrInvalidEntityID)
	}
	// makeEntityID 按 kind 注册的 Remote 能力自行置位，这里不必另算。
	return makeEntityID(uniqueID, category, kind, false), nil
}

// makeEntityID 拼出完整 EntityID。remoteCapable 为 true 或 kind 注册为 Remote 可用时置 Remote 位。
func makeEntityID(uniqueID int64, category EntityCategory, kind EntityKind, remoteCapable bool) int64 {
	// kind 是 uint8，总在 EntityKindMask（8 位）之内，无需检查。
	//
	// 低两位仍写入截断后的分类。已注册 kind 的分类以注册表为准（ResolveEntityID），只有本进程未注册该 kind 时
	// 才退回读取这两位。保留写入使 ID 的位布局不变、已存储的 ID 不需要迁移；分类超过该字段三个可用值时写入的是
	// 无意义的余数。去掉这次写入会改变 ID 格式，须单独决定。
	id := ((uint64(uniqueID) & UniqueIDMask) << UniqueIDShift) |
		((uint64(kind) & EntityKindMask) << EntityKindShift) |
		(uint64(category) & EntityCategoryMask)
	if kind != EntityKindNone && IsEntityKindRemoteCapable(kind) {
		remoteCapable = true
	}
	if remoteCapable {
		id |= EntityRemoteMask << EntityRemoteShift
	}
	return int64(id)
}

// GetEntityCategoryFromID 读取 ID 低两位的分类字段。
//
// Deprecated: kind 的分类以注册表为准，该字段只有三个可用值。请使用 ResolveEntityID(id).Category，或对 kind 使用
// EntityCategoryOfKind。只有持有本进程未链接 kind 的 ID、别无他法时才用它。
func GetEntityCategoryFromID(id int64) EntityCategory {
	return EntityCategory(uint64(id) & EntityCategoryMask)
}

// GetEntityKindFromID 读取 ID 中的 kind。
func GetEntityKindFromID(id int64) EntityKind {
	return EntityKind((uint64(id) >> EntityKindShift) & EntityKindMask)
}

// GetEntityRemoteFromID 读取 ID 中的 Remote 位。
func GetEntityRemoteFromID(id int64) bool {
	return ((uint64(id) >> EntityRemoteShift) & EntityRemoteMask) != 0
}

// GetUniqueIDFromEntityID 读取 ID 中的唯一号。
func GetUniqueIDFromEntityID(id int64) int64 {
	return int64((uint64(id) >> UniqueIDShift) & UniqueIDMask)
}

// NormalizeFullID 校验 id 是完整、可解析的 EntityID（kind 非 None 时还须与之相符），返回规范后的完整 ID。
// kind 必须已在本进程注册。
func NormalizeFullID(id int64, kind EntityKind) (int64, error) {
	if id == 0 {
		return 0, fmt.Errorf("%w: empty id", ErrInvalidEntityID)
	}
	meta := ResolveEntityID(id)
	if kind != EntityKindNone && meta.Kind != kind {
		return 0, fmt.Errorf("%w: id %d kind=%d want=%d", ErrInvalidEntityID, id, meta.Kind, kind)
	}
	if meta.Category == EntityCategoryNone {
		return 0, fmt.Errorf("%w: id %d has empty category", ErrInvalidEntityID, id)
	}
	if meta.Kind == EntityKindNone {
		return 0, fmt.Errorf("%w: id %d has empty kind", ErrInvalidEntityID, id)
	}
	if meta.UniqueID == 0 {
		return 0, fmt.Errorf("%w: id %d has empty unique id", ErrInvalidEntityID, id)
	}
	// kind 必须在本进程注册，ID 才可解析。不再把 meta.Category 与注册表比较：ResolveEntityID 的分类就取自注册表，
	// 两者必然一致，比较证明不了任何东西。
	if _, err := ResolveEntityKindCategory(meta.Kind); err != nil {
		return 0, err
	}
	return meta.FullID, nil
}

// MatchEntityID 报告 id 是否是 kind（None 表示任意）的合法完整 EntityID。
func MatchEntityID(id int64, kind EntityKind) bool {
	_, err := NormalizeFullID(id, kind)
	return err == nil
}

// IsRemoteCapableEntityID 报告 EntityID 是否具有 Remote 能力。有 kind 注册时以注册为准；Remote 位是只持有 ID 的
// 进程在传输 / 存储上的提示。
func IsRemoteCapableEntityID(id int64) bool {
	kind := GetEntityKindFromID(id)
	if kind != EntityKindNone && IsEntityKindRemoteCapable(kind) {
		return true
	}
	return GetEntityRemoteFromID(id)
}

// setRemoteCapableBit 置 ID 的 Remote 位。
func setRemoteCapableBit(id int64) int64 {
	return int64(uint64(id) | (EntityRemoteMask << EntityRemoteShift))
}

// clearRemoteCapableBit 清 ID 的 Remote 位（只供包内测试）。
func clearRemoteCapableBit(id int64) int64 {
	return int64(uint64(id) & ^(EntityRemoteMask << EntityRemoteShift))
}
