package mods

import (
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
)

// A capability name is a global key in the registry, so a duplicate is a
// runtime failure that two separately-correct packages can cause together.
// The table exists to make that visible by reading — which only works if
// something checks it.
func TestEveryCapabilityNameIsUnique(t *testing.T) {
	seen := map[string]int{}
	for _, name := range All {
		if strings.TrimSpace(string(name)) == "" {
			t.Fatal("the table contains an empty capability name")
		}
		seen[string(name)]++
	}
	for name, count := range seen {
		if count != 1 {
			t.Fatalf("capability %q appears %d times in the table", name, count)
		}
	}
	if len(seen) != len(All) {
		t.Fatalf("the table holds %d entries but only %d distinct names", len(All), len(seen))
	}
}

// Every name is namespaced, so a service cannot collide with a roost-kit
// infrastructure capability — a service named "chat" and a hypothetical kit
// transport named "chat" would otherwise be the same key.
func TestEveryCapabilityNameIsNamespaced(t *testing.T) {
	for _, name := range All {
		if !strings.HasPrefix(string(name), "service.") {
			t.Fatalf("capability %q is not namespaced under \"service.\"", name)
		}
	}
}

func TestKeyPrefixIsRequiredAndRejectsWhitespace(t *testing.T) {
	cases := []struct {
		name  string
		value string
		set   bool
	}{
		{"unset", "", false},
		{"empty", "", true},
		{"whitespace only", "   ", true},
		{"embedded space", "roost mail", true},
		{"embedded tab", "roost\tmail", true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := viper.New()
			if tc.set {
				cfg.Set("mail.key_prefix", tc.value)
			}
			if _, err := KeyPrefix(cfg, "mail"); err == nil {
				t.Fatalf("KeyPrefix accepted %s", tc.name)
			}
		})
	}
	cfg := viper.New()
	cfg.Set("mail.key_prefix", "  roost:mail  ")
	prefix, err := KeyPrefix(cfg, "mail")
	if err != nil {
		t.Fatal(err)
	}
	if prefix != "roost:mail" {
		t.Fatalf("KeyPrefix returned %q, want the trimmed value", prefix)
	}
}

func TestSecretIsRequiredAndMustNotBeEmpty(t *testing.T) {
	cfg := viper.New()
	if _, err := Secret(cfg, "platform.payment_secret"); err == nil {
		t.Fatal("Secret accepted an unset key")
	}
	cfg.Set("platform.payment_secret", "   ")
	if _, err := Secret(cfg, "platform.payment_secret"); err == nil {
		t.Fatal("Secret accepted a whitespace-only value")
	}
	cfg.Set("platform.payment_secret", "s3cret")
	got, err := Secret(cfg, "platform.payment_secret")
	if err != nil || got != "s3cret" {
		t.Fatalf("Secret returned %q err=%v", got, err)
	}
}

// A negative duration is refused rather than clamped: a caller that wrote -1
// meant something, and silently reading it as the default hides the mistake.
func TestDurationRefusesANegativeValueRatherThanClamping(t *testing.T) {
	cfg := viper.New()
	cfg.Set("mail.claim_lease", -time.Second)
	if _, err := Duration(cfg, "mail.claim_lease", time.Minute); err == nil {
		t.Fatal("Duration accepted a negative value")
	}
}

func TestDurationFallsBackWhenUnsetOrZero(t *testing.T) {
	cfg := viper.New()
	got, err := Duration(cfg, "mail.claim_lease", time.Minute)
	if err != nil || got != time.Minute {
		t.Fatalf("unset: got %s err=%v", got, err)
	}
	cfg.Set("mail.claim_lease", time.Duration(0))
	got, err = Duration(cfg, "mail.claim_lease", time.Minute)
	if err != nil || got != time.Minute {
		t.Fatalf("zero: got %s err=%v", got, err)
	}
	cfg.Set("mail.claim_lease", 5*time.Second)
	got, err = Duration(cfg, "mail.claim_lease", time.Minute)
	if err != nil || got != 5*time.Second {
		t.Fatalf("set: got %s err=%v", got, err)
	}
}

// A required duration has no fallback, so zero and unset are both errors. The
// values that use it — a send ledger ttl, a reservation ttl — must exceed the
// caller's retry horizon, and a default would be a guess at someone else's
// transport.
func TestRequiredDurationHasNoFallback(t *testing.T) {
	cfg := viper.New()
	if _, err := RequiredDuration(cfg, "mail.send_ttl"); err == nil {
		t.Fatal("RequiredDuration accepted an unset key")
	}
	cfg.Set("mail.send_ttl", time.Duration(0))
	if _, err := RequiredDuration(cfg, "mail.send_ttl"); err == nil {
		t.Fatal("RequiredDuration accepted zero")
	}
	cfg.Set("mail.send_ttl", -time.Second)
	if _, err := RequiredDuration(cfg, "mail.send_ttl"); err == nil {
		t.Fatal("RequiredDuration accepted a negative value")
	}
	cfg.Set("mail.send_ttl", 24*time.Hour)
	got, err := RequiredDuration(cfg, "mail.send_ttl")
	if err != nil || got != 24*time.Hour {
		t.Fatalf("got %s err=%v", got, err)
	}
}
