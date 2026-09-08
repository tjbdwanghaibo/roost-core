package mongo

import (
	"context"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// database implements fmongo.IDatabase.
type database struct {
	db     *mongo.Database
	policy IndexMigrationPolicy
}

func newDatabase(db *mongo.Database, policy IndexMigrationPolicy) *database {
	return &database{db: db, policy: policy}
}

func (d *database) Name() string {
	return d.db.Name()
}

func (d *database) Collection(name string) fmongo.ICollection {
	return newCollection(d.db.Collection(name), d.policy)
}

func (d *database) Drop(ctx context.Context) error {
	return d.db.Drop(ctx)
}

var _ fmongo.IDatabase = (*database)(nil)
