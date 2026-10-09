package engine

import (
	"context"
	"errors"
	"fmt"
	"testing"

	coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
	"github.com/tjbdwanghaibo/roost-core/framework/entity"
)

func TestEntityRepositoryRejectsSchemaMismatchBeforePublishing(t *testing.T) {
	ensureDataEngineRepositoryEntity()
	for _, schema := range []uint32{0, 2} {
		for _, collection := range []string{"repository_profile", "repository_inventory"} {
			t.Run(fmt.Sprintf("%s/schema_%d", collection, schema), func(t *testing.T) {
				id, _ := entity.BuildEntityID(1401, dataEngineRepositoryKind)
				store := &repositoryStore{docs: map[string][]coredata.RawDocument{
					"repository_profile":   {repositoryRaw(t, "repository_profile", id, 4)},
					"repository_inventory": {repositoryRaw(t, "repository_inventory", id, 4)},
				}}
				store.docs[collection][0].Schema = schema
				manager := entity.NewEntityManager()
				repo, err := NewEntityRepository(manager, store, repositoryGate(true))
				if err != nil {
					t.Fatal(err)
				}
				loaded, err := repo.LoadEntity(context.Background(), id, dataEngineRepositoryKind)
				if !errors.Is(err, coredata.ErrSchemaMismatch) || loaded != nil || manager.Get(id) != nil {
					t.Fatalf("loaded=%v err=%v", loaded, err)
				}
				if store.transactions.Load() != 1 || store.docs[collection][0].Version != 4 || store.docs[collection][0].Schema != schema {
					t.Fatal("schema refusal retried or changed persisted document")
				}
			})
		}
	}
}
