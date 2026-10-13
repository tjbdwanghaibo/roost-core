package tablegen

import (
	"io"
	"strings"
	"testing"
)

// 未知 flag 报错；多余的位置参数以 "unexpected arguments" 点名拒绝，而不是被忽略。
func TestRunReturnsFlagErrors(t *testing.T) {
	if err := Run([]string{"-unknown"}, io.Discard); err == nil {
		t.Fatal("unknown flag unexpectedly accepted")
	}
	if err := Run([]string{"stray"}, io.Discard); err == nil || !strings.Contains(err.Error(), `unexpected arguments ["stray"]`) {
		t.Fatalf("Run with a positional argument = %v, want it named as unexpected", err)
	}
}
