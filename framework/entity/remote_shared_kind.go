package entity

import "sync"

// remoteSharedKinds 是声明了 ownership=shared 的 Remote 托管 kind（//roost:entity … remote=managed ownership=shared）。
// 生成的 register<Entity>Entity 在登记 builder 的同一个 once 里调用 DeclareRemoteSharedKind；只在本进程登记过该 kind
// 时生效——认领只发生在会构建这个实体的进程里（写准入要加载实例），这些进程都登记了它。
var remoteSharedKinds struct {
	sync.RWMutex
	kinds map[EntityKind]struct{}
}

// DeclareRemoteSharedKind 登记 kind 的每个实体生来就是共享模式：第一次认领（写准入的首写认领、ClaimRemoteOwnership）
// 一条插入就写成共享的所有权记录，不经过“先独占认领、再切共享”的中间态，任何进程之后都按共享模式（分布式锁 +
// 写许可）读写它；并发首写时插入输了的一方读回共享记录照常准入。这类实体不能退出共享模式（LeaveRemoteSharedMode
// 拒绝）。契约见 docs/framework/REMOTE_ENTITY.md“共享 kind”。由生成代码调用。
func DeclareRemoteSharedKind(kind EntityKind) {
	remoteSharedKinds.Lock()
	defer remoteSharedKinds.Unlock()
	if remoteSharedKinds.kinds == nil {
		remoteSharedKinds.kinds = make(map[EntityKind]struct{})
	}
	remoteSharedKinds.kinds[kind] = struct{}{}
}

// RemoteKindShared 报告 kind 是否在本进程登记为共享 kind（DeclareRemoteSharedKind）。
func RemoteKindShared(kind EntityKind) bool {
	remoteSharedKinds.RLock()
	defer remoteSharedKinds.RUnlock()
	_, shared := remoteSharedKinds.kinds[kind]
	return shared
}
