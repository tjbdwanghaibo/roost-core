package app

import (
	"bytes"
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

// O-M6-6：持有单实例锁的进程把锁的身份（键、sid、本次启动的 token）登记成 ModSingletonIncarnation，供框架模块
// 接管上一代进程留下的按 sid 协调状态（Remote 实体共享锁）。Mod Init 时锁已持有，token 就是锁值的第一段；
// 未启用单实例锁时不登记——没有“旧进程已死”的保证，模块不得接管。
func TestSingletonIncarnationIsTheHeldLocksIdentity(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		h := newSingletonHarness(t, nil, nil)
		var got SingletonIncarnation
		var heldAtInit []byte
		h.svc.onInit = func(r *Registry) error {
			incarnation, ok := Lookup[SingletonIncarnation](r, ModSingletonIncarnation)
			if !ok {
				return errors.New("ModSingletonIncarnation not registered")
			}
			got = incarnation
			heldAtInit = h.store.value(incarnation.Key)
			return nil
		}
		result := h.start()
		h.awaitServed(t, result)
		if got.Key != testSingletonPrefix+":game:1000" || got.Sid != 1000 {
			t.Fatalf("incarnation = %+v, want key %s:game:1000 sid 1000", got, testSingletonPrefix)
		}
		token, _, _ := bytes.Cut(heldAtInit, []byte("|"))
		if got.Token == "" || string(token) != got.Token || len(got.Token) != 16 {
			t.Fatalf("incarnation token %q, lock value at Init %q: want the held value's 16-hex token", got.Token, heldAtInit)
		}
		if err := h.stop(t, result); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
	t.Run("disabled", func(t *testing.T) {
		h := newSingletonHarness(t, nil, nil)
		h.app.cfg.Set("singleton.enabled", false)
		result := h.start()
		h.awaitServed(t, result)
		if _, ok := h.svc.registry.Get(ModSingletonIncarnation); ok {
			t.Fatal("incarnation registered while singleton is disabled")
		}
		if err := h.stop(t, result); err != nil {
			t.Fatalf("run: %v", err)
		}
	})
}
