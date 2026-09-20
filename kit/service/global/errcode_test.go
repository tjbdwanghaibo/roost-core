package global

import (
	"errors"
	"fmt"
	"sort"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/errcode"
	"github.com/tjbdwanghaibo/roost-core/versionstore"
)

// The segment this package is allocated, and how much of it is paired.
//
// Both are literals, and both are needed. Contiguity from the first code
// catches a hole in the middle; the count catches a truncation at the end,
// which contiguity cannot see because a shorter run is still contiguous.
// Together they pin the set exactly, so adding a public error code means
// editing this — which is right: a new code in a published segment is a
// deliberate act.
const (
	segmentFirst     = 570101
	segmentLast      = 570199
	segmentAllocated = 11
)

// expectedCodes is this package's allocated set, written out.
//
// It is a list rather than a contiguous range because 570111 through 570124
// are a HOLE, and every part of the hole is deliberate:
//
//   - 570111 was a catch-all "store failed" code. It was removed because
//     nothing could produce it deliberately and an unclassified error is
//     honestly CodeInternal.
//   - 570112 through 570124 were the ACTIVITY codes. The activity service
//     lived in this package until the transport generator made the packaging
//     visible — this package published two capabilities, and app.Service is
//     one per process, so routing and activity coordination were always two
//     deployments sharing one Go package. They are now two packages, and the
//     activity codes moved with the service into its own 6201xx segment.
//
// None of the vacated numbers are reused, and CodeRequestInvalid takes 570125
// past the end of them rather than 570111. Every one of those numbers meant
// something in a shipped release. Renumbering the activity codes was already
// a breaking change — so is renaming global.ActivityService to
// activity.Service — and it is one a major version carries; quietly giving an
// old number a new meaning is not, because a client that still matches on it
// gets a wrong answer rather than an error.
var expectedCodes = []int32{
	570101, 570102, 570103, 570104, 570105, 570106, 570107, 570108, 570109, 570110,
	// 570111-570124 retired: see above.
	570125,
}

// internalReason is what errcode.ClientError returns for anything it cannot
// classify. A sentinel that produces it has a code in name only.
const internalReason = "server error"

// codedSentinels is the pairing, as data.
var codedSentinels = map[int32]error{
	CodeRouteInvalid:   ErrRouteInvalid,
	CodeRouteMissing:   ErrRouteMissing,
	CodeRouteStale:     ErrRouteStale,
	CodeRouteMigrating: ErrRouteMigrating,
	CodeLeaseInvalid:   ErrLeaseInvalid,
	CodeLeaseMissing:   ErrLeaseMissing,
	CodeLeaseNotHolder: ErrLeaseNotHolder,
	CodeLeaseExpired:   ErrLeaseExpired,
	CodeRangeInvalid:   ErrRangeInvalid,
	CodeConflict:       ErrConflict,
	CodeRequestInvalid: ErrRequestInvalid,
}

// Every sentinel this package returns must carry its own code.
//
// The confirmed defect this answers: a whole service had no error codes, so
// "board id is empty" — a pure client mistake — reached the client as
// CodeInternal / "server error". A code constant declared beside a sentinel
// that does not carry it looks like coverage and provides none.
func TestEverySentinelCarriesItsOwnCode(t *testing.T) {
	if len(codedSentinels) == 0 {
		t.Fatal("the pairing table is empty")
	}
	for code, sentinel := range codedSentinels {
		got, reason := Error(sentinel)
		if got != code {
			t.Fatalf("sentinel %v reports code %d, want %d", sentinel, got, code)
		}
		if got == errcode.CodeInternal {
			t.Fatalf("sentinel %v reports CodeInternal; a client mistake must not read as a server fault", sentinel)
		}
		// Not merely non-empty: errcode falls back name -> message -> "server
		// error", so a sentinel defined with no text still produces a reason,
		// and that reason tells a client nothing.
		if reason == internalReason {
			t.Fatalf("sentinel %v reports the generic reason %q", sentinel, reason)
		}
	}
}

