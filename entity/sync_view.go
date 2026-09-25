package entity

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
)

// SyncView 把视图身份、字段白名单和选择优先级分开。Fields 使用业务 DAO 的字段位；
// 0 表示无字段，全部字段应显式使用全位掩码。Priority 越小越优先，不表示权限继承。
type SyncView struct {
	Profile  SyncProfile
	Fields   uint64
	Priority int
}

// SyncViewSet 是构造后不可变的有限视图配置，可由同类型实体的 packer 共享。
// 业务必须先授权订阅；这里的字段白名单不替代订阅权限检查。
type SyncViewSet struct{ views map[SyncProfile]SyncView }

var ErrSyncViewUnknown = errors.New("entity: unknown sync view")

// NamedSyncView 用生成 DAO 的 Go 字段名或 wire name 配置，不保存会随 schema 改变的 bit。
// "*" 显式选择当前 schema 的全部同步字段；空列表表示无字段。
type NamedSyncView struct {
	Profile  SyncProfile
	Fields   []string
	Priority int
}

func NewSyncViewSet(views ...SyncView) (*SyncViewSet, error) {
	if len(views) == 0 {
		return nil, errors.New("entity: sync views are required")
	}
	set := &SyncViewSet{views: make(map[SyncProfile]SyncView, len(views))}
	for _, view := range views {
		view.Profile = view.Profile.Normalize()
		if _, exists := set.views[view.Profile]; exists {
			return nil, fmt.Errorf("entity: duplicate sync view %+v", view.Profile)
		}
		set.views[view.Profile] = view
	}
	return set, nil
}

func (s *SyncViewSet) Lookup(profile SyncProfile) (SyncView, bool) {
	if s == nil {
		return SyncView{}, false
	}
	view, ok := s.views[profile.Normalize()]
	return view, ok
}

// Priorities 返回独立副本，供 Manager 配置使用；修改它不会改变 packer 的字段白名单。
func (s *SyncViewSet) Priorities() map[SyncProfile]int {
	out := make(map[SyncProfile]int)
	if s != nil {
		for profile, view := range s.views {
			out[profile] = view.Priority
		}
	}
	return out
}

// MergeSyncViewPriorities 合并实体类型的优先级；同一视图身份必须使用同一优先级。
func MergeSyncViewPriorities(sets ...*SyncViewSet) (map[SyncProfile]int, error) {
	priorities := make(map[SyncProfile]int)
	for _, set := range sets {
		if set == nil {
			return nil, errors.New("entity: nil sync view set")
		}
		for profile, view := range set.views {
			if old, ok := priorities[profile]; ok && old != view.Priority {
				return nil, fmt.Errorf("entity: conflicting priority for %+v: %d and %d", profile, old, view.Priority)
			}
			priorities[profile] = view.Priority
		}
	}
	return priorities, nil
}

// NewMaskedSyncPacker 适配按字段掩码编码的业务 DAO。回调在实体锁内执行，
// 返回新建且转移所有权的字节。snapshot 使用完整白名单，delta 使用 dirty∩白名单。
// 未知视图报错，绝不自动回退到更宽视图。空 delta 仍由框架发送版本推进信息。
func NewMaskedSyncPacker(views *SyncViewSet, codec uint16, marshal func(uint64) ([]byte, error)) (SubjectSyncPacker, error) {
	if views == nil || len(views.views) == 0 || marshal == nil || codec == 0 {
		return nil, errors.New("entity: views, codec and marshal are required")
	}
	pack := func(profile SyncProfile, mask uint64) (FrozenSyncPayload, error) {
		view, ok := views.Lookup(profile)
		if !ok {
			return FrozenSyncPayload{}, fmt.Errorf("%w: %+v", ErrSyncViewUnknown, profile)
		}
		selected := view.Fields & mask
		if selected == 0 {
			return TakeFrozenSyncPayload(codec, nil), nil
		}
		raw, err := marshal(selected)
		if err != nil {
			return FrozenSyncPayload{}, err
		}
		return TakeFrozenSyncPayload(codec, raw), nil
	}
	return SubjectSyncPackFunc{
		Snapshot: func(p SyncProfile) (FrozenSyncPayload, error) { return pack(p, ^uint64(0)) },
		Delta:    pack,
	}, nil
}

// CompareSyncProfiles 只定义规范化后的稳定身份顺序；业务优先级由 Manager 单独决定。
func CompareSyncProfiles(a, b SyncProfile) int {
	a, b = a.Normalize(), b.Normalize()
	if order := cmp.Compare(a.Key, b.Key); order != 0 {
		return order
	}
	if order := cmp.Compare(a.LOD, b.LOD); order != 0 {
		return order
	}
	return cmp.Compare(a.SchemaVersion, b.SchemaVersion)
}

// NormalizeSyncProfiles 保持空列表为空，不修改调用方切片。有限视图先排序再去重，
// 单视图不需要临时 map；内容层和调度层共享同一种身份规范化规则。
func NormalizeSyncProfiles(profiles []SyncProfile) []SyncProfile {
	if len(profiles) == 0 {
		return nil
	}
	out := make([]SyncProfile, len(profiles))
	for i, profile := range profiles {
		out[i] = profile.Normalize()
	}
	slices.SortFunc(out, CompareSyncProfiles)
	return slices.Compact(out)
}
