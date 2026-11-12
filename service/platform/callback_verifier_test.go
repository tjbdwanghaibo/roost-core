package platform

import (
	"context"
	"errors"
	"testing"
)

type gatedDeliverer struct {
	*recordingDeliverer
	checks int
	deny   error
}

func (d *gatedDeliverer) VerifyCallback(context.Context, []byte, string) error {
	d.checks++
	return d.deny
}

func TestCallbackVerifierRejectsBeforeOrderReservation(t *testing.T) {
	delivery := &gatedDeliverer{recordingDeliverer: newDeliverer(), deny: ErrSignatureInvalid}
	h := newHarness(t, func(cfg *Config) { cfg.Deliver = delivery })
	raw, signature := callback(t, "gated", 1001, 99)
	if _, err := h.service.HandleCallback(context.Background(), raw, signature); !errors.Is(err, ErrSignatureInvalid) {
		t.Fatalf("err=%v", err)
	}
	_, found, err := h.orders.Get(context.Background(), "gated")
	if err != nil || found || delivery.checks != 1 || delivery.grantsFor("gated") != 0 {
		t.Fatalf("found=%v checks=%d grants=%d err=%v", found, delivery.checks, delivery.grantsFor("gated"), err)
	}
}
func TestCallbackVerifierDoesNotBypassHMAC(t *testing.T) {
	delivery := &gatedDeliverer{recordingDeliverer: newDeliverer()}
	h := newHarness(t, func(cfg *Config) { cfg.Deliver = delivery })
	raw, _ := callback(t, "gated", 1001, 99)
	if _, err := h.service.HandleCallback(context.Background(), raw, "invalid"); !errors.Is(err, ErrSignatureInvalid) || delivery.checks != 0 {
		t.Fatalf("err=%v checks=%d", err, delivery.checks)
	}
}
func TestCallbackVerifierAllowsNormalDeduplication(t *testing.T) {
	delivery := &gatedDeliverer{recordingDeliverer: newDeliverer()}
	h := newHarness(t, func(cfg *Config) { cfg.Deliver = delivery })
	raw, signature := callback(t, "gated", 1001, 99)
	if _, err := h.service.HandleCallback(context.Background(), raw, signature); err != nil {
		t.Fatal(err)
	}
	if _, err := h.service.HandleCallback(context.Background(), raw, signature); err != nil {
		t.Fatal(err)
	}
	if delivery.grantsFor("gated") != 1 || delivery.checks != 2 {
		t.Fatalf("checks=%d grants=%d", delivery.checks, delivery.grantsFor("gated"))
	}
}
