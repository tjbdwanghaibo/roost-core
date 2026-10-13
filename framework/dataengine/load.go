package dataengine

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/metrics"
)

var (
	ErrStoreRequired      = errors.New("dataengine: store is required")
	ErrRecoveryIncomplete = errors.New("dataengine: projection recovery is incomplete")
	ErrLoadDependency     = errors.New("dataengine loader: invalid dependency graph")
	ErrLoadCallback       = errors.New("dataengine loader: callback failed")
)

// RawDocument is the storage-neutral representation returned by Data Engine.
// Data contains the complete encoded document, including its identity fields.
type RawDocument struct {
	Key         DocumentKey
	Version     uint64
	Schema      uint32
	Deleted     bool
	Data        []byte
	MarkerEpoch uint64
	LockFence   uint64
	RouteEpoch  uint64
	Enveloped   bool
}

type LoadSpec struct {
	Database  string
	Scope     DatabaseScope
	Resource  string
	Filter    map[string]any
	BatchSize int
}

// Store is the single read/projection abstraction owned by Data Engine.
type Store interface {
	ReadConsistent(context.Context, func(context.Context) error) error
	Load(context.Context, LoadSpec) ([]RawDocument, error)
	StreamLoad(context.Context, LoadSpec, func(RawDocument) error) error
}

type EntityExister interface {
	Exists(int64) bool
}

type LoadTemplate struct {
	Database  string
	Scope     DatabaseScope
	Resource  string
	DependsOn []string
	Filter    map[string]any
	BatchSize int
	OnLoad    func(RawDocument) error
	Strict    bool
}

// Loader 按 DependsOn 拓扑分层加载资源：同层资源并发加载，层与层之间串行；
// 任一资源失败即取消本层其余加载并返回第一个错误。
type Loader struct {
	store       Store
	exister     EntityExister
	concurrency int
}

func NewLoader(store Store, exister EntityExister, concurrency ...int) *Loader {
	limit := 4
	if len(concurrency) > 0 && concurrency[0] > 0 {
		limit = concurrency[0]
	}
	return &Loader{store: store, exister: exister, concurrency: limit}
}

// LoadAll 校验依赖图（资源名唯一、依赖存在、无环）后逐层加载。
func (loader *Loader) LoadAll(ctx context.Context, templates []LoadTemplate) error {
	if loader == nil || loader.store == nil {
		return ErrStoreRequired
	}
	if ctx == nil {
		ctx = context.Background()
	}
	order, err := topoSort(templates)
	if err != nil {
		return err
	}
	for _, level := range buildLevels(order, templates) {
		if err := loader.loadLevel(ctx, level, templates); err != nil {
			return err
		}
	}
	return nil
}

func (loader *Loader) loadLevel(ctx context.Context, indices []int, templates []LoadTemplate) error {
	if len(indices) == 0 {
		return nil
	}
	if len(indices) == 1 {
		return loader.loadOne(ctx, templates[indices[0]])
	}
	workerCount := min(loader.concurrency, len(indices))
	workCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan int)
	errCh := make(chan error, 1)
	var workers sync.WaitGroup
	for range workerCount {
		workers.Go(func() {
			for index := range jobs {
				if err := loader.loadOne(workCtx, templates[index]); err != nil {
					select {
					case errCh <- err:
						cancel()
					default:
					}
					return
				}
			}
		})
	}
send:
	for _, index := range indices {
		select {
		case jobs <- index:
		case <-workCtx.Done():
			break send
		}
	}
	close(jobs)
	workers.Wait()
	select {
	case err := <-errCh:
		return err
	default:
		return ctx.Err()
	}
}

func (loader *Loader) loadOne(ctx context.Context, template LoadTemplate) error {
	spec := LoadSpec{
		Database: template.Database, Scope: template.Scope, Resource: template.Resource,
		Filter: template.Filter, BatchSize: template.BatchSize,
	}
	return loader.store.StreamLoad(ctx, spec, func(doc RawDocument) error {
		if doc.Deleted || (loader.exister != nil && loader.exister.Exists(doc.Key.ID)) {
			return nil
		}
		if template.OnLoad == nil {
			return nil
		}
		if err := template.OnLoad(doc); err != nil {
			if template.Strict {
				return fmt.Errorf("%w: resource=%s id=%d: %v", ErrLoadCallback, template.Resource, doc.Key.ID, err)
			}
			// 非严格模式只容忍个别坏行，不能静默：字段改名、编码变化这类系统性
			// 失败会让整批实体都加载不出来，必须经指标和日志可见。
			metrics.IncCounter("dataengine.load.skipped.total", metrics.Labels{"resource": template.Resource}, 1)
			slog.Warn("dataengine: skipped a row a non-strict load template could not decode",
				"resource", template.Resource, "id", doc.Key.ID, "err", err)
		}
		return nil
	})
}

func topoSort(templates []LoadTemplate) ([]int, error) {
	nameToIndex := make(map[string]int, len(templates))
	for index, template := range templates {
		if template.Resource == "" {
			return nil, fmt.Errorf("%w: template %d has empty resource", ErrLoadDependency, index)
		}
		if _, duplicate := nameToIndex[template.Resource]; duplicate {
			return nil, fmt.Errorf("%w: duplicate resource %q", ErrLoadDependency, template.Resource)
		}
		nameToIndex[template.Resource] = index
	}
	inDegree := make([]int, len(templates))
	adjacent := make([][]int, len(templates))
	for index, template := range templates {
		for _, dependency := range template.DependsOn {
			dependencyIndex, ok := nameToIndex[dependency]
			if !ok {
				return nil, fmt.Errorf("%w: resource %q depends on unknown resource %q", ErrLoadDependency, template.Resource, dependency)
			}
			adjacent[dependencyIndex] = append(adjacent[dependencyIndex], index)
			inDegree[index]++
		}
	}
	queue := make([]int, 0, len(templates))
	for index, degree := range inDegree {
		if degree == 0 {
			queue = append(queue, index)
		}
	}
	order := make([]int, 0, len(templates))
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		order = append(order, current)
		for _, next := range adjacent[current] {
			inDegree[next]--
			if inDegree[next] == 0 {
				queue = append(queue, next)
			}
		}
	}
	if len(order) != len(templates) {
		return nil, fmt.Errorf("%w: circular dependency", ErrLoadDependency)
	}
	return order, nil
}

func buildLevels(order []int, templates []LoadTemplate) [][]int {
	nameToIndex := make(map[string]int, len(templates))
	for index, template := range templates {
		nameToIndex[template.Resource] = index
	}
	levelsByIndex := make([]int, len(templates))
	maxLevel := 0
	for _, index := range order {
		for _, dependency := range templates[index].DependsOn {
			levelsByIndex[index] = max(levelsByIndex[index], levelsByIndex[nameToIndex[dependency]]+1)
		}
		maxLevel = max(maxLevel, levelsByIndex[index])
	}
	levels := make([][]int, maxLevel+1)
	for index, level := range levelsByIndex {
		levels[level] = append(levels[level], index)
	}
	return levels
}
