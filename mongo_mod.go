package mongo

import (
	"context"
	"fmt"
	"github.com/tjbdwanghaibo/cube-core/app"
	"github.com/tjbdwanghaibo/cube-core/health"
	fmongo "github.com/tjbdwanghaibo/cube-core/mongo"
	"github.com/tjbdwanghaibo/cube-kit/mods"
	"log/slog"
	"time"

	"github.com/spf13/viper"
)

// MongoMod implements app.Mod for MongoDB connectivity.
// It creates an IMongo instance and registers it in the Registry.
type MongoMod struct {
	client fmongo.IMongo
	cfg    *fmongo.Config
	policy IndexMigrationPolicy
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

	if timeout := cfg.GetDuration("mongo.connect_timeout"); timeout > 0 {
		m.cfg.ConnectTimeout = timeout
	}
	if maxPool := cfg.GetUint64("mongo.max_pool_size"); maxPool > 0 {
		m.cfg.MaxPoolSize = maxPool
	}
	if minPool := cfg.GetUint64("mongo.min_pool_size"); minPool > 0 {
		m.cfg.MinPoolSize = minPool
	}
	if maxIdle := cfg.GetDuration("mongo.max_idle_time"); maxIdle > 0 {
		m.cfg.MaxIdleTime = maxIdle
	}
	if timeout := cfg.GetDuration("mongo.transaction_timeout"); timeout > 0 {
		m.cfg.TransactionTimeout = timeout
	}
	if cfg.IsSet("mongo.require_replica_set") {
		m.cfg.RequireReplicaSet = cfg.GetBool("mongo.require_replica_set")
	}
	m.policy = IndexMigrationPolicy{
		AllowRecreate: cfg.GetBool("mongo.index.allow_recreate"),
	}

	return nil
}

func (m *MongoMod) Provide(r *app.Registry) error {
	// mongo-driver v2 Connect does not dial immediately; Ping verifies connectivity.
	cli, err := newMongoClient(m.cfg, m.policy)
	if err != nil {
		return err
	}
	m.client = cli
	healthReg, ok := app.Lookup[*health.Registry](r, mods.ModHealth)
	if !ok || healthReg == nil {
		return fmt.Errorf("mongo mod: capability %q not found", mods.ModHealth)
	}
	healthReg.Register("mongo", health.CheckerFunc(func(ctx context.Context) health.Result {
		if m.client == nil {
			return health.Result{Status: health.StatusFail, Message: "client not initialized"}
		}
		checkCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
		defer cancel()
		if err := m.client.Ping(checkCtx); err != nil {
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
	if client, ok := m.client.(*mongoClient); ok {
		if err := client.validateDeployment(ctx); err != nil {
			return err
		}
	}
	slog.Info("mongo mod: connected", "uri", m.cfg.URI)
	return nil
}

func (m *MongoMod) Stop() {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.StopWithContext(ctx); err != nil {
		slog.Error("mongo mod: close failed", "err", err)
	}
}

func (m *MongoMod) StopWithContext(ctx context.Context) error {
	if m == nil || m.client == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	err := m.client.Close(ctx)
	if err == nil {
		slog.Info("mongo mod: closed")
		m.client = nil
	}
	return err
}

// Client returns the IMongo instance. Must be called after Start().
func (m *MongoMod) Client() fmongo.IMongo {
	return m.client
}

var _ app.Mod = (*MongoMod)(nil)
var _ app.ModStopperWithContext = (*MongoMod)(nil)
