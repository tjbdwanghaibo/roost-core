package mongo

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/app"
	"github.com/tjbdwanghaibo/roost-core/health"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
	"github.com/tjbdwanghaibo/roost-core/kit/mods"
	fmongo "github.com/tjbdwanghaibo/roost-core/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/mongo/driver"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/spf13/viper"
)

// MongoMod implements app.Mod for MongoDB connectivity.
// It creates an IMongo instance and registers it in the Registry.
type MongoMod struct {
	// stopSerial 串行化 StopWithContext（后到者在自己的 ctx 内等第一个做完）；mu 保护 client 字段，
	// 健康检查与 Client() 可能与停止并发（RR-20261006-10）。
	stopSerial operation.Serial
	mu         sync.Mutex
	client     fmongo.IMongo
	cfg        *fmongo.Config
	policy     mongodriver.IndexMigrationPolicy
}

func NewMongoMod() *MongoMod {
	return &MongoMod{}
}

func (m *MongoMod) Name() app.ModName { return mods.ModMongo }

func (m *MongoMod) Init(cfg *viper.Viper) error {
	uri := cfg.GetString("mongo.uri")
	if uri == "" {
		uri = "mongodb://localhost:27017"
	}
	m.cfg = fmongo.DefaultConfig(uri)
	read := app.NewConfigReader(cfg) // 严格读取（维护者决定 A4）

	if timeout := read.Duration("mongo.connect_timeout"); timeout > 0 {
		m.cfg.ConnectTimeout = timeout
	}
	if maxPool := read.Int64("mongo.max_pool_size"); maxPool > 0 {
		m.cfg.MaxPoolSize = uint64(maxPool)
	}
	if minPool := read.Int64("mongo.min_pool_size"); minPool > 0 {
		m.cfg.MinPoolSize = uint64(minPool)
	}
	if maxIdle := read.Duration("mongo.max_idle_time"); maxIdle > 0 {
		m.cfg.MaxIdleTime = maxIdle
	}
	if timeout := read.Duration("mongo.transaction_timeout"); timeout > 0 {
		m.cfg.TransactionTimeout = timeout
	}
	if cfg.IsSet("mongo.require_replica_set") {
		m.cfg.RequireReplicaSet = read.Bool("mongo.require_replica_set")
	}
	m.policy = mongodriver.IndexMigrationPolicy{
		AllowRecreate: read.Bool("mongo.index.allow_recreate"),
	}
	if err := read.Err(); err != nil {
		return fmt.Errorf("mongo mod: %w", err)
	}
	return nil
}

func (m *MongoMod) Provide(r *app.Registry) error {
	// mongo-driver v2 Connect does not dial immediately; Ping verifies connectivity.
	cli, err := mongodriver.NewClient(m.cfg, m.policy)
	if err != nil {
		return err
	}
	m.mu.Lock()
	m.client = cli
	m.mu.Unlock()
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return fmt.Errorf("mongo mod: capability %q not found", mods.ModHealth)
	}
	healthReg.Register("mongo", health.CheckerFunc(func(ctx context.Context) health.Result {
		client := m.Client()
		if client == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := client.Ping(checkCtx); err != nil {
			return health.Result{Status: health.StatusFail, Message: "ping failed", Err: err}
		}
		return health.Result{Status: health.StatusOK, Message: "connected"}
	}))
	return r.Register(mods.ModMongo, fmongo.IMongo(m.client))
}

func (m *MongoMod) Start() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	if err := m.client.Ping(ctx); err != nil {
		return err
	}
	if client, ok := m.client.(*mongodriver.Client); ok {
		if err := client.ValidateDeployment(ctx); err != nil {
			return err
		}
	}
	slog.Info("mongo mod: connected", "uri", redactedURI(m.cfg.URI))
	return nil
}

func (m *MongoMod) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.StopWithContext(ctx); err != nil {
		slog.Error("mongo mod: close failed", "err", err)
	}
}

// StopWithContext 断开客户端：成功后交出 client，之后再调用返回 nil；Close 失败或 ctx 已过期时保留
// client，下次 Stop 重试。并发调用串行执行，后到者在自己的 ctx 内等第一个做完（RR-20261006-10；旧实现
// 读写 m.client 不加锁，并发调用有数据竞争）。
func (m *MongoMod) StopWithContext(ctx context.Context) error {
	if m == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := m.stopSerial.Lock(ctx); err != nil {
		return err
	}
	defer m.stopSerial.Unlock()
	client := m.Client()
	if client == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := client.Close(ctx); err != nil {
		return err
	}
	slog.Info("mongo mod: closed")
	m.mu.Lock()
	m.client = nil
	m.mu.Unlock()
	return nil
}

// redactedURI 把 URI userinfo 里的口令换成 ***，保留用户名、主机与选项，供日志使用
// （RR-20261005-NC-191）。fmongo.Config 没有单独的用户名 / 口令字段，凭据只能写在 mongo.uri 里，
// 原样记录就把口令写进了日志。userinfo 取到 '?' 之前的最后一个 '@' 为止，口令里没转义的 '/' 也盖得住。
func redactedURI(uri string) string {
	scheme := strings.Index(uri, "://")
	if scheme < 0 {
		return uri
	}
	rest := uri[scheme+len("://"):]
	query := strings.IndexByte(rest, '?')
	if query < 0 {
		query = len(rest)
	}
	at := strings.LastIndexByte(rest[:query], '@')
	if at < 0 {
		return uri
	}
	user, _, hasPassword := strings.Cut(rest[:at], ":")
	if !hasPassword {
		return uri
	}
	return uri[:scheme+len("://")] + user + ":***" + rest[at:]
}

// Client returns the IMongo instance. Must be called after Start().
func (m *MongoMod) Client() fmongo.IMongo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client
}

var _ app.Mod = (*MongoMod)(nil)
var _ app.ModStopperWithContext = (*MongoMod)(nil)
