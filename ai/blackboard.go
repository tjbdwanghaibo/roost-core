package ai

import "sync"

type Blackboard struct {
	mu     sync.RWMutex
	values map[string]any
}

func NewBlackboard() *Blackboard { return &Blackboard{values: make(map[string]any)} }
func (b *Blackboard) Set(key string, value any) {
	if b == nil || key == "" {
		return
	}
	b.mu.Lock()
	if b.values == nil {
		b.values = make(map[string]any)
	}
	b.values[key] = value
	b.mu.Unlock()
}
func (b *Blackboard) Get(key string) (any, bool) {
	if b == nil {
		return nil, false
	}
	b.mu.RLock()
	value, ok := b.values[key]
	b.mu.RUnlock()
	return value, ok
}
func (b *Blackboard) Delete(key string) {
	if b == nil {
		return
	}
	b.mu.Lock()
	delete(b.values, key)
	b.mu.Unlock()
}
func (b *Blackboard) Snapshot() map[string]any {
	if b == nil {
		return nil
	}
	b.mu.RLock()
	out := make(map[string]any, len(b.values))
	for key, value := range b.values {
		out[key] = value
	}
	b.mu.RUnlock()
	return out
}
