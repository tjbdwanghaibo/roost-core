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

// config 是 mongo.* 的声明（维护者决定 A4 ①）。时长与连接池写 0 取驱动缺省。
type config struct {
	URI                string        `config:"mongo.uri" default:"mongodb://localhost:27017" example:"mongodb://127.0.0.1:27017/?replicaSet=rs0"`
	ConnectTimeout     time.Duration `config:"mongo.connect_timeout" min:"0" example:"5s"`
	TransactionTimeout time.Duration `config:"mongo.transaction_timeout" min:"0" example:"30s"`
	RequireReplicaSet  bool          `config:"mongo.require_replica_set" default:"true" example:"true" help:"要求副本集（事务需要它）；只在确知用不到事务的单机环境写 false"`
	MaxPoolSize        int64         `config:"mongo.max_pool_size" min:"0" example:"100"`
	MinPoolSize        int64         `config:"mongo.min_pool_size" min:"0" example:"5"`
	MaxIdleTime        time.Duration `config:"mongo.max_idle_time" min:"0" example:"5m"`
	Index              struct {
		AllowRecreate bool `config:"allow_recreate" help:"索引定义变了时允许删掉重建（大集合上很慢，默认拒绝启动）"`
	} `config:"mongo.index"`
}

// ConfigSchema 声明 mongo.*。
func (m *MongoMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (m *MongoMod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("mongo mod: %w", err)
	}
	m.cfg = fmongo.DefaultConfig(settings.URI)
	if settings.ConnectTimeout > 0 {
		m.cfg.ConnectTimeout = settings.ConnectTimeout
	}
	if settings.MaxPoolSize > 0 {
		m.cfg.MaxPoolSize = uint64(settings.MaxPoolSize)
	}
	if settings.MinPoolSize > 0 {
		m.cfg.MinPoolSize = uint64(settings.MinPoolSize)
	}
	if settings.MaxIdleTime > 0 {
		m.cfg.MaxIdleTime = settings.MaxIdleTime
	}
	if settings.TransactionTimeout > 0 {
		m.cfg.TransactionTimeout = settings.TransactionTimeout
	}
	m.cfg.RequireReplicaSet = settings.RequireReplicaSet
	m.policy = mongodriver.IndexMigrationPolicy{AllowRecreate: settings.Index.AllowRecreate}
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
