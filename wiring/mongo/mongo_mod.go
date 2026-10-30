package mongo

import (
	"context"
	"errors"
	"fmt"
	"github.com/tjbdwanghaibo/roost-core/framework/app"
	"github.com/tjbdwanghaibo/roost-core/infra/observe/health"
	fmongo "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo"
	mongodriver "github.com/tjbdwanghaibo/roost-core/infra/storage/mongo/driver"
	"github.com/tjbdwanghaibo/roost-core/internal/operation"
	"github.com/tjbdwanghaibo/roost-core/wiring/mods"
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
	// 健康检查与 Client() 可能与停止并发。
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

// config 是 mongo.* 的声明（维护者决定）。时长与连接池写 0 取驱动缺省。
//
// 连接串与账号可以从环境变量读（R11）：mongo.uri / username / password 各有 `_env` 伴随键，规则见 wiring/mods 的 EnvValues
// （docs/framework/guide/13-wiring.md §2.1）。账号也可以写在 mongo.uri 的 userinfo 里，二选一，见 connection。
type config struct {
	URI                string        `config:"mongo.uri" default:"mongodb://localhost:27017" example:"mongodb://127.0.0.1:27017/?replicaSet=rs0"`
	URIEnv             string        `config:"mongo.uri_env" help:"从这个环境变量读 mongo.uri（变量必须已设置且非空，取代 mongo.uri）"`
	Username           string        `config:"mongo.username" help:"账号（与 mongo.password 同时写），不写在 mongo.uri 里时用"`
	UsernameEnv        string        `config:"mongo.username_env" help:"从这个环境变量读 mongo.username"`
	Password           string        `config:"mongo.password" help:"口令（与 mongo.username 同时写）"`
	PasswordEnv        string        `config:"mongo.password_env" help:"从这个环境变量读 mongo.password"`
	AuthSource         string        `config:"mongo.auth_source" help:"mongo.username 所在的认证库（例如 admin）；不写取驱动缺省（URI 里的库名，没有则 admin）"`
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

// endpoint 是连接串与账号的取值（R11）：Init 经 mods.ReadEnv 读环境变量，ValidateConfig 经 mods.CheckEnvNames 只查变量名。
func (c *config) endpoint(env *mods.EnvValues) (uri, username, password string) {
	return env.String("mongo.uri", c.URI, c.URIEnv),
		env.String("mongo.username", c.Username, c.UsernameEnv),
		env.String("mongo.password", c.Password, c.PasswordEnv)
}

// ValidateConfig 只检查 `_env` 的变量名（配置检查不读环境变量）。
func (c *config) ValidateConfig(bool) error {
	env := mods.CheckEnvNames()
	c.endpoint(env)
	return env.Err()
}

// connection 读环境变量、合成连接配置（Init 与运维工具的 Connection 共用）。账号规则与 cube 相同并补上与 URI 的关系：
// mongo.username 与 mongo.password 同时写或都不写；写了 mongo.username 时 mongo.uri 不能再带 userinfo（两处都有账号时
// 驱动静默用后者，这里直接拒绝）；mongo.auth_source 只跟 mongo.username 用（URI 里的账号写 ?authSource=）。
// 错误点名键，不带 URI 与口令。
func (c *config) connection() (*fmongo.Config, error) {
	env := mods.ReadEnv()
	uri, username, password := c.endpoint(env)
	if err := env.Err(); err != nil {
		return nil, err
	}
	var errs []error
	if (username == "") != (password == "") {
		errs = append(errs, errors.New("config: mongo.username and mongo.password must be set together (directly or through their _env keys)"))
	}
	if username != "" && uriHasUserinfo(uri) {
		errs = append(errs, errors.New("config: mongo.uri already carries credentials (user:password@); remove them from the URI or drop mongo.username / mongo.password"))
	}
	if c.AuthSource != "" && username == "" {
		errs = append(errs, errors.New("config: mongo.auth_source applies to mongo.username, which is not set; with credentials in mongo.uri write ?authSource= there"))
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	out := fmongo.DefaultConfig(uri)
	out.Username, out.Password, out.AuthSource = username, password, c.AuthSource
	if c.ConnectTimeout > 0 {
		out.ConnectTimeout = c.ConnectTimeout
	}
	if c.MaxPoolSize > 0 {
		out.MaxPoolSize = uint64(c.MaxPoolSize)
	}
	if c.MinPoolSize > 0 {
		out.MinPoolSize = uint64(c.MinPoolSize)
	}
	if c.MaxIdleTime > 0 {
		out.MaxIdleTime = c.MaxIdleTime
	}
	if c.TransactionTimeout > 0 {
		out.TransactionTimeout = c.TransactionTimeout
	}
	out.RequireReplicaSet = c.RequireReplicaSet
	return out, nil
}

// ConfigSchema 声明 mongo.*。
func (m *MongoMod) ConfigSchema() app.ConfigSchema { return app.SchemaOf(config{}) }

func (m *MongoMod) Init(cfg *viper.Viper) error {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return fmt.Errorf("mongo mod: %w", err)
	}
	conn, err := settings.connection()
	if err != nil {
		return fmt.Errorf("mongo mod: %w", err)
	}
	m.cfg = conn
	m.policy = mongodriver.IndexMigrationPolicy{AllowRecreate: settings.Index.AllowRecreate}
	return nil
}

// Connection 按 Mongo Mod 的声明与同一条规则（含 `_env` 环境变量，R11）读 mongo.* 的连接配置，不建连接。
// 运维工具（wiring/sidmerge）用：与 game 进程连同一个 Mongo、用同一个账号。
func Connection(cfg *viper.Viper) (*fmongo.Config, error) {
	var settings config
	if err := app.LoadConfig(cfg, &settings); err != nil {
		return nil, fmt.Errorf("mongo: %w", err)
	}
	conn, err := settings.connection()
	if err != nil {
		return nil, fmt.Errorf("mongo: %w", err)
	}
	return conn, nil
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
// client，下次 Stop 重试。并发调用串行执行，后到者在自己的 ctx 内等第一个做完；读写 m.client 都在锁内。
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

// redactedURI 把 URI userinfo 里的口令换成 ***，保留用户名、主机与选项，供日志使用。
// 口令可以写在 mongo.uri 的 userinfo 里（也可以单独写 mongo.password，那个不进日志），原样记录就把口令写进了日志。
// userinfo 取到 '?' 之前的最后一个 '@' 为止，口令里没转义的 '/' 也盖得住。
func redactedURI(uri string) string {
	prefix, userinfo, rest, ok := splitUserinfo(uri)
	if !ok {
		return uri
	}
	user, _, hasPassword := strings.Cut(userinfo, ":")
	if !hasPassword {
		return uri
	}
	return prefix + user + ":***" + rest
}

// uriHasUserinfo 报告 URI 是否带 userinfo（user@ 或 user:password@）。
func uriHasUserinfo(uri string) bool {
	_, _, _, ok := splitUserinfo(uri)
	return ok
}

// splitUserinfo 把 URI 切成 "scheme://"、userinfo 与 "@主机…"；没有 userinfo 时 ok 为 false。
func splitUserinfo(uri string) (prefix, userinfo, rest string, ok bool) {
	scheme := strings.Index(uri, "://")
	if scheme < 0 {
		return "", "", "", false
	}
	after := uri[scheme+len("://"):]
	query := strings.IndexByte(after, '?')
	if query < 0 {
		query = len(after)
	}
	at := strings.LastIndexByte(after[:query], '@')
	if at < 0 {
		return "", "", "", false
	}
	return uri[:scheme+len("://")], after[:at], after[at:], true
}

// Client returns the IMongo instance. Must be called after Start().
func (m *MongoMod) Client() fmongo.IMongo {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.client
}

var _ app.Mod = (*MongoMod)(nil)
var _ app.ModStopperWithContext = (*MongoMod)(nil)
