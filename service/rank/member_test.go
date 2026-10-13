// 排行成员编码：字节序即排名序、定宽字段、往返与确定性、畸形成员逐项拒绝、请求环的升级与畸形拒绝。

package rank

import (
	"context"
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
		"too few fields":     strings.Join(parts[:4], memberFieldSeparator),
		"too many fields":    good + memberFieldSeparator + "extra",
		"value not hex":      strings.Join(append([]string{"zz"}, parts[1:]...), memberFieldSeparator),
		"tie not hex":        strings.Join([]string{parts[0], "zz", parts[2], parts[3], parts[4]}, memberFieldSeparator),
		"brief not base64":   strings.Join([]string{parts[0], parts[1], parts[2], parts[3], "!!!"}, memberFieldSeparator),
		"empty":              "",
		"owner not a number": "0000000000000000:0000000000000000:0000000000000000:notanumber:",
		"one long token":     strings.Repeat("a", 200),
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

func TestLegacyRequestRingUpgradesWithoutDroppingPriorIDs(t *testing.T) {
	s, f := newStore(t)
	ctx := context.Background()
	submit(t, s, 1, 10, UpdateSet, "")
	f.mu.Lock()
	f.hset(s.ownerKey(arena()), "1:applied", "old-a,old-b")
	f.mu.Unlock()
	submit(t, s, 1, 1, UpdateAdd, "new,comma")
	replay := submit(t, s, 1, 100, UpdateAdd, "old-b")
	if replay.Score.Value != 11 {
		t.Fatalf("legacy proof lost: %+v", replay)
	}
	replay = submit(t, s, 1, 100, UpdateAdd, "new,comma")
	if replay.Score.Value != 11 {
		t.Fatal(replay)
	}
	if err := s.Remove(ctx, arena(), 1); err != nil {
		t.Fatal(err)
	}
	fresh := submit(t, s, 1, 7, UpdateAdd, "new,comma")
	if fresh.Score.Value != 7 {
		t.Fatalf("removed owner retained ledger: %+v", fresh)
	}
}
func TestMalformedRequestRingFailsClosed(t *testing.T) {
	s, f := newStore(t)
	submit(t, s, 1, 10, UpdateSet, "")
	f.mu.Lock()
	f.hset(s.ownerKey(arena()), "1:applied_v2", "invalid-json")
	f.mu.Unlock()
	if _, err := s.Submit(context.Background(), arena(), Score{OwnerID: 1, Value: 5}, UpdateAdd, "retry"); err == nil {
		t.Fatal("malformed ledger silently discarded")
	}
	page, err := s.Page(context.Background(), arena(), 0, 1)
	if err != nil || len(page.Entries) != 1 || page.Entries[0].Score.Value != 10 {
		t.Fatal(page, err)
	}
}
