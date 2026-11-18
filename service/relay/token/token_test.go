package token

import (
	"errors"
	"strings"
	"testing"
	"time"
)

var testKey = []byte("0123456789abcdef-relay-test-key")

func sample(now time.Time) Claims {
	return Claims{
		Relay: "relay-1", Room: "room.with.dots", Account: "acc:42", Role: RoleMember,
		MaxMembers: 8, ExpiresAt: now.Add(DefaultTTL),
	}
}

// 签发与校验往返：每个字段原样回来，名字里的点号不影响分段。
func TestSignVerifyRoundTrip(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	for _, role := range []Role{RoleHost, RoleMember} {
		claims := sample(now)
		claims.Role = role
		signed, err := Sign(testKey, claims)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(signed, Version+".") || len(strings.Split(signed, ".")) != segments {
			t.Fatalf("token %q does not have the rt1 eight-segment shape", signed)
		}
		got, err := Verify(testKey, signed, "relay-1", now)
		if err != nil {
			t.Fatalf("Verify(%s) = %v", role, err)
		}
		want := claims
		want.ExpiresAt = time.Unix(claims.ExpiresAt.Unix(), 0)
		if got != want {
			t.Fatalf("claims = %+v, want %+v", got, want)
		}
	}
}

// 过期：到过期秒那一刻起无效，前一秒有效。
func TestVerifyRefusesExpiredTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signed, err := Sign(testKey, sample(now))
	if err != nil {
		t.Fatal(err)
	}
	expiry := now.Add(DefaultTTL)
	if _, err := Verify(testKey, signed, "relay-1", expiry.Add(-time.Second)); err != nil {
		t.Fatalf("one second before expiry: %v", err)
	}
	for _, at := range []time.Time{expiry, expiry.Add(time.Hour)} {
		if _, err := Verify(testKey, signed, "relay-1", at); !errors.Is(err, ErrInvalid) {
			t.Fatalf("at %v: err = %v, want ErrInvalid", at.Sub(now), err)
		}
	}
}

// 篡改：改任何一段（包括把 member 改成 host、把人数改大、把实例改成别的）都无效；别的密钥签的无效；
// 发给别的 relay 实例的无效。
func TestVerifyRefusesTamperedForeignAndMisdirectedTokens(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	signed, err := Sign(testKey, sample(now))
	if err != nil {
		t.Fatal(err)
	}
	parts := strings.Split(signed, ".")
	replace := func(index int, value string) string {
		copied := append([]string(nil), parts...)
		copied[index] = value
		return strings.Join(copied, ".")
	}
	cases := map[string]string{
		"expiry pushed":       replace(1, "9999999999"),
		"relay swapped":       replace(2, encode("relay-2")),
		"room swapped":        replace(3, encode("other-room")),
		"account swapped":     replace(4, encode("acc:43")),
		"role promoted":       replace(5, string(RoleHost)),
		"max members raised":  replace(6, "64"),
		"signature truncated": replace(7, parts[7][:len(parts[7])-2]),
		"version changed":     replace(0, "rt2"),
		"segment dropped":     strings.Join(parts[:7], "."),
		"empty":               "",
		"oversized":           signed + strings.Repeat("x", MaxBytes),
		"account ticket":      "at2.9999999999.party.YWNj.c2ln",
	}
	for name, token := range cases {
		if _, err := Verify(testKey, token, "relay-1", now); !errors.Is(err, ErrInvalid) {
			t.Errorf("%s: err = %v, want ErrInvalid", name, err)
		}
	}
	if _, err := Verify([]byte("another-key-of-enough-length"), signed, "relay-1", now); !errors.Is(err, ErrInvalid) {
		t.Errorf("foreign key: err = %v, want ErrInvalid", err)
	}
	if _, err := Verify(testKey, signed, "relay-2", now); !errors.Is(err, ErrInvalid) {
		t.Errorf("token for relay-1 presented to relay-2: err = %v, want ErrInvalid", err)
	}
}

func TestSignRefusesIncompleteClaimsAndShortKeys(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	if _, err := Sign([]byte("short"), sample(now)); !errors.Is(err, ErrKey) {
		t.Fatalf("short key: err = %v, want ErrKey", err)
	}
	if _, err := Verify([]byte("short"), "rt1.x", "relay-1", now); !errors.Is(err, ErrKey) {
		t.Fatalf("Verify with a short key: err = %v, want ErrKey", err)
	}
	broken := []func(*Claims){
		func(c *Claims) { c.Relay = "" },
		func(c *Claims) { c.Room = "" },
		func(c *Claims) { c.Account = "" },
		func(c *Claims) { c.Role = "owner" },
		func(c *Claims) { c.MaxMembers = 0 },
		func(c *Claims) { c.MaxMembers = MaxMembersLimit + 1 },
		func(c *Claims) { c.ExpiresAt = time.Time{} },
		func(c *Claims) { c.Account = strings.Repeat("a", MaxBytes) },
	}
	for i, breakIt := range broken {
		claims := sample(now)
		breakIt(&claims)
		if _, err := Sign(testKey, claims); err == nil {
			t.Errorf("case %d: Sign accepted %+v", i, claims)
		}
	}
}

func TestKeyFromEnv(t *testing.T) {
	t.Setenv("ROOST_RELAY_TOKEN_TEST_KEY", "  "+string(testKey)+"\n")
	key, err := KeyFromEnv("ROOST_RELAY_TOKEN_TEST_KEY")
	if err != nil || string(key) != string(testKey) {
		t.Fatalf("KeyFromEnv = %q, %v", key, err)
	}
	t.Setenv("ROOST_RELAY_TOKEN_TEST_SHORT", "short-secret")
	for _, name := range []string{"", "ROOST_RELAY_TOKEN_TEST_UNSET", "ROOST_RELAY_TOKEN_TEST_SHORT"} {
		_, err := KeyFromEnv(name)
		if !errors.Is(err, ErrKey) {
			t.Errorf("KeyFromEnv(%q) = %v, want ErrKey", name, err)
		}
		if err != nil && strings.Contains(err.Error(), "short-secret") {
			t.Errorf("KeyFromEnv(%q) echoed the value: %v", name, err)
		}
	}
}
