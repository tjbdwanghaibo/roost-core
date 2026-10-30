package mongo

import "time"

// Config holds MongoDB connection configuration.
type Config struct {
	URI            string        // "mongodb://localhost:27017"
	ConnectTimeout time.Duration // topology connection/server-selection timeout; default: 10s
	MaxPoolSize    uint64        // default: 100
	MinPoolSize    uint64        // default: 10
	MaxIdleTime    time.Duration // default: 5m
	// TransactionTimeout bounds one WithTransaction call end to end: callback
	// retries and the commit. An error returned once the
	// commit was sent may mean it committed; treat it as an unknown result.
	// <= 0 selects the driver's 120s window.
	TransactionTimeout time.Duration
	RequireReplicaSet  bool // reject standalone deployments at startup

	// Username / Password / AuthSource 是写在 URI 之外的账号（wiring/mongo 的 mongo.username / password / auth_source，
	// 可以来自环境变量，R11）。Username 为空时只用 URI 里的认证选项；非空时驱动把它们并进 URI 解析出的认证选项
	// （保留 URI 里的 authMechanism 等，AuthSource 为空时保留 URI 的 authSource）。URI 与这里不能同时带账号，由 wiring/mongo 检查。
	Username   string
	Password   string
	AuthSource string
}

func DefaultConfig(uri string) *Config {
	return &Config{
		URI:                uri,
		ConnectTimeout:     10 * time.Second,
		MaxPoolSize:        100,
		MinPoolSize:        10,
		MaxIdleTime:        5 * time.Minute,
		TransactionTimeout: 30 * time.Second,
		RequireReplicaSet:  true,
	}
}
