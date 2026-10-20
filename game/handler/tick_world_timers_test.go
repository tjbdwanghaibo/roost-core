package handler

import (
	"testing"

	world "example.com/planet/game/entities/world"
)

// The driver delivers a due tick to a handler by name. The name the Nest
// generator registered for handlerTickWorldTimers has to be the one the
// World's driver was built with, or every deadline is delivered to a handler
// that does not exist and nothing fires.
func TestTheWorldTimerDriverDeliversToTheTickHandler(t *testing.T) {
	if got, want := handlerNameTickWorldTimers.String(), world.TimerTickHandler; got != want {
		t.Fatalf("nest registers %q, the driver delivers to %q", got, want)
	}
	if got := world.TimerDriver.Handler().String(); got != world.TimerTickHandler {
		t.Fatalf("the driver was built for %q, want %q", got, world.TimerTickHandler)
	}
}
