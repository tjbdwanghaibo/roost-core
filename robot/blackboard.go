package robot

import (
	"fmt"
	"strconv"
	"sync"
)

// Blackboard is the small shared memory a scenario can use between actions.
// Prefer the typed Key[T] accessors over raw string keys.
type Blackboard struct {
	mu   sync.RWMutex
	data map[string]any
}

func NewBlackboard() *Blackboard {
	return &Blackboard{data: make(map[string]any)}
}

func (b *Blackboard) Set(key string, value any) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	b.data[key] = value
	b.mu.Unlock()
}

func (b *Blackboard) Get(key string) (any, bool) {
	if b == nil || key == "" {
		return nil, false
	}
	b.mu.RLock()
	v, ok := b.data[key]
	b.mu.RUnlock()
	return v, ok
}

func (b *Blackboard) Delete(key string) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	delete(b.data, key)
	b.mu.Unlock()
}

func (b *Blackboard) Snapshot() map[string]any {
	out := make(map[string]any)
	if b == nil {
		return out
	}
	b.mu.RLock()
	for k, v := range b.data {
		out[k] = v
	}
	b.mu.RUnlock()
	return out
}

func (b *Blackboard) String(key string) (string, bool) {
	v, ok := b.Get(key)
	if !ok || v == nil {
		return "", false
	}
	switch typed := v.(type) {
	case string:
		return typed, true
	case fmt.Stringer:
		return typed.String(), true
	default:
		return fmt.Sprint(typed), true
	}
}

func (b *Blackboard) Int64(key string) (int64, bool) {
	v, ok := b.Get(key)
	if !ok || v == nil {
		return 0, false
	}
	switch typed := v.(type) {
	case int:
		return int64(typed), true
	case int8:
		return int64(typed), true
	case int16:
		return int64(typed), true
	case int32:
		return int64(typed), true
	case int64:
		return typed, true
	case uint:
		return int64(typed), true
	case uint8:
		return int64(typed), true
	case uint16:
		return int64(typed), true
	case uint32:
		return int64(typed), true
	case uint64:
		return int64(typed), true
	case float32:
		return int64(typed), true
	case float64:
		return int64(typed), true
	case string:
		n, err := strconv.ParseInt(typed, 10, 64)
		return n, err == nil
	default:
		return 0, false
	}
}
