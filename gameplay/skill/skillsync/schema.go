package skillsync

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/framework/sync/syncstream"
)

var (
	ErrSchemaNegotiationFailed = errors.New("skillsync: no compatible schema version")
	ErrSchemaMigratorRequired  = errors.New("skillsync: schema migration is required")
	ErrSchemaMigrationInvalid  = errors.New("skillsync: schema migration is invalid")
	ErrSchemaMigrationExists   = errors.New("skillsync: schema migration already exists")
	ErrSchemaMigrationMissing  = errors.New("skillsync: schema migration path is missing")
	ErrSchemaRegistrySealed    = errors.New("skillsync: schema registry is sealed")
)

// SchemaRange 是闭区间 [Min, Max] 的 schema 版本范围；Min 为 0 表示没有可用版本。
type SchemaRange struct {
	Min uint32
	Max uint32
}

// Contains 报告版本是否在范围内。
func (value SchemaRange) Contains(version uint32) bool {
	return value.Min != 0 && version >= value.Min && version <= value.Max
}

// NegotiateSchema 返回两边区间交集里的最高版本。任一边是空区间（Min 为 0，或
// Min > Max）时协商失败：Contains 与 NewApplier 都把 Min 0 当作“没有可用版本”，所以不能只在合并后的 minimum 为 0
// 时失败——一边 Min 0、另一边 Min 非 0 时会返回一个那一边并不支持的版本。
func NegotiateSchema(server, client SchemaRange) (uint32, error) {
	if server.Min == 0 || client.Min == 0 || server.Min > server.Max || client.Min > client.Max {
		return 0, ErrSchemaNegotiationFailed
	}
	minimum := server.Min
	if client.Min > minimum {
		minimum = client.Min
	}
	maximum := server.Max
	if client.Max < maximum {
		maximum = client.Max
	}
	if minimum == 0 || minimum > maximum {
		return 0, ErrSchemaNegotiationFailed
	}
	return maximum, nil
}

// SchemaMigrator 把包的载荷迁移到目标版本。
type SchemaMigrator interface {
	Migrate(packet syncstream.Packet, targetVersion uint32) ([]byte, error)
}

// SchemaMigration 把一个包的载荷沿一条已注册的边升级。传给函数的包已经带着这条边的源版本。
type SchemaMigration func(packet syncstream.Packet) ([]byte, error)

// SchemaRegistry 是并发安全的有向迁移图。Migrate 走最短路径；等长路径按版本升序选择，部署结果不随 map 迭代顺序变化。
type SchemaRegistry struct {
	mutex  sync.RWMutex
	edges  map[uint32]map[uint32]SchemaMigration
	sealed bool
}

// NewSchemaRegistry 返回空的迁移图。
func NewSchemaRegistry() *SchemaRegistry {
	return &SchemaRegistry{edges: make(map[uint32]map[uint32]SchemaMigration)}
}

// Register 注册一条迁移边；参数无效、边已存在或已封存时返回对应错误。
func (registry *SchemaRegistry) Register(from, to uint32, migration SchemaMigration) error {
	if registry == nil || from == 0 || to == 0 || from == to || migration == nil {
		return ErrSchemaMigrationInvalid
	}
	registry.mutex.Lock()
	defer registry.mutex.Unlock()
	if registry.sealed {
		return ErrSchemaRegistrySealed
	}
	if registry.edges == nil {
		registry.edges = make(map[uint32]map[uint32]SchemaMigration)
	}
	if registry.edges[from] == nil {
		registry.edges[from] = make(map[uint32]SchemaMigration)
	}
	if registry.edges[from][to] != nil {
		return ErrSchemaMigrationExists
	}
	registry.edges[from][to] = migration
	return nil
}

// Seal 防止进程启动后配置漂移。封存之后读取与迁移仍然是加锁安全的。
func (registry *SchemaRegistry) Seal() {
	if registry == nil {
		return
	}
	registry.mutex.Lock()
	registry.sealed = true
	registry.mutex.Unlock()
}

// MigrationPath 返回从 from 到 to 的最短迁移路径（含两端版本）。
func (registry *SchemaRegistry) MigrationPath(from, to uint32) ([]uint32, error) {
	if registry == nil || from == 0 || to == 0 {
		return nil, ErrSchemaMigrationInvalid
	}
	if from == to {
		return []uint32{from}, nil
	}
	registry.mutex.RLock()
	defer registry.mutex.RUnlock()
	type node struct {
		version uint32
		path    []uint32
	}
	queue := []node{{version: from, path: []uint32{from}}}
	visited := map[uint32]bool{from: true}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		next := make([]int, 0, len(registry.edges[current.version]))
		for version := range registry.edges[current.version] {
			next = append(next, int(version))
		}
		sort.Ints(next)
		for _, raw := range next {
			version := uint32(raw)
			if visited[version] {
				continue
			}
			path := append(append([]uint32(nil), current.path...), version)
			if version == to {
				return path, nil
			}
			visited[version] = true
			queue = append(queue, node{version: version, path: path})
		}
	}
	return nil, fmt.Errorf("%w: %d -> %d", ErrSchemaMigrationMissing, from, to)
}

// Migrate 沿 MigrationPath 逐边迁移包的载荷。
func (registry *SchemaRegistry) Migrate(packet syncstream.Packet, targetVersion uint32) ([]byte, error) {
	path, err := registry.MigrationPath(packet.SchemaVersion, targetVersion)
	if err != nil {
		return nil, err
	}
	current := packet.Clone()
	for index := 1; index < len(path); index++ {
		from, to := path[index-1], path[index]
		registry.mutex.RLock()
		migration := registry.edges[from][to]
		registry.mutex.RUnlock()
		if migration == nil {
			return nil, ErrSchemaMigrationMissing
		}
		current.SchemaVersion = from
		payload, migrateErr := migration(current.Clone())
		if migrateErr != nil {
			return nil, fmt.Errorf("skillsync: migrate schema %d -> %d: %w", from, to, migrateErr)
		}
		if payload == nil {
			return nil, fmt.Errorf("%w: migration %d -> %d returned nil", ErrSchemaMigrationInvalid, from, to)
		}
		current.Payload = append([]byte(nil), payload...)
		current.SchemaVersion = to
	}
	return append([]byte(nil), current.Payload...), nil
}
