package policy

import (
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

func (in *Interest) profileFor(source string, band int) entity.SyncProfile {
	if profile, ok := in.sourceProfiles[source][band]; ok {
		return profile
	}
	return in.profile(band).Normalize()
}

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

// 空间 band 有限，可在启动前检查 fallback。关系 band 由业务动态输入，
// 已声明映射先检查，其他动态值在 Subscribe 前检查，绝不悄悄放宽视图。
func (in *Interest) validateConfiguredProfiles(config InterestConfig) error {
	if len(in.viewSets) == 0 {
		return nil
	}
	check := func(source string, band int) error {
		if err := in.validateProfile(in.profileFor(source, band)); err != nil {
			return fmt.Errorf("policy: source %q band %d: %w", source, band, err)
		}
		return nil
	}
	for band := 0; band <= len(config.AOI.Bands); band++ {
		if err := check(SourceSpatial, band); err != nil {
			return err
		}
	}
	if in.self != nil {
		if err := check(SourceSelf, 0); err != nil {
			return err
		}
	}
	for source, bands := range in.sourceProfiles {
		for band := range bands {
			if err := check(source, band); err != nil {
				return err
			}
		}
	}
	for _, source := range config.Relations {
		if err := check(source, 0); err != nil {
			return err
		}
	}
	return nil
}
