package dungeon

import "testing"

// The claim window is the one piece of the exactly-once argument that is pure
// logic, so it gets a table test here; the rest of the argument (the ledger is
// written in the same transaction as the reward, only the run.State the
// session service returned earns one, and a run past the window is refused
// rather than paid) is asserted against the real handler in
// game/handler/claim_dungeon_test.go and end to end by the robot scenario.
func TestClaimWindowClosedIsTheAdmissionAndThePruningRule(t *testing.T) {
	const now int64 = 1_800_000_000
	// session.run_ttl in the generated configuration is 30 minutes: how long
	// a run may stay *open*. It is not a storage lifetime — a succeeded run
	// stays in the session service and can be finished again indefinitely,
	// which is why the window below cannot be derived from it
	// (RR-20260918-04).
	const runTTL int64 = 30 * 60
	for name, test := range map[string]struct {
		resolvedAt int64
		closed     bool
	}{
		"just resolved":          {now, false},
		"one run ttl old":        {now - runTTL, false},
		"just inside the window": {now - ClaimWindowSeconds, false},
		"just past the window":   {now - ClaimWindowSeconds - 1, true},
		"long past the window":   {now - 30*24*60*60, true},
		"absent":                 {0, true},
		"negative":               {-1, true},
	} {
		if got := ClaimWindowClosed(test.resolvedAt, now); got != test.closed {
			t.Errorf("%s: ClaimWindowClosed(%d, %d) = %v, want %v", name, test.resolvedAt, now, got, test.closed)
		}
	}
	// A run resolved at the very end of its own life still has the whole
	// window to be collected in; anything less and a legitimate first claim
	// could be refused for being slow.
	if ClaimWindowSeconds <= runTTL {
		t.Errorf("the claim window (%ds) must outlast a run's own life (%ds), or a first claim can time out before the player can send it", ClaimWindowSeconds, runTTL)
	}
}
