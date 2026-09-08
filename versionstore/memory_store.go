package versionstore

import (
	"context"
	"fmt"
	"sync"
)

// MemoryStore is a versioned store backed by a map. It exists so a service can
// be tested and run locally without Redis, and so the contract has a second
// implementation — a contract with one implementation tends to grow that
// implementation's accidents.
//
// It is NOT an alternative source of truth for production: it is process-local,
// so two replicas would each have their own. Wire it in tests and in
// single-process debugging only.
type MemoryStore[K comparable, T any] struct {
	mu          sync.Mutex
	items       map[K]Versioned[T]
	maxAttempts int
}

func NewMemoryStore[K comparable, T any]() *MemoryStore[K, T] {
	return &MemoryStore[K, T]{items: make(map[K]Versioned[T]), maxAttempts: DefaultMaxAttempts}
}

func (s *MemoryStore[K, T]) Get(_ context.Context, key K) (Versioned[T], bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, ok := s.items[key]
	return value, ok, nil
}

// Update holds the lock across mutate, so the compare and the write are
// atomic and the retry loop is never entered. mutate is still documented as
// callable more than once, because the Redis implementation does retry — a
// caller must not depend on the memory store's stronger guarantee.
func (s *MemoryStore[K, T]) Update(_ context.Context, key K, mutate Mutate[T]) (Versioned[T], bool, error) {
	if mutate == nil {
		return Versioned[T]{}, false, fmt.Errorf("versionstore: mutate is nil")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	current, found := s.items[key]
	next, save, err := mutate(current.Value, found)
	if err != nil {
		return Versioned[T]{}, false, err
	}
	if !save {
		return current, false, nil
	}
	stored := Versioned[T]{Value: next, Version: current.Version + 1}
	s.items[key] = stored
	return stored, true, nil
}

func (s *MemoryStore[K, T]) Create(_ context.Context, key K, value T) (Versioned[T], bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.items[key]; exists {
		return Versioned[T]{}, false, nil
	}
	stored := Versioned[T]{Value: value, Version: 1}
	s.items[key] = stored
	return stored, true, nil
}

func (s *MemoryStore[K, T]) Delete(_ context.Context, key K, expect Versioned[T]) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	current, found := s.items[key]
	if !found {
		if expect.Version == 0 {
			return nil
		}
		return fmt.Errorf("%w: key is absent, caller held version %d", ErrVersionMismatch, expect.Version)
	}
	if current.Version != expect.Version {
		return fmt.Errorf("%w: stored version %d, caller held %d", ErrVersionMismatch, current.Version, expect.Version)
	}
	delete(s.items, key)
	return nil
}

// Len reports how many keys are stored. Test helper.
func (s *MemoryStore[K, T]) Len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.items)
}

var _ Store[string, int] = (*MemoryStore[string, int])(nil)
