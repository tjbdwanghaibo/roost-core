package mail

import domain "github.com/tjbdwanghaibo/roost-core/service/mail"

import (
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/tjbdwanghaibo/roost-core/infra/observe/servicemetrics"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
)

// The domain tests moved to roost-core/service/mail with the implementation
// (M-07). What stays here is the Mod / transport half, and it still needs a
// running Service to wire: this is the minimum of the old harness.
type harness struct {
	service *domain.Service
	clock   *clock
}

type clock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *clock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func tokens(prefix string) func() (string, error) {
	var mu sync.Mutex
	next := 0
	return func() (string, error) {
		mu.Lock()
		defer mu.Unlock()
		next++
		return fmt.Sprintf("%s-%d", prefix, next), nil
	}
}

func newHarness(t *testing.T, mutate ...func(*domain.Config)) *harness {
	t.Helper()
	c := &clock{now: time.Unix(1_700_000_000, 0)}
	cfg := domain.Config{
		Envelopes:     newFakeEnvelopes(),
		Mailboxes:     versionstore.NewMemoryStore[int64, domain.Mailbox](),
		Sends:         versionstore.NewMemoryStore[string, domain.SentRecord](),
		ClaimLease:    30 * time.Second,
		NewMailID:     tokens("mail"),
		NewClaimToken: tokens("token"),
		Now:           c.Now,
		Metrics:       servicemetrics.NewRecorder(),
	}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := domain.New(cfg)
	if err != nil {
		t.Fatalf("new service: %v", err)
	}
	return &harness{service: service, clock: c}
}

func directTo(recipients ...int64) domain.SendRequest {
	return domain.SendRequest{
		Audience: domain.AudienceDirect, Recipients: recipients,
		Subject: "reward", Body: "well played",
		ExpiresInSeconds: 604800, RequestID: "send-1",
	}
}
