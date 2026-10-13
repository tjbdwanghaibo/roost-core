package entity

import "fmt"

// RemotePolicy 描述一个实体 kind 的跨服写路径归谁所有。
//
// Managed 表示 remote_entity 模块可以经 IThreadSafeRemoteEntity、在分布式所有权保护下加载、加锁、保存与同步该实体。
// Mirror 表示本进程保存另一服务所拥有状态的本地只读副本。两者都是 Remote 可用的，这会在 ID 中置 Remote 位，让只持有
// ID 的进程知道该实体可能在别处。锁序由 kind 的分类决定，与策略无关。
type RemotePolicy uint8

const (
	RemotePolicyNone RemotePolicy = iota
	RemotePolicyManaged
	RemotePolicyMirror
)

// RemoteCapable 报告该策略是否 Remote 可寻址，即 ID 的 Remote 位记录的内容。
func (p RemotePolicy) RemoteCapable() bool {
	switch p {
	case RemotePolicyManaged, RemotePolicyMirror:
		return true
	default:
		return false
	}
}

// RemoteManaged 报告该策略是否为 Remote 托管。
func (p RemotePolicy) RemoteManaged() bool {
	return p == RemotePolicyManaged
}

// EntityLifetime 描述框架期望的内存 / 持久化生命周期。它是声明性的；是否持久化仍由 AutoPersist 与登记的保存 / 加载
// 定义控制。
type EntityLifetime uint8

const (
	EntityLifetimeDefault EntityLifetime = iota
	EntityLifetimeEphemeral
	EntityLifetimeRuntimeRebuild
	EntityLifetimePersistedHotCold
	EntityLifetimeResident
	EntityLifetimeRemoteManaged
	EntityLifetimeMirrorCache
)

// DefaultEntityLifetime 按 Remote 策略与是否持久化给出默认生命周期。
func DefaultEntityLifetime(noPersist bool, remotePolicy RemotePolicy) EntityLifetime {
	if remotePolicy == RemotePolicyManaged {
		return EntityLifetimeRemoteManaged
	}
	if remotePolicy == RemotePolicyMirror {
		return EntityLifetimeMirrorCache
	}
	if noPersist {
		return EntityLifetimeEphemeral
	}
	return EntityLifetimePersistedHotCold
}

// ValidateEntityPolicy 校验生命周期与 Remote 策略、持久化设置一致（lifetime 为 Default 时先取默认值）。
func ValidateEntityPolicy(kind EntityKind, noPersist bool, remotePolicy RemotePolicy, lifetime EntityLifetime) error {
	if lifetime == EntityLifetimeDefault {
		lifetime = DefaultEntityLifetime(noPersist, remotePolicy)
	}
	if remotePolicy == RemotePolicyManaged && lifetime != EntityLifetimeRemoteManaged {
		return fmt.Errorf("entity kind %d remote=managed requires remote_managed lifetime, got %d", kind, lifetime)
	}
	if remotePolicy == RemotePolicyMirror && lifetime != EntityLifetimeMirrorCache {
		return fmt.Errorf("entity kind %d remote=mirror requires mirror_cache lifetime, got %d", kind, lifetime)
	}
	if remotePolicy != RemotePolicyManaged && lifetime == EntityLifetimeRemoteManaged {
		return fmt.Errorf("entity kind %d remote_managed lifetime requires remote=managed", kind)
	}
	if remotePolicy != RemotePolicyMirror && lifetime == EntityLifetimeMirrorCache {
		return fmt.Errorf("entity kind %d mirror_cache lifetime requires remote=mirror", kind)
	}
	if noPersist {
		switch lifetime {
		case EntityLifetimePersistedHotCold, EntityLifetimeResident:
			return fmt.Errorf("entity kind %d noPersist=true conflicts with lifetime %d", kind, lifetime)
		}
	}
	return nil
}
