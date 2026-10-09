package rank

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
)

// The ordering member.
//
// Everything a ranked page needs — value, tiebreak, owner — is encoded into
// the sorted-set member, so reading a page is one round trip and a rank can
// never be shown next to a score from a different read. The implementation
// this replaces kept ordering in a sorted set and payload in a hash, read them
// separately, and could show a new score at an old rank.
//
// Every element is a fixed-width, order-preserving encoding, so byte order on
// the member is exactly the ranking order under a reverse range scan:
//
//	<value desc> : <tie asc> : <owner asc> : <owner decimal>
//
// The trailing decimal owner is what makes a member addressable: given an
// owner id we can find its member without a second index, because the prefix
// is derivable only from the record we already hold.
const memberFieldSeparator = ":"

// sortableUint maps an int64 onto a uint64 whose unsigned order matches the
// signed order of the input, so a fixed-width hex rendering sorts correctly
// including across zero.
func sortableUint(value int64) uint64 {
	return uint64(value) ^ (1 << 63)
}

// encodeMember renders the ordering member for a score.
//
// Under a reverse (descending) range scan: larger Value first, then smaller
// Tie first, then smaller OwnerID first. Value is stored uncomplemented and
// Tie/Owner complemented, because the scan is reversed — the complement is
// what turns "larger first" into "smaller first" for those two fields.
func encodeMember(score Score) string {
	var b strings.Builder
	b.Grow(64)
	fmt.Fprintf(&b, "%016x", sortableUint(score.Value))
	b.WriteString(memberFieldSeparator)
	fmt.Fprintf(&b, "%016x", ^sortableUint(score.Tie))
	b.WriteString(memberFieldSeparator)
	fmt.Fprintf(&b, "%016x", ^sortableUint(score.OwnerID))
	b.WriteString(memberFieldSeparator)
	fmt.Fprintf(&b, "%020d", score.OwnerID)
	return b.String()
}

// encodeEntry renders the full stored member: the ordering prefix plus the
// opaque brief. The brief is base64 so it cannot contain the separator or a
// byte that would disturb ordering, and it sits last so it never affects rank.
func encodeEntry(score Score) string {
	member := encodeMember(score)
	if len(score.Brief) == 0 {
		return member + memberFieldSeparator
	}
	return member + memberFieldSeparator + base64.RawStdEncoding.EncodeToString(score.Brief)
}

// decodeEntry parses a stored member back into a score. It is strict: a member
// it cannot parse is an error, never a zero score. Decoding garbage to a zero
// and carrying on is how a corrupt row becomes a wrong write.
func decodeEntry(raw string) (Score, error) {
	parts := strings.Split(raw, memberFieldSeparator)
	if len(parts) != 5 {
		return Score{}, fmt.Errorf("rank: member has %d fields, want 5: %q", len(parts), raw)
	}
	value, err := strconv.ParseUint(parts[0], 16, 64)
	if err != nil {
		return Score{}, fmt.Errorf("rank: member value: %w", err)
	}
	tie, err := strconv.ParseUint(parts[1], 16, 64)
	if err != nil {
		return Score{}, fmt.Errorf("rank: member tie: %w", err)
	}
	owner, err := strconv.ParseInt(parts[3], 10, 64)
	if err != nil {
		return Score{}, fmt.Errorf("rank: member owner: %w", err)
	}
	score := Score{
		OwnerID: owner,
		Value:   int64(value ^ (1 << 63)),
		Tie:     int64((^tie) ^ (1 << 63)),
	}
	if parts[4] != "" {
		brief, err := base64.RawStdEncoding.DecodeString(parts[4])
		if err != nil {
			return Score{}, fmt.Errorf("rank: member brief: %w", err)
		}
		score.Brief = brief
	}
	return score, nil
}
