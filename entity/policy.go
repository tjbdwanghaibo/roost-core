package entity

import "fmt"

// RemotePolicy describes who owns an entity kind's write path across servers.
//
// Managed means the remote_entity module may load, lock, save and sync the
// entity through IThreadSafeRemoteEntity, behind a distributed ownership guard.
// Mirror means this process keeps a local read-only replica of state some other
// service owns. Both are remote-capable, which is what puts the remote bit in
// the ID so a process holding only an ID knows the entity may live elsewhere.
//
// There used to be a third value, Capable, meaning "remote-addressable but not
// managed here". Its only effect was to put the kind in the first lock rank,
// and lock order now comes from the kind's category, so the value said nothing
// the category does not (M-04).
type RemotePolicy uint8

const (
	RemotePolicyNone RemotePolicy = iota
	RemotePolicyManaged
	RemotePolicyMirror
)

// RemoteCapable reports whether the kind is remote-addressable at all, which is
// what the ID's remote bit records.
func (p RemotePolicy) RemoteCapable() bool {
	switch p {
	case RemotePolicyManaged, RemotePolicyMirror:
		return true
	default:
		return false
	}
}

func (p RemotePolicy) RemoteManaged() bool {
	return p == RemotePolicyManaged
}

// EntityLifetime describes the memory/persistence lifecycle expected by the
// framework. It is declarative; persistence is still controlled by AutoPersist
// and registered save/load definitions.
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
