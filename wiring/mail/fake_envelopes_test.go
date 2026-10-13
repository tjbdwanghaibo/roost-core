package mail

import domain "github.com/tjbdwanghaibo/roost-core/service/mail"

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
	items map[string]domain.Envelope
}

func newFakeEnvelopes() *fakeEnvelopes {
	return &fakeEnvelopes{items: map[string]domain.Envelope{}}
}

func (f *fakeEnvelopes) Create(_ context.Context, envelope domain.Envelope) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if _, exists := f.items[envelope.ID]; exists {
		return false, nil
	}
	f.items[envelope.ID] = cloneEnvelope(envelope)
	return true, nil
}

func (f *fakeEnvelopes) Get(_ context.Context, id string) (domain.Envelope, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	envelope, ok := f.items[id]
	if !ok {
		return domain.Envelope{}, false, nil
	}
	return cloneEnvelope(envelope), true, nil
}

func (f *fakeEnvelopes) GetMany(_ context.Context, ids []string) (map[string]domain.Envelope, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make(map[string]domain.Envelope, len(ids))
	for _, id := range ids {
		if envelope, ok := f.items[id]; ok {
			out[id] = cloneEnvelope(envelope)
		}
	}
	return out, nil
}

var _ domain.EnvelopeStore = (*fakeEnvelopes)(nil)

// cloneEnvelope mirrors the domain's unexported Envelope.clone: the two
// slices are the only shared state.
func cloneEnvelope(e domain.Envelope) domain.Envelope {
	out := e
	out.Recipients = append([]int64(nil), e.Recipients...)
	out.Attachment = append([]byte(nil), e.Attachment...)
	return out
}
