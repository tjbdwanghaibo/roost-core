package policy

import "testing"

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
