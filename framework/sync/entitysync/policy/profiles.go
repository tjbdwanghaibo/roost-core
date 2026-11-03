package policy

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

// validateProfile 检查一个视图（层级或层级并集）在每个配置的实体类型里都已声明，且优先级与 Manager 一致。
func (in *Interest) validateProfile(profile entity.SyncProfile) error {
	for name, views := range in.viewSets {
		view, ok := views.Lookup(profile)
		if !ok {
			return fmt.Errorf("%w: entity type %q, profile %+v", entity.ErrSyncViewUnknown, name, profile)
		}
		if err := in.validatePriority(view); err != nil {
			return fmt.Errorf("policy: entity type %q: %w", name, err)
		}
	}
	return nil
}

// validateConfiguredLayers 在启动时检查已知层级都能映射到授权视图：空间视野每个 LOD 层的每个距离档（有限，可以全查）、
// self 与额外块覆盖（跟随各层 0 档时已随距离档查过）。关系视野的层级在 DefineRelationView 时检查，层级并集在
// Subscribe 前检查，绝不悄悄放宽视图。
func (in *Interest) validateConfiguredLayers() error {
	if len(in.viewSets) == 0 {
		return nil
	}
	check := func(what string, layer entity.SyncProfile) error {
		if err := in.validateProfile(layer); err != nil {
			return fmt.Errorf("policy: %s layer: %w", what, err)
		}
		return nil
	}
	for level := range in.aoi.config.levels() {
		bands, _ := in.aoi.config.lodBands(level)
		for band := 0; band <= len(bands); band++ {
			if err := check(fmt.Sprintf("spatial LOD %d band %d", level, band), in.layerFor(level, band)); err != nil {
				return err
			}
		}
	}
	if in.selfVisible {
		if err := check("self", in.selfLayer); err != nil {
			return err
		}
	}
	if in.blockFollows {
		return nil
	}
	return check("block", in.blockLayer)
}
