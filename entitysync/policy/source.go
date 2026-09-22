// Package policy holds the ways subscriptions get organized on top of an
// entitysync.Manager: by distance (Interest), by membership in a group
// (Room), or one binding at a time (Direct). None of them touch the wire or
// the frames — a policy only decides WHO receives WHOM and says so to the
// manager. The manager, in turn, knows nothing about any of them (ARCH-10).
package policy

import (
	"sort"
	"sync"

	"github.com/tjbdwanghaibo/roost-core/spatial"
)

// Source is one reason an observer should receive a subject's state. Space is
// one reason; being in the same team, being on a friends list, being yourself
// are others. They all answer the same question, so they all produce the same
// events and Interest's aggregation does not care which is which.
//
// The event type is spatial's: a relation simply never emits BandChanged and
// always uses band 0.
type Source interface {
	// Name identifies the source in the aggregator's bookkeeping. Two sources
	// must not share a name.
	Name() string
	// Flush drains the changes since the last call.
	Flush() []spatial.InterestEvent
}

// RelationSource is a set-driven Source: whoever owns a relationship pushes
// the membership in, and the source turns the difference into Enter/Leave.
//
// It is the same shape for every relationship a game has — a team, a friends
// list, an alliance are the same type with a different feed, which is why it
// is not called TeamSource. Band 0 always: a relationship has no distance, so
// it asks for the best fidelity the aggregator will give.
type RelationSource struct {
	name string

	mu      sync.Mutex
	members map[int64]map[int64]struct{} // observer -> subjects
	pending []spatial.InterestEvent
}

func NewRelationSource(name string) *RelationSource {
	return &RelationSource{name: name, members: make(map[int64]map[int64]struct{})}
}

func (s *RelationSource) Name() string { return s.name }

// Set replaces an observer's set. The diff is computed here rather than by the
// caller: a caller that had to compute it would need to remember the previous
// set, and then there would be two copies of the same fact.
func (s *RelationSource) Set(observer int64, subjects []int64) {
	if observer == 0 {
		return
	}
	next := make(map[int64]struct{}, len(subjects))
	for _, subject := range subjects {
		// A relation to oneself is legitimate here — "self" is exactly the
		// degenerate relationship — so unlike the spatial source this does
		// not exclude observer == subject.
		if subject != 0 {
			next[subject] = struct{}{}
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	previous := s.members[observer]
	for subject := range previous {
		if _, kept := next[subject]; !kept {
			s.pending = append(s.pending, spatial.InterestEvent{Observer: observer, Subject: subject, Kind: spatial.InterestLeave, Band: -1})
		}
	}
	for subject := range next {
		if _, had := previous[subject]; !had {
			s.pending = append(s.pending, spatial.InterestEvent{Observer: observer, Subject: subject, Kind: spatial.InterestEnter})
		}
	}
	if len(next) == 0 {
		delete(s.members, observer)
		return
	}
	s.members[observer] = next
}

// Clear drops an observer and leaves everything it held.
func (s *RelationSource) Clear(observer int64) { s.Set(observer, nil) }

// Forget removes a subject from every observer's set. It is what a departing
// player needs: the relation is gone in both directions, and the observers
// that held it must be told.
func (s *RelationSource) Forget(subject int64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for observer, subjects := range s.members {
		if _, held := subjects[subject]; !held {
			continue
		}
		delete(subjects, subject)
		s.pending = append(s.pending, spatial.InterestEvent{Observer: observer, Subject: subject, Kind: spatial.InterestLeave, Band: -1})
		if len(subjects) == 0 {
			delete(s.members, observer)
		}
	}
}

// Flush drains the events, ordered like the spatial source's so the aggregator
// sees one deterministic sequence whatever the sources' internal iteration
// order was.
func (s *RelationSource) Flush() []spatial.InterestEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return nil
	}
	events := s.pending
	s.pending = nil
	sort.SliceStable(events, func(i, j int) bool {
		if events[i].Observer != events[j].Observer {
			return events[i].Observer < events[j].Observer
		}
		return events[i].Subject < events[j].Subject
	})
	return events
}

var _ Source = (*RelationSource)(nil)
