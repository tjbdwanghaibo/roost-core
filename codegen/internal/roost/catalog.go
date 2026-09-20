package roost

import (
	"fmt"
	"sort"
)

type modSpec struct {
	ImportPath  string
	Alias       string
	Constructor string
	Depends     []string
	Config      string
	DevService  string
}

var modCatalog = map[string]modSpec{
	"lock": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/lock", Alias: "kitlock", Constructor: "kitlock.NewLockMod()",
	},
	"ops": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/ops", Alias: "kitops", Constructor: "kitops.NewOpsMod()",
		Config: "ops:\n  enabled: true\n  addr: 127.0.0.1:9100\n  admin_enabled: false\n  admin_token: \"\"\n  allow_dev_token: false\n",
	},
	"statslog": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/statslog", Alias: "kitstatslog", Constructor: "kitstatslog.NewStatsLogMod()",
		Config: "stats_log:\n  enabled: true\n  dir: log\n  interval: 1m\n",
	},
	"configdata": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/configdata", Alias: "kitconfigdata", Constructor: "kitconfigdata.NewConfigDataMod()",
		Config: "config_data:\n  dir: configs/data\n",
	},
	"etcd": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/etcd", Alias: "kitetcd", Constructor: "kitetcd.NewEtcdMod()",
		Config:     "etcd:\n  endpoints: 127.0.0.1:2379\n  username: \"\"\n  password: \"\"\n  service_prefix: /roost/services\n  lease_ttl: 10\n  advertise_addr: 127.0.0.1:9000\n",
		DevService: "etcd",
	},
	"redis": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/redis", Alias: "kitredis", Constructor: "kitredis.NewRedisMod()",
		Config:     "redis:\n  addr: 127.0.0.1:6379\n  password: \"\"\n  db: 0\n  pool_size: 32\n  min_idle_conns: 4\n",
		DevService: "redis",
	},
	"mongo": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/mongo", Alias: "kitmongo", Constructor: "kitmongo.NewMongoMod()",
		Config:     "mongo:\n  uri: mongodb://127.0.0.1:27017/?replicaSet=rs0\n  connect_timeout: 5s\n  transaction_timeout: 30s\n  require_replica_set: true\n  max_pool_size: 100\n  min_pool_size: 5\n  max_idle_time: 5m\n",
		DevService: "mongo",
	},
	"nats": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/nats", Alias: "kitnats", Constructor: "kitnats.NewNatsMod(nil)",
		Config:     "nats:\n  url: nats://127.0.0.1:4222\n  prefix: roost\n  worker_num: 8\n  reliable:\n    enabled: false\n",
		DevService: "nats",
	},
	"room": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/room", Alias: "kitroom", Constructor: "kitroom.NewRoomMod(0)", Depends: []string{"nats"},
		Config: "room:\n  transport: jetstream\n  prefix: roost.room\n  storage: file\n  replicas: 1\n  publish_timeout: 3s\n",
	},
	"remote_entity": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/remoteentity", Alias: "kitremoteentity", Depends: []string{"redis", "mongo", "room"},
		Config: "remote_entity:\n  lock_ttl: 15s\n  retry_count: 3\n  retry_delay: 100ms\n  op_timeout: 3s\n  unlock_retry_count: 5\n  unlock_retry_interval: 100ms\n  version_ttl: 24h\n  finalize_retry_interval: 500ms\n  max_write_batch: 64\n  wrapper_capacity: 65536\n  wrapper_idle_ttl: 5m\n  snapshot_cache_shards: 64\n  snapshot_cache_entries: 10000\n  snapshot_cache_bytes: 268435456\n  snapshot_cache_ttl: 30s\n  snapshot_l2_ttl: 10m\n  snapshot_interest_ttl: 30s\n  snapshot_interest_keys: 10000\n  snapshot_interest_subs: 100000\n  marker_cache_ttl: 2s\n  snapshot_load_timeout: 3s\n  snapshot_max_waiters: 4096\n  async_finalize_capacity: 4096\n  async_finalize_workers: 16\n  transaction_track_limit: 100000\n  transaction_track_ttl: 10m\n  mongo:\n    database: remote_entity\n    transaction_ttl: 168h\n",
	},
	"dataengine": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/dataengine", Alias: "kitdataengine", Depends: []string{"mongo", "nats"},
		Config: "persistence:\n  engine: dataengine\nnest:\n  worker_num: 8\n  heartbeat_worker_num: 2\n  queue_capacity: 4096\n  delayed_capacity: 4096\n  max_delay: 24h\n  tick_duration: 50ms\n  request_timeout: 3s\n  pipelined:\n    allowlist: []\n    async: false\n    async_workers: 8\n    async_queue_capacity: 4096\ndataengine:\n  database: game\n  startup_timeout: 30s\n  shutdown_timeout: 30s\n  transaction_receipt_ttl: 720h\n  receipt_ttl: 720h\n  wal:\n    dir: data/wal/dataengine\n    writer_version: 2\n    segment_bytes: 268435456\n    max_disk_bytes: 8589934592\n    max_unacked_age: 24h\n    queue_capacity: 8192\n    group_commit_interval: 2ms\n  projection:\n    batch_records: 256\n    retry_min: 100ms\n    retry_max: 5s\n  outbox:\n    workers: 2\n    batch_size: 64\n    lease_duration: 30s\n    poll_interval: 100ms\n    retry_min: 1s\n    retry_max: 1m\n    max_pending: 1000000\n    max_oldest_age: 30m\n  effects:\n    subject_prefix: roost.effect\n    stream: ROOST_EFFECTS\n    max_age: 168h\n    max_bytes: 8589934592\n    duplicate_window: 10m\n    replicas: 1\n",
	},
	"manager": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/manager", Alias: "kitmanager",
	},
	"nest": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/nest", Alias: "kitnest",
	},
	"saga": {
		ImportPath: "github.com/tjbdwanghaibo/roost-kit/saga", Alias: "kitsaga", Constructor: "kitsaga.NewMod()", Depends: []string{"mongo", "nats"},
		Config: "saga:\n  database: saga\n  subject_prefix: roost.saga\n  stream: ROOST_SAGA\n  coordinator_workers: 4\n  publisher_workers: 4\n  coordinator_claim_batch: 3\n  publisher_claim_batch: 1\n  lease_duration: 15s\n  store_timeout: 3s\n  poll_interval: 100ms\n  publish_timeout: 3s\n  publish_backoff_min: 50ms\n  publish_backoff_max: 5s\n  max_payload_bytes: 65536\n  completion_receipt_ttl: 720h\n  stream_max_age: 168h\n  stream_max_bytes: 8589934592\n  duplicate_window: 10m\n  replicas: 1\n  result_ack_wait: 30s\n  result_process_timeout: 3s\n  result_max_deliver: 25000\n  result_max_ack_pending: 256\n  result_nak_backoff_min: 250ms\n  result_nak_backoff_max: 30s\n  start_effect_stream: ROOST_EFFECTS\n  start_effect_prefix: roost.effect\n  start_effect_durable: roost-saga-start\n  start_effect_ack_wait: 30s\n  start_effect_process_timeout: 3s\n  start_effect_max_deliver: 25000\n  start_effect_max_ack_pending: 256\n  start_effect_nak_backoff_min: 250ms\n  start_effect_nak_backoff_max: 30s\n  result_effect_durable: roost-saga-start-result\n  result_effect_ack_wait: 30s\n  result_effect_process_timeout: 3s\n  result_effect_max_deliver: 25000\n  result_effect_max_ack_pending: 256\n  result_effect_nak_backoff_min: 250ms\n  result_effect_nak_backoff_max: 30s\n",
	},
}

