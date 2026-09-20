package session

import "testing"

// The Admin domain tests moved to roost-core/service/session (M-08). This one
// stays: it asserts about the generated Capability wrapper, which is transport.
// Admin is deliberately not on the bus.
func TestTheOperatorSurfaceIsNotOnTheSessionInterface(t *testing.T) {
	var asSession any = Capability(&Service{})
	if _, ok := asSession.(Admin); ok {
		t.Fatal("the capability published to other processes satisfies Admin; a caller over the " +
			"bus could declare a resource gone, which is a claim about the outside world that " +
			"this service cannot verify")
	}
}
