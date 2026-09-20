package activity

import (
	"errors"
	"fmt"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/errcode"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// The pairing table, the segment's contiguity and its exact count are pinned
// by TestEveryClientMistakeHasItsOwnCode in activity_test.go, which predates
// this file: the error-code assertions were written alongside the service and
// moved with it when the package was split out of global.
//
// What was NOT covered there is what this file adds, and both gaps are the
// kind that only show up at an RPC boundary — which is precisely where this
// package now has one.

// A foreign sentinel this package maps deliberately must NOT fall through to
// the internal code.
//
// This is the gap worth naming. The errors.Is branch for
// versionstore.ErrConflict was WRITTEN when the activity service was split out
// of package global — before the split it borrowed global's Error function —
// and nothing exercised it. Compare-and-set exhaustion under contention is a
// real, retryable outcome a caller can act on; "server error" is not an answer
// it can act on. Every store call in this package goes through versionstore,
// so this is not a rare path.
func TestAForeignSentinelIsMappedIntoThisSegment(t *testing.T) {
	err := fmt.Errorf("update: %w", versionstore.ErrConflict)
	code, reason := Error(err)
	if code == errcode.CodeInternal {
		t.Fatalf("%v reached the caller as CodeInternal; a foreign sentinel needs a deliberate "+
			"mapping, and a caller told \"server error\" cannot know the call is retryable", err)
	}
	if code != CodeConflict {
		t.Fatalf("%v maps to %d, want CodeConflict (%d)", err, code, CodeConflict)
	}
	if reason == "server error" {
		t.Fatalf("%v reports the generic reason %q", err, reason)
	}
}

// The code survives however deeply the error is wrapped, and when two coded
// errors are wrapped together the FIRST one wins.
//
// This package does not currently double-wrap anywhere: refuseNotify uses a
// single %w per branch. The precedence assertion is here anyway, and the
// reason is worth being exact about rather than dressing up as a property the
// code already relies on. It is a GUARD: the day someone writes
// `fmt.Errorf("%w: %w", ErrStatus, cause)` — which is how the sibling packages
// report a refusal that carries the caller's own reason — the order matters,
// and getting it backwards tells a client why its argument was odd instead of
// that the call was refused. That is a silent wrong answer, not an error, so
// there is nothing to notice later.
func TestTheCodeSurvivesWrappingAndTheOuterCodeWins(t *testing.T) {
	wrapped := fmt.Errorf("%w: context", ErrNotifyUnexpected)
	deeper := fmt.Errorf("caller: %w", wrapped)
	joined := errors.Join(deeper, errors.New("and something else"))
	for name, err := range map[string]error{"once": wrapped, "twice": deeper, "joined": joined} {
		if code := Code(err); code != CodeNotifyUnexpected {
			t.Fatalf("%s-wrapped error reports code %d, want %d", name, code, CodeNotifyUnexpected)
		}
	}
	// errors.Is still discriminates, so no existing caller changed meaning.
	if !errors.Is(wrapped, ErrNotifyUnexpected) {
		t.Fatal("errors.Is no longer matches the sentinel it was wrapped with")
	}
	if errors.Is(wrapped, ErrNotifyLate) {
		t.Fatal("errors.Is matches a different sentinel; the codes are not discriminating")
	}
	if code := Code(fmt.Errorf("%w: %w", ErrNotifyUnexpected, ErrRangeInvalid)); code != CodeNotifyUnexpected {
		t.Fatalf("a doubly-wrapped error reports code %d, want the OUTER %d",
			code, CodeNotifyUnexpected)
	}
}

// A nil error is CodeOK with no reason, which is what fills an RPC envelope on
// the success path.
func TestANilErrorIsCodeOK(t *testing.T) {
	if code, reason := Error(nil); code != CodeOK || reason != "" {
		t.Fatalf("a nil error reports (%d, %q), want (CodeOK, \"\")", code, reason)
	}
}
