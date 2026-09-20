package session

import (
	"context"
	"sync"
)

// The domain tests moved to roost-core/service/session with the
// implementation (M-08); the Mod test here only needs a Releaser.
type recordingReleaser struct {
	mu       sync.Mutex
	releases map[string]int
}

func newReleaser() *recordingReleaser {
	return &recordingReleaser{releases: map[string]int{}}
}

func (r *recordingReleaser) Release(_ context.Context, _ Run, resource Resource) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.releases[resource.Kind+":"+resource.ID]++
	return nil
}
