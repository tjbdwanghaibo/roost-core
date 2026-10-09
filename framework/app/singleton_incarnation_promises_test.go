package app

import (
	"bytes"
	"errors"
	"testing"
)

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
