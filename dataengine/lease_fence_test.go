package dataengine

import (
	"bytes"
	"errors"
	"testing"
)

func TestLeaseFenceReceiptRejectsIdentityAndPayloadTampering(t *testing.T) {
	receipt, err := NewLeaseFenceReceipt(LeaseFence{
		Database: "game", Resource: "_claims", DocumentID: "saga-step/cmd-1", Owner: "worker-1", Token: 7,
		Digest: bytes.Repeat([]byte{1}, 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if fence, control, err := DecodeLeaseFenceReceipt(receipt); err != nil || !control || fence.Token != 7 {
		t.Fatalf("fence=%+v control=%t err=%v", fence, control, err)
	}

	tamperedID := receipt
	tamperedID.Digest = append([]byte(nil), receipt.Digest...)
	tamperedID.Payload = append([]byte(nil), receipt.Payload...)
	tamperedID.ID = "game/_claims/saga-step/another"
	if _, control, err := DecodeLeaseFenceReceipt(tamperedID); !control || !errors.Is(err, ErrInvalidLeaseFence) {
		t.Fatalf("tampered ID control=%t err=%v", control, err)
	}

	tamperedPayload := receipt
	tamperedPayload.Digest = append([]byte(nil), receipt.Digest...)
	tamperedPayload.Payload = append([]byte(nil), receipt.Payload...)
	tamperedPayload.Payload[len(tamperedPayload.Payload)-2] = '8'
	if _, control, err := DecodeLeaseFenceReceipt(tamperedPayload); !control || !errors.Is(err, ErrInvalidLeaseFence) {
		t.Fatalf("tampered payload control=%t err=%v", control, err)
	}
}
