package chat

import (
	businessclock "github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"github.com/tjbdwanghaibo/roost-core/infra/storage/versionstore"
	"testing"
	"time"
)

func TestMissingBusinessClockUsesProcessOffset(t *testing.T) {
	old := businessclock.Offset()
	businessclock.SetOffset(24 * time.Hour)
	t.Cleanup(func() { businessclock.SetOffset(old) })
	s, err := NewStore(versionstore.NewMemoryStore[string, channelState](), Config{Policy: allowAllPolicy{}, Bodies: testRegistry(t)})
	if err != nil {
		t.Fatal(err)
	}
	if delta := s.(*channelStore).now().Sub(time.Now()); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("default business clock ignores process offset: %s", delta)
	}
}
func TestMissingSystemClockDoesNotFollowBusinessClock(t *testing.T) {
	s, err := NewStore(versionstore.NewMemoryStore[string, channelState](), Config{Policy: allowAllPolicy{}, Bodies: testRegistry(t), Now: func() time.Time { return time.Unix(100, 0) }})
	if err != nil {
		t.Fatal(err)
	}
	if delta := s.(*channelStore).system().Sub(time.Now()); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("infra/base/security/retention follows business clock: %s", delta)
	}
}
