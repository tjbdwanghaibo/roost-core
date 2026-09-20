package entity

import (
	"io"
	"testing"
)

func TestRunReturnsFlagErrors(t *testing.T) {
	if err := Run([]string{"-unknown"}, io.Discard); err == nil {
		t.Fatal("unknown flag unexpectedly accepted")
	}
	if err := Run([]string{"unexpected"}, io.Discard); err == nil {
		t.Fatal("unexpected positional argument accepted")
	}
}