var knownFeatures = map[string]bool{
	"protocol": true, "config": true, "entity": true, "nest": true,
	"event": true, "dao": true, "attribute": true, "webroute": true, "rpc": true,
	"errcode":           true,
	"saga":              true,
	"nettransport-quic": true, "nettransport-kcp": true, "nettransport-udp": true,
}

func resolveMods(requested []string) ([]string, error) {
	requested = append([]string(nil), requested...)
	needsPersistence := contains(requested, "nest") || contains(requested, "saga")
	if needsPersistence && !contains(requested, "dataengine") {
		requested = append(requested, "dataengine")
	}
	seen := map[string]bool{}
	visiting := map[string]bool{}
	var out []string
	var visit func(string) error
	visit = func(name string) error {
		spec, ok := modCatalog[name]
		if !ok {
			return fmt.Errorf("unknown kit mod %q", name)
		}
		if seen[name] {
			return nil
		}
		if visiting[name] {
			return fmt.Errorf("kit mod dependency cycle at %q", name)
		}
		visiting[name] = true
		for _, dependency := range spec.Depends {
			if err := visit(dependency); err != nil {
				return err
			}
		}
		visiting[name] = false
		seen[name] = true
		out = append(out, name)
		return nil
	}
	for _, name := range requested {
		if err := visit(name); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func allProjectMods(m Manifest) []string {
	requested := append([]string(nil), m.SharedMods...)
	for name := range m.Services {
		requested = append(requested, effectiveServiceMods(m, name)...)
	}
	resolved, err := resolveMods(requested)
	if err == nil {
		return resolved
	}
	// Manifest validation reports the actual catalog error. Keep rendering
	// deterministic for callers that are collecting more than one diagnostic.
	seen := map[string]bool{}
	for _, mod := range requested {
		seen[mod] = true
	}
	out := make([]string, 0, len(seen))
	for mod := range seen {
		out = append(out, mod)
	}
	sort.Strings(out)
	return out
}
