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
	// retries and the commit (RR-20261005-NC-101). An error returned once the
	// commit was sent may mean it committed; treat it as an unknown result.
	// <= 0 selects the driver's 120s window.
	TransactionTimeout time.Duration
	RequireReplicaSet  bool // reject standalone deployments at startup
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
