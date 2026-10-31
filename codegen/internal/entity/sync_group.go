package entity

import (
	"fmt"
	"path/filepath"
)

// InspectSyncGroup 是 access.player.sync_groups 对一个实体的前置检查（roost generate / doctor 共用）：dir 包里有名为
// name 的实体，且标记打开了 Entity Sync（sync=true）——按组推送的就是这个实体的同步视图。拒绝时错误里写明改法。
func InspectSyncGroup(dir, name string) error {
	defs, _, err := parsePackage(dir)
	if err != nil {
		return err
	}
	for _, ent := range defs.Entities {
		if ent.Name != name {
			continue
		}
		if !ent.Sync {
			return fmt.Errorf(`%s: access.player.sync_groups sends %s to its group members through Entity Sync, but its //roost:entity marker does not say sync=true; add sync=true, syncNamespace=<name> and subjectPacker=<a packer whose view set accepts the SyncGroup view key>`,
				filepath.ToSlash(ent.SourceFile), ent.Name)
		}
		return nil
	}
	return fmt.Errorf("%s: no //roost:entity struct named %s", filepath.ToSlash(dir), name)
}
