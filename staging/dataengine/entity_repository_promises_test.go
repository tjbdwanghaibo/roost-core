package dataengine

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/dataengine"
	"github.com/tjbdwanghaibo/roost-core/entity"
)

const (
	dataEngineDuplicateDAOKind entity.EntityKind = 241
	dataEngineNilBuilderKind   entity.EntityKind = 242
)

var registerCorruptBuilderKinds sync.Once

func ensureCorruptBuilderKinds() {
	registerCorruptBuilderKinds.Do(func() {
		build := func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &dataEngineRepositoryEntity{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
		}
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineDuplicateDAOKind,
			DaoBuilders: []entity.DaoBuilderFunc{
				func() entity.DaoInterface { return &dataEngineRepositoryDAO{collection: "repository_dup"} },
				func() entity.DaoInterface { return &dataEngineRepositoryDAO{collection: "repository_dup"} },
			},
			Builder: build,
		})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineNilBuilderKind,
			DaoBuilders: []entity.DaoBuilderFunc{nil},
			Builder:     build,
		})
	})
}

func expectCorrupt(t *testing.T, err error, text string) {
	t.Helper()
	if !errors.Is(err, ErrEntityAggregateCorrupt) || !strings.Contains(err.Error(), text) {
		t.Fatalf("LoadEntity = %v, want ErrEntityAggregateCorrupt containing %q", err, text)
	}
}

// Loading an aggregate is where a bad store answer would become a live
// entity. Each shape of bad answer is refused with the resource named, and
// nothing is published to the manager: two documents for one id, a document
// keyed to another entity, a builder that declares the same resource twice,
// a nil DAO builder, and a remote-managed entity persisted without its
// version envelope.
func TestEntityRepositoryRefusesEachCorruptAggregateShape(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	ensureDataEngineRemoteRepositoryEntity()
	ensureCorruptBuilderKinds()
	ctx := context.Background()

	t.Run("two documents for one id", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1201, dataEngineRepositoryKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{
			"repository_profile":   {repositoryRaw(t, "repository_profile", id, 1), repositoryRaw(t, "repository_profile", id, 2)},
			"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
		}}
		manager := entity.NewEntityManager()
		repository, _ := newEntityRepository(manager, store, nil, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		expectCorrupt(t, err, "resource=repository_profile")
		expectCorrupt(t, err, "documents=2")
		if manager.Get(id) != nil {
			t.Fatal("corrupt aggregate was published")
		}
	})
	t.Run("document keyed to another entity", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1202, dataEngineRepositoryKind)
		other, _ := entity.BuildEntityID(1203, dataEngineRepositoryKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{
			"repository_profile":   {repositoryRaw(t, "repository_profile", other, 1)},
			"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
		}}
		repository, _ := newEntityRepository(entity.NewEntityManager(), store, nil, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineRepositoryKind)
		expectCorrupt(t, err, "documents=1")
	})
	t.Run("builder declares one resource twice", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1204, dataEngineDuplicateDAOKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{"repository_dup": {repositoryRaw(t, "repository_dup", id, 1)}}}
		repository, _ := newEntityRepository(entity.NewEntityManager(), store, nil, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineDuplicateDAOKind)
		expectCorrupt(t, err, `duplicate DAO resource "repository_dup"`)
	})
	t.Run("nil DAO builder", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1205, dataEngineNilBuilderKind)
		repository, _ := newEntityRepository(entity.NewEntityManager(), &repositoryStore{docs: map[string][]coredata.RawDocument{}}, nil, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineNilBuilderKind)
		expectCorrupt(t, err, "DAO builder 0 is nil")
	})
	t.Run("remote-managed entity without a version envelope", func(t *testing.T) {
		id, _ := entity.BuildEntityID(1206, dataEngineRemoteRepositoryKind)
		store := &repositoryStore{docs: map[string][]coredata.RawDocument{"repository_remote": {repositoryRaw(t, "repository_remote", id, 3)}}}
		repository, _ := newEntityRepository(entity.NewEntityManager(), store, nil, repositoryGate(true))
		_, err := repository.LoadEntity(ctx, id, dataEngineRemoteRepositoryKind)
		expectCorrupt(t, err, "has no version envelope")
	})
}
