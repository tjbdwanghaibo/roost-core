package rank

import (
	"strings"
	"testing"
)

// decodeEntry is strict on purpose: a member it cannot parse is an error,
// never a zero score, because a zero score that is written back would replace
// a real one. Each way a stored member can be malformed is pinned.
func TestDecodeEntryRefusesEachMalformedMember(t *testing.T) {
	good := encodeEntry(Score{OwnerID: 42, Value: 900, Tie: 7, Brief: []byte("brief")})
	decoded, err := decodeEntry(good)
	if err != nil || decoded.OwnerID != 42 || decoded.Value != 900 || decoded.Tie != 7 || string(decoded.Brief) != "brief" {
		t.Fatalf("round trip = %+v, %v", decoded, err)
	}
	parts := strings.Split(good, memberFieldSeparator)
	cases := map[string]string{
		"too few fields":   strings.Join(parts[:4], memberFieldSeparator),
		"too many fields":  good + memberFieldSeparator + "extra",
		"value not hex":    strings.Join(append([]string{"zz"}, parts[1:]...), memberFieldSeparator),
		"tie not hex":      strings.Join([]string{parts[0], "zz", parts[2], parts[3], parts[4]}, memberFieldSeparator),
		"brief not base64": strings.Join([]string{parts[0], parts[1], parts[2], parts[3], "!!!"}, memberFieldSeparator),
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			got, err := decodeEntry(raw)
			if err == nil {
				t.Fatalf("malformed member %q decoded to %+v", raw, got)
			}
			if got.OwnerID != 0 || got.Value != 0 || got.Tie != 0 || len(got.Brief) != 0 {
				t.Fatalf("a failed decode must not hand back a partial score: %+v", got)
			}
		})
	}
}
