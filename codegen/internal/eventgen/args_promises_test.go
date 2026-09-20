package eventgen

import (
	"io"
	"strings"
	"testing"
)

// U-0151 · C2 · gap map codegen `internal/eventgen` 1/1：多余的位置参数以"unexpected arguments"拒绝，而不是被忽略。
func TestRunNamesUnexpectedPositionalArguments(t *testing.T) {
	if err := Run([]string{"stray"}, io.Discard); err == nil || !strings.Contains(err.Error(), `unexpected arguments ["stray"]`) {
		t.Fatalf("Run with a positional argument = %v", err)
	}
}
