package security

import (
	"encoding/base64"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"
)

// forge builds a token whose payload is exactly `payload`, signed with the
// right secret, so that only the payload rule under test can refuse it.
func forge(payload, secret string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(payload)) + "." + base64.RawURLEncoding.EncodeToString(sign(payload, secret))
}

// A session token is the only thing standing between a socket and a player
// id. Every way it can be malformed, forged, or replayed against the wrong
// player is pinned; only round trip, wrong player and expiry had tests.
func TestSessionTokenRefusesEachMalformedOrForgedToken(t *testing.T) {
	now := time.Unix(1_000_000, 0)
	future := now.Add(time.Hour).UnixMilli()
	good, err := SignSessionToken(42, "secret", time.Hour, now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := SignSessionToken(0, "secret", time.Hour, now); !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "player id required") {
		t.Fatalf("sign for player 0 = %v", err)
	}
	if _, err := SignSessionToken(42, "", time.Hour, now); !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "secret required") {
		t.Fatalf("sign without secret = %v", err)
	}
	if _, err := VerifySessionToken(good, "", 42, now); !errors.Is(err, ErrTokenInvalid) || !strings.Contains(err.Error(), "secret required") {
		t.Fatalf("verify without secret = %v", err)
	}

	payloadOf := func(token string) string {
		raw, err := base64.RawURLEncoding.DecodeString(strings.SplitN(token, ".", 2)[0])
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	nonce := strings.Split(payloadOf(good), ":")[2]
	sigOf := func(token string) string { return strings.SplitN(token, ".", 2)[1] }

	cases := []struct {
		name   string
		token  string
		want   error
		expect int64 // 0 = any player, so the payload rule itself is what refuses
	}{
		{"no separator", strings.ReplaceAll(good, ".", "_"), ErrTokenInvalid, 42},
		{"three segments", good + ".extra", ErrTokenInvalid, 42},
		{"payload not base64", "!!!." + sigOf(good), ErrTokenInvalid, 42},
		{"signature not base64", strings.SplitN(good, ".", 2)[0] + ".!!!", ErrTokenInvalid, 42},
		{"signed with another secret", forge(payloadOf(good), "other-secret"), ErrTokenInvalid, 42},
		{"payload edited after signing", base64.RawURLEncoding.EncodeToString([]byte("43:"+strings.SplitN(payloadOf(good), ":", 2)[1])) + "." + sigOf(good), ErrTokenInvalid, 42},
		{"payload with two fields", forge("42:"+nonce, "secret"), ErrTokenInvalid, 42},
		{"payload with four fields", forge(payloadOf(good)+":more", "secret"), ErrTokenInvalid, 42},
		{"player id not a number", forge("abc:"+itoa(future)+":"+nonce, "secret"), ErrTokenInvalid, 0},
		{"player id zero", forge("0:"+itoa(future)+":"+nonce, "secret"), ErrTokenInvalid, 0},
		{"expiry not a number", forge("42:soon:"+nonce, "secret"), ErrTokenInvalid, 42},
		{"expiry not positive", forge("42:0:"+nonce, "secret"), ErrTokenInvalid, 42},
		{"empty nonce", forge("42:"+itoa(future)+":", "secret"), ErrTokenInvalid, 42},
		{"expired", forge("42:"+itoa(now.UnixMilli())+":"+nonce, "secret"), ErrTokenExpired, 42},
		{"another player's token", good, ErrTokenInvalid, 43},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := VerifySessionToken(tc.token, "secret", tc.expect, now)
			if !errors.Is(err, tc.want) {
				t.Fatalf("Verify = %v, want %v", err, tc.want)
			}
		})
	}
	if claims, err := VerifySessionToken(good, "secret", 0, now); err != nil || claims.PlayerID != 42 {
		t.Fatalf("expectPlayerID 0 must accept any player: %+v, %v", claims, err)
	}
}

func itoa(v int64) string { return strconv.FormatInt(v, 10) }
