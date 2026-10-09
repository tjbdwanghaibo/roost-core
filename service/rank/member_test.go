package rank

import (
	"math"
	"sort"
	"strings"
	"testing"
)

// Byte order on the member must be exactly the ranking order under a reverse
// scan. Everything else in the service depends on this, so it is checked
// against an independently written comparator rather than against itself.
func TestMemberByteOrderIsTheRankingOrder(t *testing.T) {
	scores := []Score{
		{OwnerID: 1, Value: 100, Tie: 50},
		{OwnerID: 2, Value: 100, Tie: 10}, // same value, earlier tie -> ahead of owner 1
		{OwnerID: 3, Value: 200, Tie: 99}, // highest value -> first
		{OwnerID: 4, Value: -5, Tie: 0},   // negative sorts below zero
		{OwnerID: 5, Value: 0, Tie: 0},
		{OwnerID: 6, Value: 100, Tie: 10}, // ties with owner 2 on value+tie -> lower id ahead
		{OwnerID: math.MaxInt64, Value: math.MaxInt64, Tie: math.MinInt64},
		{OwnerID: math.MinInt64, Value: math.MinInt64, Tie: math.MaxInt64},
	}

	// The intended order, written independently of the encoding.
	want := append([]Score(nil), scores...)
	sort.SliceStable(want, func(i, j int) bool {
		if want[i].Value != want[j].Value {
			return want[i].Value > want[j].Value // higher value first
		}
		if want[i].Tie != want[j].Tie {
			return want[i].Tie < want[j].Tie // smaller tie first
		}
		return want[i].OwnerID < want[j].OwnerID // smaller owner first
	})

	// What a reverse lexicographic scan over the members produces.
	members := make([]string, 0, len(scores))
	byMember := map[string]Score{}
	for _, score := range scores {
		member := encodeMember(score)
		members = append(members, member)
		byMember[member] = score
	}
	sort.Sort(sort.Reverse(sort.StringSlice(members)))

	for i, member := range members {
		got := byMember[member]
		if got.OwnerID != want[i].OwnerID {
			t.Fatalf("position %d: byte order gives owner %d, ranking order wants %d\nmembers: %v",
				i, got.OwnerID, want[i].OwnerID, members)
		}
	}
}

// All ordering fields are fixed width, or a shorter rendering would sort
// before a longer one regardless of value.
func TestMemberFieldsAreFixedWidth(t *testing.T) {
	widths := map[int]int{0: 16, 1: 16, 2: 16, 3: 20}
	for _, score := range []Score{
		{OwnerID: 1, Value: 0, Tie: 0},
		{OwnerID: math.MaxInt64, Value: math.MaxInt64, Tie: math.MaxInt64},
		{OwnerID: math.MinInt64, Value: math.MinInt64, Tie: math.MinInt64},
		{OwnerID: -1, Value: -1, Tie: -1},
	} {
		parts := strings.Split(encodeMember(score), memberFieldSeparator)
		if len(parts) != 4 {
			t.Fatalf("%+v encoded to %d fields", score, len(parts))
		}
		for index, width := range widths {
			if len(parts[index]) != width {
				t.Fatalf("%+v field %d is %d chars, want %d: %q", score, index, len(parts[index]), width, parts[index])
			}
		}
	}
}

// The full stored entry round-trips, and the brief never disturbs ordering.
func TestEntryRoundTripsAndBriefDoesNotAffectOrder(t *testing.T) {
	for _, score := range []Score{
		{OwnerID: 7, Value: 42, Tie: 9},
		{OwnerID: 8, Value: -42, Tie: -9, Brief: []byte(`{"name":"a:b:c"}`)},
		{OwnerID: 9, Value: 0, Tie: 0, Brief: []byte{0x00, 0xff, 0x3a}},
		{OwnerID: math.MinInt64, Value: math.MaxInt64, Tie: math.MinInt64, Brief: []byte("")},
	} {
		got, err := decodeEntry(encodeEntry(score))
		if err != nil {
			t.Fatalf("%+v: %v", score, err)
		}
		if got.OwnerID != score.OwnerID || got.Value != score.Value || got.Tie != score.Tie {
			t.Fatalf("round trip changed the score: got %+v want %+v", got, score)
		}
		if len(score.Brief) == 0 {
			if len(got.Brief) != 0 {
				t.Fatalf("empty brief round-tripped to %q", got.Brief)
			}
			continue
		}
		if string(got.Brief) != string(score.Brief) {
			t.Fatalf("brief round-tripped to %q, want %q", got.Brief, score.Brief)
		}
	}

	// A brief that contains the separator must not change where the entry
	// sorts: the ordering prefix is identical either way.
	plain := encodeEntry(Score{OwnerID: 1, Value: 5, Tie: 1})
	withBrief := encodeEntry(Score{OwnerID: 1, Value: 5, Tie: 1, Brief: []byte("a:b:c:d:e")})
	if encodeMember(Score{OwnerID: 1, Value: 5, Tie: 1}) != strings.Join(strings.Split(plain, memberFieldSeparator)[:4], memberFieldSeparator) {
		t.Fatal("the ordering prefix is not the first four fields")
	}
	if strings.Split(plain, memberFieldSeparator)[0] != strings.Split(withBrief, memberFieldSeparator)[0] {
		t.Fatal("a brief changed the ordering prefix")
	}
}

// A member that cannot be parsed is an error, never a zero score. Decoding
// garbage to a zero and carrying on is how a corrupt row becomes a wrong
// write, and it is also what manufactured the orphan members that silently
// truncated season archives in the implementation this replaces.
func TestDecodeRejectsMalformedMembers(t *testing.T) {
	for _, raw := range []string{
		"",
		"only:three:fields:here",
		"zz" + strings.Repeat("0", 14) + ":0000000000000000:0000000000000000:00000000000000000001:",
		"0000000000000000:0000000000000000:0000000000000000:notanumber:",
		"0000000000000000:0000000000000000:0000000000000000:00000000000000000001:!!!notbase64",
		strings.Repeat("a", 200),
	} {
		if _, err := decodeEntry(raw); err == nil {
			t.Fatalf("malformed member %q decoded without error", raw)
		}
	}
}

// Two owners with the same value and the same tie must still order
// deterministically, and the same input must always encode identically —
// otherwise a re-submitted identical score would create a second member.
func TestEncodingIsDeterministic(t *testing.T) {
	score := Score{OwnerID: 123, Value: 456, Tie: 789, Brief: []byte("x")}
	first := encodeEntry(score)
	for i := 0; i < 8; i++ {
		if again := encodeEntry(score); again != first {
			t.Fatalf("encoding is not deterministic: %q then %q", first, again)
		}
	}
}
