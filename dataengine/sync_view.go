package dataengine

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// NewSyncViewSet 将生成 DAO 的字段名配置解析为 entity 的不可变视图集合。
// 构建期字段位可变，业务声明仍按名称绑定；未知或歧义字段在启动时拒绝。
func NewSyncViewSet(fields []SyncFieldMeta, definitions ...entity.NamedSyncView) (*entity.SyncViewSet, error) {
	names := make(map[string]uint64, len(fields)*2)
	var all uint64
	for _, field := range fields {
		if field.Bit == 0 || field.Bit&(field.Bit-1) != 0 || all&field.Bit != 0 {
			return nil, fmt.Errorf("dataengine: invalid sync field bit for %q", field.Name)
		}
		all |= field.Bit
		for _, name := range []string{field.Name, field.WireName} {
			if name == "" || name == "*" {
				return nil, fmt.Errorf("dataengine: invalid sync field name %q", name)
			}
			if bit, exists := names[name]; exists && bit != field.Bit {
				return nil, fmt.Errorf("dataengine: ambiguous sync field %q", name)
			}
			names[name] = field.Bit
		}
	}
	views := make([]entity.SyncView, 0, len(definitions))
	for _, definition := range definitions {
		view := entity.SyncView{Profile: definition.Profile, Priority: definition.Priority}
		for _, name := range definition.Fields {
			if name == "*" {
				view.Fields |= all
				continue
			}
			bit, exists := names[name]
			if !exists {
				return nil, fmt.Errorf("dataengine: unknown sync field %q in view %q", name, definition.Profile.Key)
			}
			view.Fields |= bit
		}
		views = append(views, view)
	}
	return entity.NewSyncViewSet(views...)
}
