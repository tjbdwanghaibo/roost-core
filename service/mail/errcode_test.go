package mail

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/errcode"
)

// The segment mail is allocated. Written out here rather than derived, because
// this is the specification: a code outside it collides with another service,
// and a hole inside it means a sentinel lost its pairing.
const (
	segmentFirst = 590101
	segmentLast  = 590199
	// segmentAllocated is how many codes are actually paired.
	//
	// It is a literal, and it has to be: contiguity from segmentFirst catches
	// a hole in the middle but not a truncation at the end — dropping the last
	// entry leaves a shorter run that is still contiguous. Start plus count
	// pins the set exactly. Adding a public error code therefore means editing
	// this number, which is right: a new code in a published segment is a
	// deliberate act, not something that should slip in.
	segmentAllocated = 15
)

// internalReason is what errcode.ClientError returns for anything it cannot
// classify. A sentinel that produces it has a code in name only.
const internalReason = "server error"

// Every sentinel this package returns must carry its own code.
//
// The confirmed defect this answers: a whole service had no error codes, so
// "board id is empty" — a pure client mistake — reached the client as
// CodeInternal / "server error". A code constant declared beside a sentinel
// that does not carry it looks like coverage and provides none.
func TestEverySentinelCarriesItsOwnCode(t *testing.T) {
	if len(codeBySentinel) == 0 {
		t.Fatal("the pairing table is empty")
	}
	for code, sentinel := range codeBySentinel {
		got, reason := errcode.ClientError(sentinel)
		if got != code {
			t.Fatalf("sentinel %v reports code %d, want %d", sentinel, got, code)
		}
		if got == errcode.CodeInternal {
			t.Fatalf("sentinel %v reports CodeInternal; a client mistake must not read as a server fault", sentinel)
		}
		// Not merely non-empty: it must not be the generic internal reason.
		// errcode falls back name -> message -> "server error", so a sentinel
		// defined with no text still produces a non-empty reason — and that
		// reason tells a client nothing, which is the failure being prevented.
		if reason == internalReason {
			t.Fatalf("sentinel %v reports the generic reason %q; a client learns nothing from it",
				sentinel, reason)
		}
	}
}

// No two sentinels share a code. errcode.Define keys a global registry on the
// code and OVERWRITES a duplicate silently, so a collision is invisible
// without this check — and two services sharing a code means a client that
// switches on it takes the wrong branch.
func TestNoTwoSentinelsShareACode(t *testing.T) {
	seen := map[int32]error{}
	for code, sentinel := range codeBySentinel {
		if previous, ok := seen[code]; ok {
			t.Fatalf("code %d is used by both %v and %v", code, previous, sentinel)
		}
		seen[code] = sentinel
	}
	// And every code is inside this package's allocated segment, so a typo
	// cannot silently land in another service's range.
	for code := range codeBySentinel {
		if code < segmentFirst || code > segmentLast {
			t.Fatalf("code %d is outside mail's segment %d-%d", code, segmentFirst, segmentLast)
		}
	}
}

// The segment is allocated contiguously from its first code, with no holes.
//
// This is what catches an entry disappearing from the pairing table, which the
// table-driven test above structurally cannot: a table-driven test that loses a
// row simply checks less. Contiguity is an independent property — remove
// 590109 and there is a hole at 590109 — so the table cannot shrink unnoticed.
func TestTheCodeSegmentIsContiguousFromItsFirstCode(t *testing.T) {
	codes := make([]int, 0, len(codeBySentinel))
	for code := range codeBySentinel {
		codes = append(codes, int(code))
	}
	sort.Ints(codes)
	if len(codes) != segmentAllocated {
		t.Fatalf("%d codes are paired, want %d; a code was added or removed without updating "+
			"the count, and contiguity alone cannot see a truncation at the end",
			len(codes), segmentAllocated)
	}
	if codes[0] != segmentFirst {
		t.Fatalf("the segment starts at %d, want %d", codes[0], segmentFirst)
	}
	for index, code := range codes {
		want := segmentFirst + index
		if code != want {
			t.Fatalf("the segment has a hole: expected %d at position %d, found %d. "+
				"Either a sentinel is missing from the pairing table or a code was skipped",
				want, index, code)
		}
	}
}

// The code survives however deeply the error is wrapped, which is the whole
// reason the sentinels carry it rather than a lookup table doing so: every
// existing call site wraps with fmt.Errorf and none of them had to change.
func TestTheCodeSurvivesWrapping(t *testing.T) {
	wrapped := fmt.Errorf("%w: mail %s", ErrMailMissing, "m1")
	deeper := fmt.Errorf("list: %w", wrapped)
	joined := errors.Join(deeper, errors.New("and something else"))

	for name, err := range map[string]error{
		"once":   wrapped,
		"twice":  deeper,
		"joined": joined,
	} {
		if code, _ := errcode.ClientError(err); code != CodeMailMissing {
			t.Fatalf("%s-wrapped error reports code %d, want %d", name, code, CodeMailMissing)
		}
	}
	// And errors.Is still discriminates, so no existing test or caller
	// changed meaning.
	if !errors.Is(wrapped, ErrMailMissing) {
		t.Fatal("errors.Is no longer matches the sentinel it was wrapped with")
	}
	if errors.Is(wrapped, ErrMailInvalid) {
		t.Fatal("errors.Is matches a different sentinel; the codes are not discriminating")
	}
	// The context added at the call site is still in the message.
	if got := wrapped.Error(); !strings.Contains(got, "mail m1") {
		t.Fatalf("the wrapped error lost its context: %q", got)
	}
}

// The errors real call paths return carry codes — not just the bare sentinels.
// A pairing table that is correct while the service returns something else is
// a table that describes an intention.
func TestErrorsFromRealCallPathsCarryTheirCodes(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()

	cases := []struct {
		name string
		run  func() error
		want int32
	}{
		{"keyless send", func() error {
			req := directTo(1)
			req.RequestID = ""
			_, err := h.service.Send(ctx, req)
			return err
		}, CodeRequestInvalid},
		{"mail with no expiry", func() error {
			req := directTo(1)
			req.ExpiresInSeconds = 0
			_, err := h.service.Send(ctx, req)
			return err
		}, CodeMailInvalid},
		{"broadcast with no deliverer", func() error {
			_, err := h.service.Send(ctx, SendRequest{
				Audience: AudienceBroadcast, Subject: "s",
				ExpiresInSeconds: 3600, RequestID: "b1",
			})
			return err
		}, CodeAudienceInvalid},
		{"negative page limit", func() error {
			_, err := h.service.List(ctx, 1, "", -1)
			return err
		}, CodeRangeInvalid},
		{"claim a mail that was never delivered", func() error {
			_, err := h.service.ReserveClaim(ctx, 1, "no-such-mail", "")
			return err
		}, CodeMailMissing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.run()
			if err == nil {
				t.Fatal("the call succeeded")
			}
			code, reason := errcode.ClientError(err)
			if code != tc.want {
				t.Fatalf("code %d (%q), want %d; the error a caller sees is %v", code, reason, tc.want, err)
			}
		})
	}
}

// A store failure is NOT a client error. It has no code of its own and must
// read as CodeInternal — the honest answer, and the reason the "store failed"
// code that once sat in the constant block is gone.
func TestAStoreFailureReadsAsInternal(t *testing.T) {
	code, _ := errcode.ClientError(errors.New("dial redis: connection refused"))
	if code != errcode.CodeInternal {
		t.Fatalf("an unclassified failure reports code %d, want CodeInternal", code)
	}
}
