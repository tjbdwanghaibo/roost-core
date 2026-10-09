package nats

import (
	"errors"
	"testing"
)

func TestPermanentPreservesCauseAndMarker(t *testing.T) {
	cause := errors.New("bad wire data")
	err := Permanent(cause)
	if !errors.Is(err, cause) || !IsPermanent(err) {
		t.Fatalf("permanent error lost cause or marker: %v", err)
	}
}
