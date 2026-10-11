package policy

import (
	"math/rand/v2"
	"reflect"
	"sync"
	"testing"
)

func TestRelationDeltaMatchesFullReplacement(t *testing.T) {
	actual, reference := NewRelationSource("team"), NewRelationSource("team")
	rng := rand.New(rand.NewPCG(71, 89))
	model := map[int64]map[int64]struct{}{}
	for step := range 1200 {
		observer := int64(rng.IntN(6) - 2)
		added, removed := make([]int64, rng.IntN(12)), make([]int64, rng.IntN(12))
		for i := range added {
			added[i] = int64(rng.IntN(16) - 7)
		}
		for i := range removed {
			removed[i] = int64(rng.IntN(16) - 7)
		}
		if observer != 0 {
			if model[observer] == nil {
				model[observer] = map[int64]struct{}{}
			}
			for _, id := range added {
				if id != 0 {
					model[observer][id] = struct{}{}
				}
			}
			for _, id := range removed {
				delete(model[observer], id)
			}
		}
		ids := []int64{}
		for id := range model[observer] {
			ids = append(ids, id)
		}
		reference.Set(observer, ids)
		actual.ApplyDelta(observer, added, removed)
		if !reflect.DeepEqual(actual.members, reference.members) || !reflect.DeepEqual(actual.observers, reference.observers) {
			t.Fatalf("step %d indexes differ", step)
		}
		if a, r := actual.Flush(), reference.Flush(); !reflect.DeepEqual(a, r) {
			t.Fatalf("step %d events differ: %v / %v", step, a, r)
		}
		// 交错全量替换与退出后重建，检查增量不持有过期成员表。
		if step%13 == 0 {
			actual.Clear(observer)
			reference.Clear(observer)
			delete(model, observer)
			actual.Flush()
			reference.Flush()
		}
	}
}

func TestRelationForgetAfterReplacementClearAndReadd(t *testing.T) {
	s := NewRelationSource("friends")
	s.Set(1, []int64{0, 1, 10, 10, 20})
	s.Set(2, []int64{10, 20})
	s.Flush()
	s.Set(1, []int64{1, 20})
	s.Clear(2)
	s.Flush()
	s.Forget(10)
	if events := s.Flush(); len(events) != 0 {
		t.Fatalf("stale reverse relationship: %v", events)
	}
	s.Set(2, []int64{20})
	s.Flush()
	s.Forget(20)
	events := s.Flush()
	if len(events) != 2 || events[0].Observer != 1 || events[1].Observer != 2 || events[0].Kind != InterestLeave || events[1].Kind != InterestLeave {
		t.Fatal(events)
	}
	s.Forget(20)
	if len(s.Flush()) != 0 {
		t.Fatal("duplicate leave")
	}
	s.Forget(1)
	events = s.Flush()
	if len(events) != 1 || events[0].Subject != 1 {
		t.Fatal(events)
	}
	s.Set(1, []int64{20})
	events = s.Flush()
	if len(events) != 1 || events[0].Kind != InterestEnter {
		t.Fatal(events)
	}
}

// 独立全量集合模型核对重复 ID、清空、退出和重建，不依赖正式实现的临时表。
func TestRelationSetMatchesMembershipModel(t *testing.T) {
	s := NewRelationSource("friends")
	model := map[int64]map[int64]struct{}{}
	rng := rand.New(rand.NewPCG(31, 57))
	var want []InterestEvent
	for step := range 1500 {
		observer := int64(rng.IntN(8) - 3)
		subject := int64(rng.IntN(16) - 7)
		if step%7 == 0 {
			s.Forget(subject)
			for oid, members := range model {
				if _, ok := members[subject]; ok {
					delete(members, subject)
					want = append(want, InterestEvent{Observer: oid, Subject: subject, Kind: InterestLeave, Band: -1})
					if len(members) == 0 {
						delete(model, oid)
					}
				}
			}
		} else {
			ids := make([]int64, rng.IntN(24))
			for i := range ids {
				ids[i] = int64(rng.IntN(16) - 7)
			}
			s.Set(observer, ids)
			if observer != 0 {
				next := map[int64]struct{}{}
				for _, id := range ids {
					if id != 0 {
						next[id] = struct{}{}
					}
				}
				for id := range model[observer] {
					if _, ok := next[id]; !ok {
						want = append(want, InterestEvent{Observer: observer, Subject: id, Kind: InterestLeave, Band: -1})
					}
				}
				for id := range next {
					if _, ok := model[observer][id]; !ok {
						want = append(want, InterestEvent{Observer: observer, Subject: id, Kind: InterestEnter})
					}
				}
				if len(next) == 0 {
					delete(model, observer)
				} else {
					model[observer] = next
				}
			}
			// 调用返回后修改输入，不得污染正式集合或下次工作区。
			for i := range ids {
				ids[i] = 999
			}
		}
		if !reflect.DeepEqual(s.members, model) {
			t.Fatalf("step %d: membership mismatch", step)
		}
		for id, observers := range s.observers {
			for oid := range observers {
				if _, ok := model[oid][id]; !ok {
					t.Fatalf("stale reverse relation %d/%d", oid, id)
				}
			}
		}
		for oid, members := range model {
			for id := range members {
				if _, ok := s.observers[id][oid]; !ok {
					t.Fatalf("missing reverse relation %d/%d", oid, id)
				}
			}
		}
		if step%11 == 0 {
			sortInterestEvents(want)
			if got := s.Flush(); !reflect.DeepEqual(got, want) {
				t.Fatalf("step %d: events mismatch", step)
			}
			want = nil
		}
	}
	sortInterestEvents(want)
	if got := s.Flush(); !reflect.DeepEqual(got, want) {
		t.Fatal("final events mismatch")
	}
}

func TestRelationSetWorkspaceConcurrentLifecycle(t *testing.T) {
	s := NewRelationSource("team")
	var wg sync.WaitGroup
	for worker := range 8 {
		wg.Go(func() {
			id := int64(worker + 1)
			for range 100 {
				s.Set(id, []int64{id, id, 0})
				s.Set(id, []int64{id, id})
				s.ApplyDelta(id, []int64{-id, -id}, []int64{id})
				s.ApplyDelta(id, []int64{id}, []int64{-id})
				s.Forget(id)
				s.Clear(id)
				s.Flush()
			}
		})
	}
	wg.Wait()
	if len(s.members) != 0 || len(s.observers) != 0 {
		t.Fatal("relationships remain after clear")
	}
	// 超过复用上限的大表替换成小表，再次重复和清空，不得与 scratch 共享。
	large := make([]int64, relationScratchLimit+1)
	for i := range large {
		large[i] = int64(i + 1)
	}
	s.Set(1, large)
	s.Set(1, []int64{-1})
	s.Set(1, []int64{-1, -1})
	s.Clear(1)
	if len(s.members) != 0 || len(s.observers) != 0 {
		t.Fatal("large replacement leaked relations")
	}
}
