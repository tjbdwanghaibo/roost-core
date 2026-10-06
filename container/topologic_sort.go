package container

import (
	"log/slog"
	"slices"
	"sync"
)

func NewTopologicalSortCache[T comparable]() *TopologicalSortCache[T] {
	return &TopologicalSortCache[T]{
		dependencies: make(map[T][]T),
	}
}

type TopologicalSortCache[T comparable] struct {
	cache        []T
	cacheLock    sync.RWMutex
	isCacheValid bool

	dependLock   sync.RWMutex
	dependencies map[T][]T
}

func (t *TopologicalSortCache[T]) RegisterCompDependency(tT T, dependencies ...T) {
	t.dependLock.Lock()
	defer t.dependLock.Unlock()

	if _, exists := t.dependencies[tT]; exists {
		slog.Error("dependency already registered", "key", tT)
		return
	}

	// 复制调用方的切片：`deps...` 传入的就是调用方的数组，之后调用方复用它会改掉登记的依赖，
	// 且不使排序缓存失效（RR-20261005-NC-268）。
	t.dependencies[tT] = slices.Clone(dependencies)
	t.invalidateTopologicalSortCache()
}

func (t *TopologicalSortCache[T]) GetTopologicalSortedComponents() []T {
	t.cacheLock.RLock()
	if t.isCacheValid && t.cache != nil {
		cached := make([]T, len(t.cache))
		copy(cached, t.cache)
		t.cacheLock.RUnlock()
		return cached
	}
	t.cacheLock.RUnlock()

	t.dependLock.RLock()
	defer t.dependLock.RUnlock()

	allComponents := make(map[T]bool)
	for compType := range t.dependencies {
		allComponents[compType] = true
	}

	inDegree := make(map[T]int)
	graph := make(map[T][]T)

	for compType := range allComponents {
		inDegree[compType] = 0
		graph[compType] = []T{}
	}

	for compType, deps := range t.dependencies {
		for _, dep := range deps {
			if _, exists := inDegree[dep]; !exists {
				inDegree[dep] = 0
			}
			inDegree[dep]++
			graph[compType] = append(graph[compType], dep)
		}
	}

	var queue []T
	for compType, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, compType)
		}
	}

	var sorted []T
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		sorted = append(sorted, current)

		for _, neighbor := range graph[current] {
			inDegree[neighbor]--
			if inDegree[neighbor] == 0 {
				queue = append(queue, neighbor)
			}
		}
	}

	// 与参与排序的全部节点比较，而不是只与注册过的键比较（RR-20261005-NC-184）：
	// 只作为依赖出现、自己没注册的节点也在 inDegree 里、也会进 sorted。之前
	// A→B（B 未注册）被误报成环；A↔B 成环再加 C→X,Y 时两边长度恰好相等，
	// 环被藏起来、A 与 B 被静默丢掉。
	if len(sorted) != len(inDegree) {
		slog.Error("circular dependency detected in topological sort")
		return nil
	}

	// Reverse: most-depended-on first
	for i, j := 0, len(sorted)-1; i < j; i, j = i+1, j-1 {
		sorted[i], sorted[j] = sorted[j], sorted[i]
	}

	t.cacheLock.Lock()
	t.cache = make([]T, len(sorted))
	copy(t.cache, sorted)
	t.isCacheValid = true
	t.cacheLock.Unlock()

	result := make([]T, len(sorted))
	copy(result, sorted)
	return result
}

func (t *TopologicalSortCache[T]) GetCompDependencies(componentType T) []T {
	t.dependLock.RLock()
	defer t.dependLock.RUnlock()

	if deps, exists := t.dependencies[componentType]; exists {
		return slices.Clone(deps) // 返回副本，调用方改动不影响登记（RR-20261005-NC-268）
	}
	return nil
}

func (t *TopologicalSortCache[T]) invalidateTopologicalSortCache() {
	t.cacheLock.Lock()
	defer t.cacheLock.Unlock()
	t.isCacheValid = false
	t.cache = nil
}
