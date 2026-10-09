package activity

import domain "github.com/tjbdwanghaibo/roost-core/service/activity"

import (
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"sync"
	"testing"
	"time"
)

// activityClock is the injected clock for the activity half. Named apart from
// the routing half's clock so both halves can be exercised in one package
// without one test's time travel moving the other's deadlines.
type activityClock struct {
	mu  sync.Mutex
	now time.Time
}

func (c *activityClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *activityClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

// testGroupsYAML is the activity groups file every coordinator in this
// package's tests is built with, unless a test is about the groups themselves:
// "group-a" (the group activityKey uses) of games 1 to 9 and 1000, and
// "group-b" of games 2001 and 2002 for the tests that need a second group. New
// requires groups (RR-20261006-17), and the Mod's tests read the same file
// (modConfig), so the two ways of building a coordinator agree on it.
const testGroupsYAML = `groups:
  - id: group-a
    game_sids: [1, 2, 3, 4, 5, 6, 7, 8, 9, 1000]
  - id: group-b
    game_sids: [2001, 2002]
`

// testGroups parses testGroupsYAML for a Config built directly.
func testGroups(t testing.TB) *domain.Groups {
	t.Helper()
	groups, err := domain.ParseGroups([]byte(testGroupsYAML), "test activity groups")
	if err != nil {
		t.Fatal(err)
	}
	return &groups
}

func newActivityService(t *testing.T, mutate ...func(*domain.Config)) (*domain.Service, *activityClock) {
	t.Helper()
	c := &activityClock{now: time.Unix(1_700_000_000, 0)}
	cfg := domain.Config{
		Groups:              testGroups(t),
		Activities:          versionstore.NewMemoryStore[domain.Key, domain.Activity](),
		Participants:        versionstore.NewMemoryStore[domain.ParticipantKey, domain.Participant](),
		Ledger:              versionstore.NewMemoryStore[domain.RequestKey, domain.ProgressReservation](),
		Audits:              versionstore.NewMemoryStore[domain.Key, domain.NotifyAuditLog](),
		Dispatches:          versionstore.NewMemoryStore[domain.DispatchKey, domain.Dispatch](),
		Windows:             versionstore.NewMemoryStore[string, domain.Window](),
		GraceWindow:         30 * time.Second,
		ReservationTTL:      10 * time.Minute,
		DispatchBackoff:     5 * time.Second,
		DispatchMaxAttempts: 3,
		Now:                 c.Now,
	}
	for _, m := range mutate {
		m(&cfg)
	}
	service, err := domain.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return service, c
}

func activityKey(id string) domain.Key {
	return domain.Key{GroupID: "group-a", ActivityID: id, Phase: domain.PhaseClose}
}
