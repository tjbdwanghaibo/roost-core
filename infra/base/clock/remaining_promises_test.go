package clock

import (
	"testing"
	"time"
)

func TestProcessClockKeepsConfiguredSubmillisecondOffset(t *testing.T) {
	c := NewLogicClock()
	want := 1500 * time.Microsecond
	c.SetOffset(want)
	if got := c.Offset(); got != want {
		t.Fatalf("process offset=%s registry offset=%s", got, want)
	}
}
