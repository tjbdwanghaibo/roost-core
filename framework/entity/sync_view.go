package entity

import (
	"cmp"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
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
		if strings.ContainsAny(view.Profile.Key, syncLayerReserved) {
			// 并集身份由框架拼出（UnionSyncProfiles），业务声明的层级不能和它混淆。
			return nil, fmt.Errorf("entity: sync view key %q uses a reserved character (%q)", view.Profile.Key, syncLayerReserved)
		}
		if _, exists := set.views[view.Profile]; exists {
			return nil, fmt.Errorf("entity: duplicate sync view %+v", view.Profile)
		}
		set.views[view.Profile] = view
	}
	return set, nil
}

// Lookup 返回视图的字段白名单与优先级。并集身份（UnionSyncProfiles）解析为各层级字段的并集、优先级取其中最优（最小）的；
// 任一层级未声明就是未知视图，绝不悄悄放宽。
func (s *SyncViewSet) Lookup(profile SyncProfile) (SyncView, bool) {
	if s == nil {
		return SyncView{}, false
	}
	profile = profile.Normalize()
	layers := profile.Layers()
	if len(layers) == 1 {
		view, ok := s.views[profile]
		return view, ok
	}
	union := SyncView{Profile: profile}
	for index, layer := range layers {
		view, ok := s.views[layer]
		if !ok {
			return SyncView{}, false
		}
		union.Fields |= view.Fields
		if index == 0 || view.Priority < union.Priority {
			union.Priority = view.Priority
		}
	}
	return union, true
}

// Covers 报告层级 a 的字段是否包含层级 b 的全部字段（两者都须已声明）。同一目标被两个层级看见时，
// 包含关系成立就只订阅数据多的那个，不成立才取并集。
func (s *SyncViewSet) Covers(a, b SyncProfile) bool {
	va, okA := s.Lookup(a)
	vb, okB := s.Lookup(b)
	return okA && okB && va.Fields&vb.Fields == vb.Fields
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

// ---- 层级并集 ----
//
// 层级（layer）就是一个 SyncProfile：视野给出层级，层级的字段由 SyncViewSet 声明（NamedSyncView 按 DAO 字段名列出）。
// 同一目标被几个彼此不包含的层级同时看见时，订阅的是它们的并集。并集仍是一个 SyncProfile，Manager、帧缓存与线上格式
// 都不用认识第二种身份：Key 是各层级身份按 CompareSyncProfiles 排序后用 "+" 连接（层级 LOD、SchemaVersion 非零时
// 写成 key@lod 或 key@lod@schema），LOD 取各层级里最小（最细）的。客户端按 "+" 拆开就知道这份数据由哪些层级组成。

// syncLayerReserved 是并集身份占用的字符，业务声明的视图 Key 不能含有。
const syncLayerReserved = "+@"

// UnionSyncProfiles 返回几个层级的并集身份：去重后只剩一个层级时就是它本身，空输入是缺省视图。
func UnionSyncProfiles(profiles ...SyncProfile) SyncProfile {
	var layers []SyncProfile
	for _, profile := range profiles {
		layers = append(layers, profile.Normalize().Layers()...)
	}
	layers = NormalizeSyncProfiles(layers)
	switch len(layers) {
	case 0:
		return SyncProfile{}.Normalize()
	case 1:
		return layers[0]
	}
	tokens := make([]string, len(layers))
	lod := layers[0].LOD
	for index, layer := range layers {
		tokens[index] = layerToken(layer)
		lod = min(lod, layer.LOD)
	}
	return SyncProfile{Key: strings.Join(tokens, "+"), LOD: lod}
}

// Layers 把并集身份拆回各层级（已规范化、有序）；普通视图返回它自己。
func (p SyncProfile) Layers() []SyncProfile {
	p = p.Normalize()
	if !strings.Contains(p.Key, "+") {
		return []SyncProfile{p}
	}
	tokens := strings.Split(p.Key, "+")
	layers := make([]SyncProfile, 0, len(tokens))
	for _, token := range tokens {
		layer, ok := parseLayerToken(token)
		if !ok {
			// 不是框架拼出的并集（业务视图集不接受这种 Key）：按一个普通身份处理，查找时自然是未知视图。
			return []SyncProfile{p}
		}
		layers = append(layers, layer)
	}
	return layers
}

func layerToken(layer SyncProfile) string {
	switch {
	case layer.SchemaVersion != 0:
		return layer.Key + "@" + strconv.Itoa(int(layer.LOD)) + "@" + strconv.FormatUint(uint64(layer.SchemaVersion), 10)
	case layer.LOD != 0:
		return layer.Key + "@" + strconv.Itoa(int(layer.LOD))
	default:
		return layer.Key
	}
}

func parseLayerToken(token string) (SyncProfile, bool) {
	parts := strings.Split(token, "@")
	if parts[0] == "" || len(parts) > 3 {
		return SyncProfile{}, false
	}
	layer := SyncProfile{Key: parts[0]}
	if len(parts) > 1 {
		lod, err := strconv.ParseUint(parts[1], 10, 8)
		if err != nil {
			return SyncProfile{}, false
		}
		layer.LOD = uint8(lod)
	}
	if len(parts) > 2 {
		schema, err := strconv.ParseUint(parts[2], 10, 32)
		if err != nil {
			return SyncProfile{}, false
		}
		layer.SchemaVersion = uint32(schema)
	}
	return layer, true
}
