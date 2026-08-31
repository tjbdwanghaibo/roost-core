package dataengine

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
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
