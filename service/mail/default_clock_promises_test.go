package mail

import (
	businessclock "github.com/tjbdwanghaibo/roost-core/clock"
	"testing"
	"time"
)

func TestMissingBusinessClockUsesProcessOffset(t *testing.T) {
	old := businessclock.Offset()
	businessclock.SetOffset(24 * time.Hour)
	t.Cleanup(func() { businessclock.SetOffset(old) })
	h := newHarness(t)
	cfg := h.service.cfg
	cfg.Now = nil
	s, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if delta := s.cfg.Now().Sub(time.Now()); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("default business clock ignores process offset: %s", delta)
	}
}

func TestRedisEnvelopeDefaultClockMatchesBusinessExpiry(t *testing.T) {
	old := businessclock.Offset()
	businessclock.SetOffset(24 * time.Hour)
	t.Cleanup(func() { businessclock.SetOffset(old) })
	store, err := NewRedisEnvelopes(newFakeRedisEnvelopes(), "clock-test", nil)
	if err != nil {
		t.Fatal(err)
	}
	if delta := store.(*redisEnvelopes).now().Sub(time.Now()); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("envelope expiry compared with wrong clock: %s", delta)
	}
}
