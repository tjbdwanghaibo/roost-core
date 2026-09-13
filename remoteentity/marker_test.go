package remoteentity

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/tjbdwanghaibo/roost-core/entity"
)

type markerEvalStub struct {
	mu     sync.Mutex
	values map[string]string
}

func newMarkerEvalStub() *markerEvalStub {
	return &markerEvalStub{values: make(map[string]string)}
}

func (s *markerEvalStub) Eval(_ context.Context, script string, _ []string, args ...any) (any, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(args) == 0 {
		return nil, errors.New("missing marker field")
	}
	field := args[0].(string)
	switch script {
	case ownershipGetScript:
		return s.values[field], nil
	case ownershipClaimScript:
		if len(args) != 2 {
			return nil, errors.New("invalid claim args")
		}
		owner := args[1].(string)
		if current := s.values[field]; current != "" {
			_, lease, err := parseMarkerLease(current)
			if err == nil && lease.OwnerSid == mustParseMarkerOwner(owner) {
				return current, nil
			}
			return "", nil
		}
		value := "local:" + owner + ":1:1"
		s.values[field] = value
		return value, nil
	case ownershipEnterSharedScript:
		if len(args) != 2 || s.values[field] != args[1].(string) {
			return "", nil
		}
		_, lease, err := parseMarkerLease(s.values[field])
		if err != nil || lease.Shared {
			return "", nil
		}
		lease.Shared = true
		value := markerLeaseAsLuaWould(lease, incMarkerEpochAsLuaWould(lease.MarkerEpoch))
		if value == "" {
			return "", nil
		}
		s.values[field] = value
		return value, nil
	case ownershipLeaveSharedScript:
		if len(args) != 2 || s.values[field] != args[1].(string) {
			return "", nil
		}
		_, lease, err := parseMarkerLease(s.values[field])
		if err != nil || !lease.Shared {
			return "", nil
		}
		lease.Shared = false
		value := markerLeaseAsLuaWould(lease, incMarkerEpochAsLuaWould(lease.MarkerEpoch))
		if value == "" {
			return "", nil
		}
		s.values[field] = value
		return value, nil
	default:
		return nil, errors.New("unexpected script")
	}
}

// markerLeaseAsLuaWould models how the script builds the new lease string.
//
// The script used to concatenate a Lua NUMBER, and Lua 5.1 renders numbers
// with "%.14g", so 10^14 came out as "1e+14" and Go could no longer read the
// record back — after it had been written (RR-20260913-11). The script now
// increments the decimal digits itself and refuses anything past uint64, so
// this double does the same; an empty result models that refusal.
func markerLeaseAsLuaWould(lease entity.RemoteEntityMarkerLease, nextEpoch uint64) string {
	if nextEpoch == 0 {
		return ""
	}
	lease.MarkerEpoch = nextEpoch
	return formatMarkerLease(lease)
}

// incMarkerEpochAsLuaWould is the script's incdec: exact, and nil past uint64.
func incMarkerEpochAsLuaWould(value uint64) uint64 {
	if value == ^uint64(0) {
		return 0
	}
	return value + 1
}

func mustParseMarkerOwner(raw string) int32 {
	_, lease, _ := parseMarkerLease("local:" + raw + ":1:1")
	return lease.OwnerSid
}

func TestMockOwnershipStorePreservesEpochAcrossModeTransitions(t *testing.T) {
	store := newMockMarkerStore()
	claimed, err := store.ClaimOwnership(context.Background(), 42, 1001)
	if err != nil || claimed.Shared || claimed.MarkerEpoch != 1 {
		t.Fatalf("claim = %#v, err=%v", claimed, err)
	}
	shared, err := store.EnterSharedExpected(context.Background(), 42, claimed)
	if err != nil || !shared.Shared || shared.MarkerEpoch != 2 {
		t.Fatalf("enter shared = %#v, err=%v", shared, err)
	}
	local, err := store.LeaveSharedExpected(context.Background(), 42, shared)
	if err != nil || local.Shared || local.MarkerEpoch != 3 || local.OwnerSid != 1001 {
		t.Fatalf("leave shared = %#v, err=%v", local, err)
	}
	observed, found, err := store.GetOwnership(context.Background(), 42)
	if err != nil || !found || observed != local {
		t.Fatalf("ownership found=%v lease=%#v err=%v", found, observed, err)
	}
}

func TestParseMarkerLeasePreservesModeAndFence(t *testing.T) {
	shared, lease, err := parseMarkerLease("shared:1001:9:4")
	if err != nil || !shared || !lease.Shared || lease.OwnerSid != 1001 || lease.MarkerEpoch != 9 || lease.RouteEpoch != 4 {
		t.Fatalf("shared parse = %v %#v %v", shared, lease, err)
	}
	shared, lease, err = parseMarkerLease("local:1001:10:4")
	if err != nil || shared || lease.Shared || lease.OwnerSid != 1001 || lease.MarkerEpoch != 10 || lease.RouteEpoch != 4 {
		t.Fatalf("local parse = %v %#v %v", shared, lease, err)
	}
}

func TestRedisOwnershipClaimAndEnterSharedRejectStaleLease(t *testing.T) {
	redis := newMarkerEvalStub()
	store := newRedisMarkerForEval(redis, "marks")
	expected, err := store.ClaimOwnership(context.Background(), 42, 1001)
	if err != nil || expected != (entity.RemoteEntityMarkerLease{OwnerSid: 1001, MarkerEpoch: 1, RouteEpoch: 1}) {
		t.Fatalf("claim = %#v, err=%v", expected, err)
	}
	if _, err = store.ClaimOwnership(context.Background(), 42, 2002); err == nil {
		t.Fatal("a second owner must not claim an existing lease")
	}

	first, err := store.EnterSharedExpected(context.Background(), 42, expected)
	if err != nil || !first.Shared || first.MarkerEpoch != 2 || first.OwnerSid != 1001 || first.RouteEpoch != 1 {
		t.Fatalf("enter shared = %#v, err=%v", first, err)
	}
	if _, err = store.EnterSharedExpected(context.Background(), 42, expected); err == nil {
		t.Fatal("stale expected lease must not overwrite an existing shared lease")
	}
	observed, found, err := store.GetOwnership(context.Background(), 42)
	if err != nil || !found || observed != first {
		t.Fatalf("ownership after stale transition found=%v lease=%#v err=%v", found, observed, err)
	}
}
