package attribute

import (
	"strings"
	"testing"
)

// U-0151 · C2 · gap map codegen `internal/attribute` 1/10：非数字 / 非正数拒绝，空值取回退值。
func TestParsePositiveIntRefusesNonPositiveValues(t *testing.T) {
	for _, raw := range []string{"abc", "0", "-3"} {
		if _, err := parsePositiveInt(raw, 4); err == nil || !strings.Contains(err.Error(), "invalid positive int") {
			t.Fatalf("parsePositiveInt(%q) = %v", raw, err)
		}
	}
	if v, err := parsePositiveInt("", 4); err != nil || v != 4 {
		t.Fatalf("parsePositiveInt(\"\") = (%d, %v), want the fallback", v, err)
	}
	if v, err := parsePositiveInt("7", 4); err != nil || v != 7 {
		t.Fatalf("parsePositiveInt(\"7\") = (%d, %v)", v, err)
	}
}
