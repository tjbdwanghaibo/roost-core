package mongo

import (
	"context"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// mongoClient implements fmongo.IMongo by wrapping mongo-driver v2.
type mongoClient struct {
	cli    *mongo.Client
	policy IndexMigrationPolicy
}

func newMongoClient(cfg *fmongo.Config, policy IndexMigrationPolicy) (*mongoClient, error) {
	opts := options.Client().
		ApplyURI(cfg.URI).
		SetTimeout(cfg.ConnectTimeout).
		SetMaxPoolSize(cfg.MaxPoolSize).
		SetMinPoolSize(cfg.MinPoolSize)

	cli, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("mongo: connect: %w", err)
	}
	return &mongoClient{cli: cli, policy: policy}, nil
}

func (c *mongoClient) Database(name string) fmongo.IDatabase {
	return newDatabase(c.cli.Database(name), c.policy)
}

func (c *mongoClient) DatabaseForSid(prefix string, sid int32) fmongo.IDatabase {
	name := fmt.Sprintf("%s_%d", prefix, sid)
	return newDatabase(c.cli.Database(name), c.policy)
}

func (c *mongoClient) StartSession(ctx context.Context) (fmongo.ISession, error) {
	sess, err := c.cli.StartSession()
	if err != nil {
		return nil, err
	}
	return &session{sess: sess}, nil
}

func (c *mongoClient) Ping(ctx context.Context) error {
	return c.cli.Ping(ctx, nil)
}

func (c *mongoClient) Close(ctx context.Context) error {
	return c.cli.Disconnect(ctx)
}

var _ fmongo.IMongo = (*mongoClient)(nil)
