package account

import (
	businessclock "github.com/tjbdwanghaibo/roost-core/infra/base/clock"
	"testing"
	"time"
)

func TestMissingBusinessClockUsesProcessOffset(t *testing.T) {
	old := businessclock.Offset()
	businessclock.SetOffset(24 * time.Hour)
	t.Cleanup(func() { businessclock.SetOffset(old) })
	_, _, cfg := newService(t)
	cfg.Now = nil
	cfg.SystemNow = nil
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if delta := s.cfg.Now().Sub(time.Now()); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("default business clock ignores process offset: %s", delta)
	}
}
func TestMissingSystemClockDoesNotFollowBusinessClock(t *testing.T) {
	_, _, cfg := newService(t)
	cfg.Now = func() time.Time { return time.Unix(100, 0) }
	cfg.SystemNow = nil
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if delta := s.cfg.SystemNow().Sub(time.Now()); delta < -time.Minute || delta > time.Minute {
		t.Fatalf("infra/base/security/retention follows business clock: %s", delta)
	}
}
