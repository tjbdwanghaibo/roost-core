package driver

import (
	"context"
	"errors"
	"fmt"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	"github.com/tjbdwanghaibo/roost-core/internal/hoptrace"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/event"
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

	// closeSerial 串行化 Close：并发的后到者等第一次断开做完，再按 closed 判断。
	closeSerial operation.Serial
	closed      bool
	// disconnect 为 nil 时用 cli.Disconnect；只有测试替换它，用来控制断开的耗时与结果。
	disconnect func(context.Context) error
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
	if hoptrace.Enabled() {
		// 性能夹具在建连接之前安装了逐跳计时：按命令名记每次往返（跳名 mongo.<命令>，命令名是固定的十几个）。
		// 生产不安装，连接上没有监视器。
		opts.SetMonitor(&event.CommandMonitor{
			Succeeded: func(_ context.Context, succeeded *event.CommandSucceededEvent) {
				hoptrace.Observe("mongo."+succeeded.CommandName, succeeded.Duration)
			},
			Failed: func(_ context.Context, failed *event.CommandFailedEvent) {
				hoptrace.Observe("mongo."+failed.CommandName+".failed", failed.Duration)
			},
		})
	}

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

// Close 断开客户端，幂等：断开成功之后再调用返回 nil；Close 之后的命令返回 mongo.ErrClientDisconnected。
// mongo-driver 的 Disconnect 对已经断开的客户端返回 ErrClientDisconnected，这时连接已经释放，按已关闭
// 返回 nil。Disconnect 断开各 server 时的 ctx 错误由驱动自己忽略，其余错误
// （FLE 客户端断开失败，拓扑没断开）照常返回，下一次 Close 重做断开。
//
// 并发调用串行执行：后到者在自己的 ctx 内等第一次断开做完，ctx 先结束返回 ctx 错误（客户端状态不变，
// 可以再调用）。不串行时后到者会从驱动拿到 ErrClientDisconnected 立刻返回 nil，而第一个调用者可能
// 还在等在途连接归还、关连接池。
func (c *Client) Close(ctx context.Context) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := c.closeSerial.Lock(ctx); err != nil {
		return err
	}
	defer c.closeSerial.Unlock()
	if c.closed {
		return nil
	}
	if err := c.disconnectClient(ctx); err != nil && !errors.Is(err, mongo.ErrClientDisconnected) {
		return err
	}
	c.closed = true
	return nil
}

func (c *Client) disconnectClient(ctx context.Context) error {
	if c.disconnect != nil {
		return c.disconnect(ctx)
	}
	return c.cli.Disconnect(ctx)
}

var _ fmongo.IMongo = (*Client)(nil)
