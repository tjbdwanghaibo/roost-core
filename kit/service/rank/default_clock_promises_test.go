package rank

import (
	businessclock "github.com/tjbdwanghaibo/roost-core/clock"
	"testing"
	"time"
)

func TestMissingBusinessClockUsesProcessOffset(t *testing.T) {
	old := businessclock.Offset()
	businessclock.SetOffset(24 * time.Hour)
	t.Cleanup(func() { businessclock.SetOffset(old) })
	s, err := NewRedisStore(newFakeRedis(), RedisConfig{Prefix: "clock-test"})
	if err != nil {
		t.Fatal(err)
	}
	if delta := s.cfg.Now().Sub(time.Now()); delta < 23*time.Hour || delta > 25*time.Hour {
		t.Fatalf("default business clock ignores process offset: %s", delta)
	}
}
