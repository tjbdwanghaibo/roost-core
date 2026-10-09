package app

import (
	"context"
	"errors"
	"testing"
)

func TestSingletonIdentityChecksTheCurrentToken(t *testing.T) {
	store := newFakeSingletonStore(newFakeClock())
	checker := singletonLiveness{store: store, prefix: "test"}
	key := singletonKey("test", "game", 17)
	store.set(key, "current|host|42|123", 0)
	for _, token := range []string{"current", "previous"} {
		matches, err := checker.Matches(context.Background(), "game", 17, token)
		if err != nil || matches != (token == "current") {
			t.Fatalf("token=%s match=%v err=%v", token, matches, err)
		}
	}
	store.set(key, "replacement|host|43|124", 0)
	if matches, err := checker.Matches(context.Background(), "game", 17, "current"); err != nil || matches {
		t.Fatalf("stale generation=%v err=%v", matches, err)
	}
	store.remove(key)
	if matches, err := checker.Matches(context.Background(), "game", 17, "current"); err != nil || matches {
		t.Fatalf("missing lock=%v err=%v", matches, err)
	}
	store.set(key, "bare-token", 0)
	if _, err := checker.Matches(context.Background(), "game", 17, "bare-token"); err == nil {
		t.Fatal("malformed lock granted authority")
	}
	want := errors.New("backend failed")
	store.getErr = want
	if _, err := checker.Matches(context.Background(), "game", 17, "current"); !errors.Is(err, want) {
		t.Fatalf("backend error=%v", err)
	}
}

func TestSingletonIdentityCapabilityUsesTheSameHeldLock(t *testing.T) {
	h := newSingletonHarness(t, nil, nil)
	h.svc.onInit = func(registry *Registry) error {
		checker, ok := Lookup[SingletonIdentityChecker](registry, ModSingletonIdentityChecker)
		if !ok {
			return errors.New("identity checker not registered")
		}
		identity, ok := Lookup[SingletonIncarnation](registry, ModSingletonIncarnation)
		if !ok {
			return errors.New("process identity not registered")
		}
		matches, err := checker.Matches(context.Background(), "game", identity.Sid, identity.Token)
		if err != nil {
			return err
		}
		if !matches {
			return errors.New("checker does not see held lock")
		}
		return nil
	}
	result := h.start()
	h.awaitServed(t, result)
	if err := h.stop(t, result); err != nil {
		t.Fatal(err)
	}
}
