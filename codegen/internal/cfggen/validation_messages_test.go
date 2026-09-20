package cfggen

import (
	"strings"
	"testing"
)

// TestCfggenRejectsBrokenMeta only asks for SOME error, so a meta with a
// missing key was rejected by the "key not declared" check when the "key is
// required" check was removed, and vice versa — each check covered for the
// other. Here every case must fail for the reason it names (U-0034).
func TestCfggenRejectsBrokenMetaForTheStatedReason(t *testing.T) {
	cases := map[string][2]string{
		"missing key":      {"tables:\n  - name: t\n    fields:\n      - { name: id, type: int32 }\n", "key is required"},
		"key not declared": {"tables:\n  - name: t\n    key: nope\n    fields:\n      - { name: id, type: int32 }\n", "key field \"nope\" not declared"},
		"bean keyword":     {"beans:\n  - name: type\n    fields:\n      - { name: id, type: int32 }\n", "is a Go keyword"},
		"bean predeclared": {"beans:\n  - name: string\n    fields:\n      - { name: id, type: int32 }\n", "shadows a predeclared Go identifier"},
	}
	for name, testCase := range cases {
		_, err := runCfggen(t, testCase[0])
		if err == nil {
			t.Fatalf("%s: accepted", name)
		}
		if !strings.Contains(err.Error(), testCase[1]) {
			t.Fatalf("%s: error %q does not contain %q", name, err, testCase[1])
		}
	}
}
