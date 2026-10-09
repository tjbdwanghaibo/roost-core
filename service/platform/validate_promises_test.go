package platform

import (
	"errors"
	"strings"
	"testing"
)

func validOrder() Order {
	return Order{OrderID: "o-1", PlayerID: 7, Channel: "appstore", ProductID: "gems.100", AmountMinor: 499, Currency: "USD", MaxAttempts: 3}
}

// Order.Validate stands between a provider callback and goods delivery. The
// amount rule in particular is the "free money" guard; each rule is pinned by
// message so a dropped one turns exactly one case red.
func TestOrderValidateRefusesEachBrokenField(t *testing.T) {
	if err := validOrder().Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	cases := []struct {
		name   string
		mutate func(*Order)
		text   string
	}{
		{"blank order id", func(o *Order) { o.OrderID = " " }, "order id is empty"},
		{"order id too long", func(o *Order) { o.OrderID = strings.Repeat("o", MaxOrderIDBytes+1) }, "order id is"},
		{"player id not positive", func(o *Order) { o.PlayerID = 0 }, "player id must be positive"},
		{"blank product id", func(o *Order) { o.ProductID = "" }, "product id is empty"},
		{"product id too long", func(o *Order) { o.ProductID = strings.Repeat("p", MaxProductIDBytes+1) }, "product id is"},
		{"zero amount", func(o *Order) { o.AmountMinor = 0 }, "paid amount must be positive, got 0"},
		{"negative amount", func(o *Order) { o.AmountMinor = -1 }, "paid amount must be positive, got -1"},
		{"no attempt budget", func(o *Order) { o.MaxAttempts = 0 }, "max attempts must be positive"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			o := validOrder()
			tc.mutate(&o)
			err := o.Validate()
			if !errors.Is(err, ErrOrderInvalid) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want ErrOrderInvalid containing %q", err, tc.text)
			}
		})
	}
}

// A credential without a secret is exactly the shape the replaced login
// accepted; the verifier's answer must name a channel and an open id or the
// player id derived from it is meaningless.
func TestCredentialAndVerifiedValidateRefuseEachBlankField(t *testing.T) {
	good := Credential{Channel: "wechat", OpenID: "open-1", Secret: "ticket"}
	if err := good.Validate(); err != nil {
		t.Fatalf("baseline rejected: %v", err)
	}
	for name, tc := range map[string]struct {
		mutate func(*Credential)
		text   string
	}{
		"blank channel":    {func(c *Credential) { c.Channel = " " }, "channel is empty"},
		"blank open id":    {func(c *Credential) { c.OpenID = "" }, "open id is empty"},
		"open id too long": {func(c *Credential) { c.OpenID = strings.Repeat("x", MaxOpenIDBytes+1) }, "open id is"},
		"blank secret":     {func(c *Credential) { c.Secret = "  " }, "credential secret is empty"},
	} {
		t.Run(name, func(t *testing.T) {
			c := good
			tc.mutate(&c)
			err := c.Validate()
			if !errors.Is(err, ErrRequestInvalid) || !strings.Contains(err.Error(), tc.text) {
				t.Fatalf("Validate = %v, want ErrRequestInvalid containing %q", err, tc.text)
			}
		})
	}
	if err := (Verified{Channel: "wechat", OpenID: "open-1"}).Validate(); err != nil {
		t.Fatalf("verified baseline rejected: %v", err)
	}
	if err := (Verified{OpenID: "open-1"}).Validate(); !errors.Is(err, ErrIdentityDenied) || !strings.Contains(err.Error(), "verifier returned an empty channel") {
		t.Fatalf("verified without channel = %v", err)
	}
	if err := (Verified{Channel: "wechat"}).Validate(); !errors.Is(err, ErrIdentityDenied) || !strings.Contains(err.Error(), "verifier returned an empty open id") {
		t.Fatalf("verified without open id = %v", err)
	}
}
