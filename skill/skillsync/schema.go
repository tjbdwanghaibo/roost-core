package skillsync

import (
	"errors"
	"fmt"
	"sort"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/syncstream"
)

var (
	ErrSchemaNegotiationFailed = errors.New("skillsync: no compatible schema version")
	ErrSchemaMigratorRequired  = errors.New("skillsync: schema migration is required")
	ErrSchemaMigrationInvalid  = errors.New("skillsync: schema migration is invalid")
	ErrSchemaMigrationExists   = errors.New("skillsync: schema migration already exists")
	ErrSchemaMigrationMissing  = errors.New("skillsync: schema migration path is missing")
	ErrSchemaRegistrySealed    = errors.New("skillsync: schema registry is sealed")
)

type SchemaRange struct {
	Min uint32
	Max uint32
}

func (value SchemaRange) Contains(version uint32) bool {
	return value.Min != 0 && version >= value.Min && version <= value.Max
}

func NegotiateSchema(server, client SchemaRange) (uint32, error) {
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

type SchemaMigrator interface {
	Migrate(packet syncstream.Packet, targetVersion uint32) ([]byte, error)
}

// SchemaMigration upgrades one packet payload across one registered edge. The
// packet supplied to the function already carries the edge's source version.
type SchemaMigration func(packet syncstream.Packet) ([]byte, error)

type schemaMigrationEdge struct {
	to      uint32
	migrate SchemaMigration
}

// SchemaRegistry is a concurrency-safe directed migration graph. Migrate uses
// the shortest path; equal-length paths are resolved by ascending version so a
// deployment cannot vary with map iteration order.
type SchemaRegistry struct {
	mutex  sync.RWMutex
	edges  map[uint32]map[uint32]SchemaMigration
	sealed bool
}

func NewSchemaRegistry() *SchemaRegistry {
	return &SchemaRegistry{edges: make(map[uint32]map[uint32]SchemaMigration)}
}

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

// Seal prevents configuration drift after process startup. Reads and
// migrations remain lock-safe after sealing.
func (registry *SchemaRegistry) Seal() {
	if registry == nil {
		return
	}
	registry.mutex.Lock()
	registry.sealed = true
	registry.mutex.Unlock()
}

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
