package state

import (
	"errors"
	"fmt"
	"slices"
	"testing"
)

// 测试用状态：按来源的 Rooted / Silenced（同步）、持久的 Protected、派生的 Stunned（同步）。
const (
	Rooted    ID = 1
	Silenced  ID = 2
	Protected ID = 3
	Stunned   ID = 4
	Manual    ID = 5
)

var (
	buff1  = Source{Kind: 1, ID: 1}
	buff2  = Source{Kind: 1, ID: 2}
	action = Source{Kind: 2, ID: 7}
)

func testCatalog(t testing.TB) *Catalog {
	t.Helper()
	c, err := NewCatalog(
		Def{ID: Rooted, Name: "rooted", Sync: true},
		Def{ID: Silenced, Name: "silenced", Sync: true},
		Def{ID: Protected, Name: "protected", Sync: true, Persist: true},
		Def{ID: Stunned, Name: "stunned", Sync: true},
		Def{ID: Manual, Name: "manual"},
	)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func newTestSet(t testing.TB, reported *[]error) *Set {
	t.Helper()
	return NewSet(NewStateDao(1, "game", testCatalog(t)), SetConfig{OnError: func(err error) {
		if reported != nil {
			*reported = append(*reported, err)
		}
	}})
}

func TestCatalogRefusesBadDeclarations(t *testing.T) {
	for _, defs := range [][]Def{
		{{ID: 0, Name: "zero"}},
		{{ID: 1}},
		{{ID: 1, Name: "a"}, {ID: 1, Name: "b"}},
		{{ID: 1, Name: "a"}, {ID: 2, Name: "a"}},
	} {
		if _, err := NewCatalog(defs...); !errors.Is(err, ErrCatalogInvalid) {
			t.Errorf("NewCatalog(%+v) = %v, want ErrCatalogInvalid", defs, err)
		}
	}
	c := testCatalog(t)
	if id, ok := c.Lookup("protected"); !ok || id != Protected || fmt.Sprint(c.IDs()) != "[1 2 3 4 5]" {
		t.Fatalf("lookup %d %v ids %v", id, ok, c.IDs())
	}
}

// 按来源：有任一来源即生效；同一来源重复加不叠加，撤掉不存在的来源无效——不会出现匿名计数“多减一次就
// 错位”；RemoveSource 从全部状态撤掉一个来源。
func TestASourcedStateHoldsWhileAnySourceRemains(t *testing.T) {
	s := newTestSet(t, nil)
	for _, step := range []func() error{
		func() error { return s.Add(Rooted, buff1) },
		func() error { return s.Add(Rooted, buff1) },
		func() error { return s.Add(Rooted, buff2) },
		func() error { return s.Add(Silenced, buff1) },
		func() error { return s.Remove(Rooted, action) },
		func() error { return s.Remove(Rooted, buff2) },
	} {
		if err := step(); err != nil {
			t.Fatal(err)
		}
	}
	if !s.Has(Rooted) || fmt.Sprint(s.Sources(Rooted)) != "[{1 1}]" {
		t.Fatalf("rooted %v sources %v, want only buff1 left", s.Has(Rooted), s.Sources(Rooted))
	}
	removed, err := s.RemoveSource(buff1)
	if err != nil || fmt.Sprint(removed) != "[1 2]" || s.Has(Rooted) || s.Has(Silenced) {
		t.Fatalf("RemoveSource: %v %v rooted %v silenced %v", removed, err, s.Has(Rooted), s.Has(Silenced))
	}
	if err := s.Add(99, buff1); !errors.Is(err, ErrUnknownState) {
		t.Fatalf("unknown state: %v", err)
	}
}

// 派生：查询时现算、不存储；Changed 只在值真的变了时通知；同一状态也可以同时有来源（任一为真即生效）。
func TestADerivedStateIsComputedAndNotifiedOnlyWhenItChanges(t *testing.T) {
	s := newTestSet(t, nil)
	stunTag := false
	if err := s.Provide(Stunned, func() bool { return stunTag }); err != nil {
		t.Fatal(err)
	}
	var seen []string
	if _, err := s.Watch(Stunned, func(c Change) { seen = append(seen, fmt.Sprint("first ", c.Active)) }); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Watch(Stunned, func(c Change) { seen = append(seen, fmt.Sprint("second ", c.Active)) }); err != nil {
		t.Fatal(err)
	}
	s.Changed(Stunned) // 没变
	stunTag = true
	s.Changed(Stunned)
	s.Changed(Stunned) // 仍为真，不再通知
	if err := s.Add(Stunned, action); err != nil {
		t.Fatal(err)
	}
	stunTag = false
	s.Changed(Stunned) // 还有来源，仍为真
	if err := s.Remove(Stunned, action); err != nil {
		t.Fatal(err)
	}
	want := []string{"first true", "second true", "first false", "second false"}
	if !slices.Equal(seen, want) {
		t.Fatalf("watchers saw %v, want %v", seen, want)
	}
	if len(s.Dao().sources) != 0 {
		t.Fatalf("a derived state was stored: %v", s.Dao().sources)
	}
	for _, bad := range []struct {
		id ID
		fn func() bool
	}{{Stunned, func() bool { return true }}, {Protected, func() bool { return true }}, {Rooted, nil}, {99, func() bool { return true }}} {
		if err := s.Provide(bad.id, bad.fn); err == nil {
			t.Errorf("Provide(%d) accepted", bad.id)
		}
	}
}

// 观察者里改状态立即生效，引出的通知排在本轮之后；观察者看到的总是交替的值。互相触发超过上限截停并报告。
func TestWatcherChangesAreNotifiedInTheNextRoundAndRunawayIsBounded(t *testing.T) {
	var reported []error
	s := newTestSet(t, &reported)
	var seen []string
	_, _ = s.Watch(Rooted, func(c Change) {
		seen = append(seen, fmt.Sprint("rooted ", c.Active, " manual ", s.Has(Manual)))
		if c.Active {
			if err := s.Add(Manual, action); err != nil {
				t.Error(err)
			}
			seen = append(seen, fmt.Sprint("manual set inside: ", s.Has(Manual)))
		}
	})
	_, _ = s.Watch(Manual, func(c Change) { seen = append(seen, fmt.Sprint("manual ", c.Active)) })
	if err := s.Add(Rooted, buff1); err != nil {
		t.Fatal(err)
	}
	want := []string{"rooted true manual false", "manual set inside: true", "manual true"}
	if !slices.Equal(seen, want) {
		t.Fatalf("saw %v, want %v", seen, want)
	}

	// 两个观察者互相翻转：截停。
	_, _ = s.Watch(Silenced, func(c Change) { _ = s.Remove(Silenced, buff2); _ = s.Add(Silenced, buff2) })
	_, _ = s.Watch(Silenced, func(c Change) {
		if c.Active {
			_ = s.Remove(Silenced, buff2)
		} else {
			_ = s.Add(Silenced, buff2)
		}
	})
	if err := s.Add(Silenced, buff2); err != nil {
		t.Fatal(err)
	}
	if len(reported) != 1 || !errors.Is(reported[0], ErrNotifyRunaway) {
		t.Fatalf("reported %v, want one ErrNotifyRunaway", reported)
	}
}

func TestQueriesAndWatcherBundles(t *testing.T) {
	s := newTestSet(t, nil)
	_ = s.Add(Rooted, buff1)
	_ = s.Add(Manual, action)
	if !s.Any(Silenced, Rooted) || s.Any(Silenced, Stunned) {
		t.Fatal("Any")
	}
	if !s.Satisfies(Requirement{All: []ID{Rooted, Manual}, None: []ID{Silenced}}) || s.Satisfies(Requirement{None: []ID{Manual}}) {
		t.Fatal("Satisfies")
	}
	if fmt.Sprint(s.Active()) != "[1 5]" || fmt.Sprint(s.Synced()) != "[1]" {
		t.Fatalf("active %v synced %v", s.Active(), s.Synced())
	}
	calls := 0
	w := s.NewWatcher()
	_ = w.Watch(Rooted, func(Change) { calls++ })
	_ = w.Watch(Manual, func(Change) { calls++ })
	w.Close()
	w.Close()
	_ = s.Remove(Rooted, buff1)
	_ = s.Remove(Manual, action)
	if calls != 0 || len(s.watches) != 0 || len(s.seen) != 0 {
		t.Fatalf("closed watcher still called %d times (watches %d seen %v)", calls, len(s.watches), s.seen)
	}
}

// 持久状态不能在事务外改：改动无法落库，拒绝而不是静默丢失。不持久的可以（加载 / 新建的初始化阶段）。
func TestAPersistentStateCannotChangeOutsideATransaction(t *testing.T) {
	s := newTestSet(t, nil)
	if err := s.Add(Protected, action); !errors.Is(err, ErrNotInTransaction) || s.Has(Protected) {
		t.Fatalf("persistent add outside a transaction: %v has %v", err, s.Has(Protected))
	}
	if err := s.Add(Rooted, action); err != nil || !s.Has(Rooted) {
		t.Fatalf("memory add outside a transaction: %v", err)
	}
}

// 持久文档只含声明了 Persist 的状态，键是状态名；加载时跳过不认识的名字与不再持久的状态。
func TestTheDaoPersistsOnlyPersistentStatesByName(t *testing.T) {
	dao := NewStateDao(42, "game", testCatalog(t))
	dao.sources[Protected] = []Source{action}
	dao.sources[Rooted] = []Source{buff1}
	payload, version, err := dao.MarshalPersisted()
	if err != nil {
		t.Fatal(err)
	}
	restored := NewStateDao(42, "game", testCatalog(t))
	if err := restored.RestorePersisted(payload, version, 3); err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(restored.sources) != "map[3:[{2 7}]]" || restored.tracker.Version() != 3 {
		t.Fatalf("restored %v version %d", restored.sources, restored.tracker.Version())
	}
	// 声明变了：protected 不再持久、多了一个不认识的名字——都被跳过。
	renamed, err := NewCatalog(Def{ID: Protected, Name: "protected"}, Def{ID: Rooted, Name: "rooted", Persist: true})
	if err != nil {
		t.Fatal(err)
	}
	full := NewStateDao(42, "game", renamed)
	if err := full.RestorePersisted(payload, version, 3); err != nil || len(full.sources) != 0 {
		t.Fatalf("restore under a changed catalog: %v %v", err, full.sources)
	}
	if err := restored.RestorePersisted(payload, version+1, 3); err == nil {
		t.Fatal("a newer schema version was accepted")
	}
}
