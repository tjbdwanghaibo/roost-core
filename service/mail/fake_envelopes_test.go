package mail

import (
	"context"
	"sync"
)

// fakeEnvelopes is an EnvelopeStore for tests. It lives here rather than in
// the production package because constraint 2 says so: a production package
// that ships an in-memory source of truth ships a configuration in which
// nothing is durable and every test passes.
//
// It is deliberately strict about the one thing the contract promises and the
// implementation being replaced got wrong: Create never overwrites.
type fakeEnvelopes struct {
	mu    sync.Mutex
	items map[string]Envelope
	// getManyCalls and getCalls let a test assert on the number of round
	// trips, not just the number of results. The defect this package answers
	// was unbounded round trips behind a bounded result, so "how many reads"
	// has to be observable.
	getManyCalls int
	getCalls     int
	failGetMany  error
}

func newFakeEnvelopes() *fakeEnvelopes {
	return &fakeEnvelopes{items: map[string]Envelope{}}
}

func (f *fakeEnvelopes) Create(_ context.Context, envelope Envelope) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.items[envelope.ID]; exists {
		return false, nil
	}
	f.items[envelope.ID] = envelope.clone()
	return true, nil
}

func (f *fakeEnvelopes) Get(_ context.Context, id string) (Envelope, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls++
	envelope, ok := f.items[id]
	if !ok {
		return Envelope{}, false, nil
	}
	return envelope.clone(), true, nil
}

func (f *fakeEnvelopes) GetMany(_ context.Context, ids []string) (map[string]Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getManyCalls++
	if f.failGetMany != nil {
		return nil, f.failGetMany
	}
	out := make(map[string]Envelope, len(ids))
	for _, id := range ids {
		if envelope, ok := f.items[id]; ok {
			out[id] = envelope.clone()
		}
	}
	return out, nil
}

// drop removes an envelope without touching any mailbox, which is what an
// expiry sweep looks like from a mailbox's point of view.
func (f *fakeEnvelopes) drop(id string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.items, id)
}

func (f *fakeEnvelopes) roundTrips() (gets int, getManys int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getCalls, f.getManyCalls
}

func (f *fakeEnvelopes) resetCounts() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getCalls, f.getManyCalls = 0, 0
}

var _ EnvelopeStore = (*fakeEnvelopes)(nil)
