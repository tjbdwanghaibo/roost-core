package driver

import (
	"context"
	"errors"
	"fmt"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
	"go.mongodb.org/mongo-driver/v2/mongo/readconcern"
	"go.mongodb.org/mongo-driver/v2/mongo/readpref"
	"go.mongodb.org/mongo-driver/v2/mongo/writeconcern"
)

// Client implements fmongo.IMongo by wrapping mongo-driver v2.
type Client struct {
	cli               *mongo.Client
	policy            IndexMigrationPolicy
	txnTimeout        time.Duration
	requireReplicaSet bool
}

func NewClient(cfg *fmongo.Config, policy IndexMigrationPolicy) (*Client, error) {
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
	return &Client{cli: cli, policy: policy, txnTimeout: cfg.TransactionTimeout, requireReplicaSet: cfg.RequireReplicaSet}, nil
}

func (c *Client) Database(name string) fmongo.IDatabase {
	return newDatabase(c.cli.Database(name), c.policy)
}

func (c *Client) DatabaseForSid(prefix string, sid int32) fmongo.IDatabase {
	name := fmt.Sprintf("%s_%d", prefix, sid)
	return newDatabase(c.cli.Database(name), c.policy)
}

func (c *Client) StartSession(ctx context.Context) (fmongo.ISession, error) {
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

func (c *Client) ValidateDeployment(ctx context.Context) error {
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

func (c *Client) Ping(ctx context.Context) error {
	return c.cli.Ping(ctx, nil)
}

// Close 断开客户端，幂等：mongo-driver 的 Disconnect 对已经断开的客户端返回 ErrClientDisconnected，
// 这时连接已经释放，重试也做不了任何事，按已关闭返回 nil（RR-20261005-NC-260，与 etcd Client.Close 的
// NC-173 残余同类）。旧实现原样返回，Mongo Mod 只在成功时交出 client，之后每次 Stop 都失败。
// Disconnect 断开各 server 时的 ctx 错误由驱动自己忽略，其余错误（FLE 客户端断开失败）照常返回。
func (c *Client) Close(ctx context.Context) error {
	if err := c.cli.Disconnect(ctx); err != nil && !errors.Is(err, mongo.ErrClientDisconnected) {
		return err
	}
	return nil
}

var _ fmongo.IMongo = (*Client)(nil)
