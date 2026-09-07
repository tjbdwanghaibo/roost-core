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
	dataEngineNoBuilderKind entity.EntityKind = 243
	dataEngineNoPersistKind entity.EntityKind = 244
	dataEngineNoLoaderKind  entity.EntityKind = 245
	dataEngineWrongIDKind   entity.EntityKind = 246
)

// noLoaderDAO shadows RestorePersisted with a different signature so it no
// longer satisfies entity.PersistedDaoLoader.
type noLoaderDAO struct{ dataEngineRepositoryDAO }

func (*noLoaderDAO) RestorePersisted() {}

// wrongIDDAO decodes to a different entity than the one requested.
type wrongIDDAO struct{ dataEngineRepositoryDAO }

func (dao *wrongIDDAO) RestorePersisted(raw []byte, schema uint32, version uint64) error {
	if err := dao.dataEngineRepositoryDAO.RestorePersisted(raw, schema, version); err != nil {
		return err
	}
	dao.id++
	return nil
}

var registerLoadGuardKinds sync.Once

func ensureLoadGuardKinds() {
	registerLoadGuardKinds.Do(func() {
		build := func(param *entity.EntityCreateParam) (entity.IThreadSafeEntity, error) {
			return &dataEngineRepositoryEntity{EntityBase: entity.NewEntityBaseWithMutex(param.Id, param.Category, false, param.Mutex, param.Kind)}, nil
		}
		entity.MustRegisterEntityKindCategory(dataEngineNoBuilderKind, 1)
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{Category: 1, Kind: dataEngineNoPersistKind, NoPersist: true, Builder: build})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineNoLoaderKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &noLoaderDAO{dataEngineRepositoryDAO{collection: "repository_noloader"}}
			}},
			Builder: build,
		})
		entity.RegisterEntityBuilder(&entity.EntityBuilderParam{
			Category: 1, Kind: dataEngineWrongIDKind,
			DaoBuilders: []entity.DaoBuilderFunc{func() entity.DaoInterface {
				return &wrongIDDAO{dataEngineRepositoryDAO{collection: "repository_wrongid"}}
			}},
			Builder: build,
		})
	})
}

// U-0101 (C2): the load path's remaining refusals — a kind the registry does
// not know, a kind with nothing persistent, a DAO that cannot be hydrated, a
// DAO that decodes to another id, and a stored schema the repository has no
// migrator for — each fail by name and publish nothing.
func TestEntityRepositoryRefusesEachUnloadableAggregate(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	ensureLoadGuardKinds()
	ctx := context.Background()

	if _, err := newEntityRepository(nil, &repositoryStore{}, nil, repositoryGate(true)); err == nil {
		t.Fatal("repository without a manager accepted")
	}
	if _, err := newEntityRepository(entity.NewEntityManager(), nil, nil, repositoryGate(true)); err == nil {
		t.Fatal("repository without a store accepted")
	}
	var nilRepository *EntityRepository
	if _, err := nilRepository.LoadEntity(ctx, 1, dataEngineRepositoryKind); !errors.Is(err, coredata.ErrStoreRequired) {
		t.Fatalf("nil repository LoadEntity = %v", err)
	}

	schema2 := func(resource string, id int64) coredata.RawDocument {
		doc := repositoryRaw(t, resource, id, 1)
		doc.Schema = 2
		return doc
	}
	cases := []struct {
		name string
		kind entity.EntityKind
		docs func(id int64) map[string][]coredata.RawDocument
		want error
		text string
	}{
		{"no builder for kind", dataEngineNoBuilderKind, func(int64) map[string][]coredata.RawDocument { return nil }, nil, "no builder for entity kind 243"},
		{"kind without persistent DAO", dataEngineNoPersistKind, func(int64) map[string][]coredata.RawDocument { return nil }, ErrEntityAggregateNotFound, "has no persistent DAO"},
		{"DAO cannot be hydrated", dataEngineNoLoaderKind, func(id int64) map[string][]coredata.RawDocument {
			return map[string][]coredata.RawDocument{"repository_noloader": {repositoryRaw(t, "repository_noloader", id, 1)}}
		}, ErrEntityAggregateCorrupt, "resource=repository_noloader does not implement PersistedDaoLoader"},
		{"DAO decodes to another id", dataEngineWrongIDKind, func(id int64) map[string][]coredata.RawDocument {
			return map[string][]coredata.RawDocument{"repository_wrongid": {repositoryRaw(t, "repository_wrongid", id, 1)}}
		}, ErrEntityAggregateCorrupt, "decoded id="},
		{"stored schema without a migrator", dataEngineRepositoryKind, func(id int64) map[string][]coredata.RawDocument {
			return map[string][]coredata.RawDocument{
				"repository_profile":   {schema2("repository_profile", id)},
				"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 1)},
			}
		}, ErrMigrationUnsupported, "resource=repository_profile"},
	}
	for index, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			id, err := entity.BuildEntityID(int64(1301+index), tc.kind)
			if err != nil {
				t.Fatal(err)
			}
			manager := entity.NewEntityManager()
			repository, err := newEntityRepository(manager, &repositoryStore{docs: tc.docs(id)}, nil, repositoryGate(true))
			if err != nil {
				t.Fatal(err)
			}
			_, err = repository.LoadEntity(ctx, id, tc.kind)
			if err == nil || (tc.want != nil && !errors.Is(err, tc.want)) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("LoadEntity = %v; want %v containing %q", err, tc.want, tc.text)
			}
			if manager.Get(id) != nil {
				t.Fatal("an unloadable aggregate was published")
			}
		})
	}
}