// The paired codes are exactly the allocated set.
//
// An explicit list rather than a contiguity check, because this segment has a
// documented hole at 570111 — see expectedCodes. The list is strictly stronger
// than contiguity anyway: it catches an addition, a removal and a renumbering,
// where contiguity alone misses a truncation at the end.
func TestTheCodeSegmentIsExactlyAsAllocated(t *testing.T) {
	codes := make([]int32, 0, len(codedSentinels))
	for code := range codedSentinels {
		codes = append(codes, code)
		if code < segmentFirst || code > segmentLast {
			t.Fatalf("code %d is outside this package's segment %d-%d", code, segmentFirst, segmentLast)
		}
	}
	sort.Slice(codes, func(i, j int) bool { return codes[i] < codes[j] })
	if len(codes) != segmentAllocated {
		t.Fatalf("%d codes are paired, want %d", len(codes), segmentAllocated)
	}
	if len(codes) != len(expectedCodes) {
		t.Fatalf("%d codes are paired but %d are declared in expectedCodes", len(codes), len(expectedCodes))
	}
	for index, code := range codes {
		if code != expectedCodes[index] {
			t.Fatalf("at position %d the paired code is %d, expectedCodes says %d; a code was "+
				"added, removed or renumbered", index, code, expectedCodes[index])
		}
	}
	// And 570111 stays vacant: it once meant "store failed", and a code that
	// changes meaning is worse than a gap.
	for _, code := range codes {
		if code == 570111 {
			t.Fatal("570111 was reused; it previously meant \"store failed\" and a code that " +
				"changes meaning silently breaks any client that matched on it")
		}
	}
}

// The code survives however deeply the error is wrapped, which is the whole
// reason the sentinels carry it rather than a lookup table doing so: every
// existing call site wraps with fmt.Errorf and none of them had to change.
func TestTheCodeSurvivesWrapping(t *testing.T) {
	first, second := twoDistinctSentinels(t)
	firstCode, _ := Error(first)

	wrapped := fmt.Errorf("%w: context", first)
	deeper := fmt.Errorf("caller: %w", wrapped)
	joined := errors.Join(deeper, errors.New("and something else"))
	for name, err := range map[string]error{"once": wrapped, "twice": deeper, "joined": joined} {
		if code := Code(err); code != firstCode {
			t.Fatalf("%s-wrapped error reports code %d, want %d", name, code, firstCode)
		}
	}
	// errors.Is still discriminates, so no existing test or caller changed
	// meaning.
	if !errors.Is(wrapped, first) {
		t.Fatal("errors.Is no longer matches the sentinel it was wrapped with")
	}
	if errors.Is(wrapped, second) {
		t.Fatal("errors.Is matches a different sentinel; the codes are not discriminating")
	}
	// When two coded errors are wrapped together, the FIRST wins. That is what
	// makes a refusal wrapping a caller's own reason report the refusal.
	if code := Code(fmt.Errorf("%w: %w", first, second)); code != firstCode {
		t.Fatalf("a doubly-wrapped error reports code %d, want the outer %d", code, firstCode)
	}
}

// An unclassified failure reads as CodeInternal — the honest answer, and the
// reason there is no service-specific catch-all code.
func TestAnUnclassifiedFailureReadsAsInternal(t *testing.T) {
	if code := Code(errors.New("dial redis: connection refused")); code != errcode.CodeInternal {
		t.Fatalf("an unclassified failure reports code %d, want CodeInternal", code)
	}
	if code, reason := Error(nil); code != CodeOK || reason != "" {
		t.Fatalf("a nil error reports (%d, %q), want (CodeOK, \"\")", code, reason)
	}
}

// A foreign sentinel this package maps deliberately must NOT fall through to
// the internal code. Compare-and-set exhaustion under contention is a real, retryable outcome.
func TestAForeignSentinelIsMappedIntoThisSegment(t *testing.T) {
	err := fmt.Errorf("update: %w", versionstore.ErrConflict)
	code := Code(err)
	if code == errcode.CodeInternal {
		t.Fatalf("%v reached the caller as CodeInternal; a foreign sentinel needs a deliberate mapping", err)
	}
	if code != CodeConflict {
		t.Fatalf("%v maps to %d, want %d", err, code, CodeConflict)
	}
}

// twoDistinctSentinels returns the lowest two coded sentinels, so the wrapping
// test does not depend on any particular error existing.
func twoDistinctSentinels(t *testing.T) (error, error) {
	t.Helper()
	codes := make([]int, 0, len(codedSentinels))
	for code := range codedSentinels {
		codes = append(codes, int(code))
	}
	sort.Ints(codes)
	if len(codes) < 2 {
		t.Skip("this package has fewer than two coded sentinels")
	}
	return codedSentinels[int32(codes[0])], codedSentinels[int32(codes[1])]
}
