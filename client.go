package mongo

import (
	"context"
	"fmt"
	"time"

	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// mongoClient implements fmongo.IMongo by wrapping mongo-driver v2.
type mongoClient struct {
	cli               *mongo.Client
	policy            IndexMigrationPolicy
	txnTimeout        time.Duration
	requireReplicaSet bool
}

func newMongoClient(cfg *fmongo.Config, policy IndexMigrationPolicy) (*mongoClient, error) {
	wc := writeconcern.Majority()
	journal := true
	wc.Journal = &journal
	opts := options.Client().
		ApplyURI(cfg.URI).
		SetConnectTimeout(cfg.ConnectTimeout).
		SetServerSelectionTimeout(cfg.ConnectTimeout).
		SetMaxPoolSize(cfg.MaxPoolSize).
		SetMinPoolSize(cfg.MinPoolSize).
		SetMaxConnIdleTime(cfg.MaxIdleTime).
		SetReadConcern(readconcern.Majority()).
		SetWriteConcern(wc).
		SetReadPreference(readpref.Primary()).
		SetRetryReads(true).
		SetRetryWrites(true)

	cli, err := mongo.Connect(opts)
	if err != nil {
		return nil, fmt.Errorf("mongo: connect: %w", err)
	}
	return &mongoClient{cli: cli, policy: policy, txnTimeout: cfg.TransactionTimeout, requireReplicaSet: cfg.RequireReplicaSet}, nil
}

func (c *mongoClient) Database(name string) fmongo.IDatabase {
	return newDatabase(c.cli.Database(name), c.policy)
}

func (c *mongoClient) DatabaseForSid(prefix string, sid int32) fmongo.IDatabase {
	name := fmt.Sprintf("%s_%d", prefix, sid)
	return newDatabase(c.cli.Database(name), c.policy)
}

func (c *mongoClient) StartSession(ctx context.Context) (fmongo.ISession, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	sess, err := c.cli.StartSession()
	if err != nil {
		return nil, err
	}
	wc := writeconcern.Majority()
	journal := true
	wc.Journal = &journal
	return &session{sess: sess, timeout: c.txnTimeout, options: options.Transaction().
		SetReadConcern(readconcern.Snapshot()).
		SetReadPreference(readpref.Primary()).
		SetWriteConcern(wc)}, nil
}

func (c *mongoClient) validateDeployment(ctx context.Context) error {
	if c == nil || c.cli == nil {
		return fmt.Errorf("mongo: client is not initialized")
	}
	var hello struct {
		SetName               string `bson:"setName"`
		Message               string `bson:"msg"`
		LogicalSessionTimeout *int64 `bson:"logicalSessionTimeoutMinutes"`
	}
	if err := c.cli.Database("admin").RunCommand(ctx, bson.D{{Key: "hello", Value: 1}}).Decode(&hello); err != nil {
		return fmt.Errorf("mongo: hello: %w", err)
	}
	if c.requireReplicaSet && hello.SetName == "" && hello.Message != "isdbgrid" {
		return fmt.Errorf("mongo: production mode requires a replica set or sharded transaction deployment")
	}
	if hello.LogicalSessionTimeout == nil {
		return fmt.Errorf("mongo: deployment does not support durable sessions/transactions")
	}
	return nil
}

func (c *mongoClient) Ping(ctx context.Context) error {
	return c.cli.Ping(ctx, nil)
}

func (c *mongoClient) Close(ctx context.Context) error {
	return c.cli.Disconnect(ctx)
}

var _ fmongo.IMongo = (*mongoClient)(nil)
