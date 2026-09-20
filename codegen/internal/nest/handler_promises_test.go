package nest

import (
	"strings"
	"testing"
)

// Method handlers are invoked through an injected receiver and targets are
// resolved by name; a value receiver would be invoked on a copy, and an
// ambiguous or blank target declaration would bind the handler to nothing.
func TestParseFileRefusesValueReceiversAndBadTargetDeclarations(t *testing.T) {
	cases := []struct{ label, source, want string }{
		{"value receiver", `package capability
type BagOwner interface { Bag() any }
type Handler struct{}
//roost:nest target=player
func (h Handler) handlerUse(owner BagOwner) error { return nil }
`, "method handler receiver must be a pointer"},
		{"target and targets together", `package capability
type BagOwner interface { Bag() any }
//roost:nest target=player targets=player
func handlerBoth(owner BagOwner) error { return nil }
`, "target and targets cannot be used together"},
		{"empty target name", `package capability
type BagOwner interface { Bag() any }
type GuildOwner interface { Guild() any }
//roost:nest targets=player,,alliance
func handlerBlank(owner BagOwner, guild GuildOwner) error { return nil }
`, "target names must not be empty"},
	}
	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			path := writeTempGoFile(t, tc.source)
			if _, _, err := parseFile(path); err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("parseFile = %v, want %q", err, tc.want)
			}
		})
	}
}
