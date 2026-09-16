package mail

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	fredis "github.com/tjbdwanghaibo/roost-core/redis"
)

// U-0140 · C2（空洞测试）· nightly gap map kit `service/mail` 9/20。
//
// Redis 存储的三条构造前提（客户端、键前缀、发送账本 TTL）；Send 的过期秒数必须
// 为正；Deliver / List / MarkRead / Delete 的玩家 id 必须为正、邮件 id 不能为空——
// 这些都要在触达任何存储之前以对应哨兵拒绝。

// idleRedis is an IRedis that must never be reached: the constructor's own
// checks come before any call.
type idleRedis struct{ fredis.IRedis }

func TestNewRedisStoresRefusesEachMissingPrerequisite(t *testing.T) {
	if _, err := NewRedisStores(nil, RedisConfig{Prefix: "mail", SendTTL: time.Minute}); err == nil || !strings.Contains(err.Error(), "redis client is nil") {
		t.Fatalf("NewRedisStores(nil client) = %v", err)
	}
	if _, err := NewRedisStores(idleRedis{}, RedisConfig{Prefix: "  ", SendTTL: time.Minute}); err == nil || !strings.Contains(err.Error(), "key prefix is required") {
		t.Fatalf("NewRedisStores with a blank prefix = %v", err)
	}
	for _, ttl := range []time.Duration{0, -time.Second} {
		if _, err := NewRedisStores(idleRedis{}, RedisConfig{Prefix: "mail", SendTTL: ttl}); err == nil || !strings.Contains(err.Error(), "send ttl must be positive") {
			t.Fatalf("NewRedisStores with send ttl %v = %v", ttl, err)
		}
	}
}

func TestServiceEntryPointsRefuseInvalidIdentifiers(t *testing.T) {
	ctx := context.Background()
	h := newHarness(t)
	for _, seconds := range []int64{0, -1} {
		req := directTo(7)
		req.ExpiresInSeconds = seconds
		if _, err := h.service.Send(ctx, req); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "expires_in_seconds must be positive") {
			t.Fatalf("Send with expires_in_seconds=%d = %v", seconds, err)
		}
	}
	if err := h.service.Deliver(ctx, 0, "m-1", 0); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "player id must be positive") {
		t.Fatalf("Deliver to player 0 = %v", err)
	}
	if err := h.service.Deliver(ctx, 7, "  ", 0); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "mail id is empty") {
		t.Fatalf("Deliver with a blank mail id = %v", err)
	}
	if _, err := h.service.List(ctx, 0, "", 10); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "player id must be positive") {
		t.Fatalf("List for player 0 = %v", err)
	}
	if _, err := h.service.MarkRead(ctx, -1, "m-1"); !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), "player id must be positive") {
		t.Fatalf("MarkRead for player -1 = %v", err)
	}
	if _, err := h.service.MarkRead(ctx, 7, " "); !errors.Is(err, ErrMailInvalid) || !strings.Contains(err.Error(), "mail id is empty") {
		t.Fatalf("MarkRead with a blank mail id = %v", err)
	}
	if _, err := h.service.Delete(ctx, 0, "m-1"); !errors.Is(err, ErrRequestInvalid) {
		t.Fatalf("Delete for player 0 = %v", err)
	}
	if _, err := h.service.Delete(ctx, 7, ""); !errors.Is(err, ErrMailInvalid) {
		t.Fatalf("Delete with an empty mail id = %v", err)
	}
	if _, found, err := h.service.Mailbox(ctx, 7); err != nil || found {
		t.Fatalf("refused operations created a mailbox: found=%v err=%v", found, err)
	}
}
