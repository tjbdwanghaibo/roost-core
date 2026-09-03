package dataengine

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/metrics"
)

type loaderStore struct {
	docs []RawDocument
}

func (*loaderStore) ReadConsistent(ctx context.Context, read func(context.Context) error) error {
	return read(ctx)
}

func (store *loaderStore) Load(ctx context.Context, spec LoadSpec) ([]RawDocument, error) {
	docs := make([]RawDocument, 0, len(store.docs))
	err := store.StreamLoad(ctx, spec, func(doc RawDocument) error {
		docs = append(docs, doc)
		return nil
	})
	return docs, err
}

func (store *loaderStore) StreamLoad(_ context.Context, _ LoadSpec, consume func(RawDocument) error) error {
	for _, doc := range store.docs {
		if err := consume(doc); err != nil {
			return err
		}
	}
	return nil
}

type loaderExister map[int64]bool

func (values loaderExister) Exists(id int64) bool { return values[id] }

func TestLoaderRespectsDependenciesAndSkipsLoadedOrDeletedDocuments(t *testing.T) {
	store := &loaderStore{docs: []RawDocument{
		{Key: DocumentKey{ID: 1}}, {Key: DocumentKey{ID: 2}}, {Key: DocumentKey{ID: 3}, Deleted: true},
	}}
	var mu sync.Mutex
	var order []string
	load := func(name string) func(RawDocument) error {
		return func(RawDocument) error {
			mu.Lock()
			order = append(order, name)
			mu.Unlock()
			return nil
		}
	}
	err := NewLoader(store, loaderExister{2: true}).LoadAll(context.Background(), []LoadTemplate{
		{Resource: "guilds", DependsOn: []string{"players"}, OnLoad: load("guilds")},
		{Resource: "players", OnLoad: load("players")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(order, []string{"players", "guilds"}) {
		t.Fatalf("order=%v", order)
	}
}

func TestLoaderRejectsUnknownCircularAndStrictCallbackFailures(t *testing.T) {
	store := &loaderStore{docs: []RawDocument{{Key: DocumentKey{ID: 1}}}}
	tests := []struct {
		name      string
		templates []LoadTemplate
		contains  error
	}{
		{name: "unknown", templates: []LoadTemplate{{Resource: "players", DependsOn: []string{"missing"}}}, contains: ErrLoadDependency},
		{name: "cycle", templates: []LoadTemplate{{Resource: "a", DependsOn: []string{"b"}}, {Resource: "b", DependsOn: []string{"a"}}}, contains: ErrLoadDependency},
		{name: "strict", templates: []LoadTemplate{{Resource: "players", Strict: true, OnLoad: func(RawDocument) error { return errors.New("decode") }}}, contains: ErrLoadCallback},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := NewLoader(store, nil).LoadAll(context.Background(), test.templates)
			if !errors.Is(err, test.contains) {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

// counterValue reads one counter series without needing a package-level
// reset, so the assertion is a delta and stays correct however the rest of
// the suite exercises the same metric.
func counterValue(name string, labels metrics.Labels) int64 {
	for _, metric := range metrics.Snapshot() {
		if metric.Name != name || metric.Kind != metrics.KindCounter {
			continue
		}
		match := len(metric.Labels) == len(labels)
		for key, want := range labels {
			if metric.Labels[key] != want {
				match = false
				break
			}
		}
		if match {
			return metric.Value
		}
	}
	return 0
}

// A non-strict template tolerates a bad row, but it must not do so invisibly:
// a systematic decode failure otherwise loads zero entities and reports
// nothing at all, which is the one outcome the framework must never hide.
func TestLoaderNonStrictSkipIsCountedAndStrictStillFails(t *testing.T) {
	broken := errors.New("cannot decode row")
	labels := metrics.Labels{"resource": "players"}
	before := counterValue("dataengine.load.skipped.total", labels)
	store := &loaderStore{docs: []RawDocument{
		{Key: DocumentKey{Resource: "players", ID: 1}, Version: 1},
		{Key: DocumentKey{Resource: "players", ID: 2}, Version: 1},
	}}

	loaded := 0
	if err := NewLoader(store, nil).LoadAll(context.Background(), []LoadTemplate{{
		Resource: "players", OnLoad: func(RawDocument) error { loaded++; return broken },
	}}); err != nil {
		t.Fatalf("non-strict load must not fail: %v", err)
	}
	if loaded != len(store.docs) {
		t.Fatalf("callback invocations=%d, want %d", loaded, len(store.docs))
	}
	if delta := counterValue("dataengine.load.skipped.total", labels) - before; delta != int64(len(store.docs)) {
		t.Fatalf("skipped counter delta=%d, want %d", delta, len(store.docs))
	}

	// Strict still fails loudly, and stops at the first bad row.
	strictCalls := 0
	err := NewLoader(store, nil).LoadAll(context.Background(), []LoadTemplate{{
		Resource: "players", Strict: true,
		OnLoad: func(RawDocument) error { strictCalls++; return broken },
	}})
	if !errors.Is(err, ErrLoadCallback) {
		t.Fatalf("strict err=%v, want ErrLoadCallback", err)
	}
	if strictCalls != 1 {
		t.Fatalf("strict template kept going after a bad row: calls=%d", strictCalls)
	}
}
