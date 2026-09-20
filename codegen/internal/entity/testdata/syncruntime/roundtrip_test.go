//go:build entitysyncruntime

package syncruntime

// The entity generator's runtime gate. roost-codegen has no runtime
// dependency, so `go test ./internal/entity` can only compare generated text
// — which is how the sync wiring drifted away from Core's API without a
// single test noticing: the generated file named a struct field and a
// constant that Core does not have (RR-20260918-01). This file is compiled by
// scripts/entity-sync-runtime.sh in a throwaway module against the pinned
// roost-core, so "the generated entity builds" and "the generated entity
// actually gets its Sync state" are both checked.

import (
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

// The generated package-level registration is what a project calls once at
// startup; without it no builder exists and BuildEntity refuses.
func TestMain(m *testing.M) {
	RegisterEntity()
	m.Run()
}

func TestSyncEntityBuildsAndCarriesItsSyncState(t *testing.T) {
	id, err := entity.BuildEntityID(7, EntityKindAvatar)
	if err != nil {
		t.Fatalf("build id: %v", err)
	}
	built, err := entity.BuildEntity(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindAvatar, Id: id})
	if err != nil {
		t.Fatalf("build entity: %v", err)
	}
	// The generated wiring must reach Core's Sync state; compiling is not
	// enough, because a builder that silently drops the parameter also
	// compiles.
	base, ok := built.(interface{ Sync() *entity.SubjectSyncState })
	if !ok {
		t.Fatalf("built entity %T exposes no Sync()", built)
	}
	state := base.Sync()
	if state == nil {
		t.Fatal("sync=true entity was built without a sync state")
	}
	if !state.Enabled() {
		t.Fatal("the sync state is present but disabled")
	}
	// The packer the marker named must be the one that got installed. Mark
	// the subject dirty and prepare: with a packer this produces an update,
	// and with a nil one Core answers ErrSubjectSyncPacker — which is
	// exactly what a generated builder that dropped the factory would give.
	state.MarkFullDirty(1)
	if !state.PendingDirty() {
		t.Fatal("marking the subject dirty left nothing pending")
	}
	prepared, err := state.Prepare(nil)
	if err != nil {
		t.Fatalf("prepare through the generated packer: %v", err)
	}
	if prepared == nil {
		t.Fatal("prepare produced nothing for a dirty subject")
	}
}

// The control: sync=false builds the same shape with no sync state, so a
// project that never asked for replication pays nothing.
func TestPlainEntityHasNoSyncState(t *testing.T) {
	id, err := entity.BuildEntityID(9, EntityKindPlain)
	if err != nil {
		t.Fatalf("build id: %v", err)
	}
	built, err := entity.BuildEntity(&entity.EntityCreateParam{IsCreate: true, Kind: EntityKindPlain, Id: id})
	if err != nil {
		t.Fatalf("build entity: %v", err)
	}
	base, ok := built.(interface{ Sync() *entity.SubjectSyncState })
	if !ok {
		t.Fatalf("built entity %T exposes no Sync()", built)
	}
	if state := base.Sync(); state != nil && state.Enabled() {
		t.Fatal("sync=false entity came out with replication enabled")
	}
}
