package entity

import (
	"errors"
	"fmt"
	"sync"
)

// ErrRemoteSnapshotsDisabled 表示读的 kind 声明了不写、不发布快照（//roost:entity snapshots=none）：它没有 Mirror
// 也没有 RemoteViewRef 可读，读它是装配错误，不是“暂时没有”。
var ErrRemoteSnapshotsDisabled = errors.New("remote snapshot: the entity kind publishes no snapshots")

// remoteSnapshotsOff 是声明了 snapshots=none 的 Remote 托管 kind。生成的 register<Entity>Entity 在登记 builder 的
// 同一个 once 里调用 DisableRemoteSnapshots；只在本进程登记过该 kind 时生效（只读进程没登记它，靠生成器在同一工程
// 里拒绝指向它的 //roost:mirror）。
var remoteSnapshotsOff struct {
	sync.RWMutex
	kinds map[EntityKind]struct{}
}

// DisableRemoteSnapshots 登记 kind 不写、不发布快照：它的提交只有 DAO 写入（owner 冷加载、接管、迁移都读 DAO
// 集合与 _remote_entity_meta，不读快照），没有 _remote_entity_snapshots 文档、L2 条目与兴趣广播。由生成代码调用。
func DisableRemoteSnapshots(kind EntityKind) {
	remoteSnapshotsOff.Lock()
	defer remoteSnapshotsOff.Unlock()
	if remoteSnapshotsOff.kinds == nil {
		remoteSnapshotsOff.kinds = make(map[EntityKind]struct{})
	}
	remoteSnapshotsOff.kinds[kind] = struct{}{}
}

// RemoteSnapshotsDisabled 报告 kind 是否在本进程登记为不发布快照。
func RemoteSnapshotsDisabled(kind EntityKind) bool {
	remoteSnapshotsOff.RLock()
	defer remoteSnapshotsOff.RUnlock()
	_, off := remoteSnapshotsOff.kinds[kind]
	return off
}

// CheckRemoteSnapshotsEnabled 是读快照的入口检查（Mirror 读者构造、Nest 的 RemoteViewRef 预取）。
func CheckRemoteSnapshotsEnabled(kind EntityKind) error {
	if RemoteSnapshotsDisabled(kind) {
		return fmt.Errorf("%w: kind %d", ErrRemoteSnapshotsDisabled, kind)
	}
	return nil
}
