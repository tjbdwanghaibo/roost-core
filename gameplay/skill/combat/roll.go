package combat

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

// Deterministic chance rolls. The damage pipeline takes avoidance and
// critical outcomes as pre-rolled facts (Dodge, ForceCritical, ...); these
// helpers are the standard way for a host to produce those facts without
// wall-clock or math/rand state: the same key, purpose, and coordinates
// always yield the same roll, so replays and replicas agree bit-exactly.
//
// The key is a match- or cast-scoped secret (skill hosts typically derive
// it from the match seed). Coordinates pin the roll to one gameplay moment —
// for a skill effect command, the recommended coordinates are the event's
// RootEventID, EventID, EffectIndex, and the target entity id, which makes
// every damage instance roll independently while staying reproducible.

// RollValue derives a uniform value in [0, BasisPointScale) from the key,
// purpose, and coordinates. Distinct purposes ("crit", "dodge") roll
// independently at the same coordinates.
func RollValue(key []byte, purpose string, coordinates ...uint64) int64 {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(purpose))
	var buffer [8]byte
	for _, coordinate := range coordinates {
		binary.BigEndian.PutUint64(buffer[:], coordinate)
		_, _ = mac.Write(buffer[:])
	}
	digest := mac.Sum(nil)
	// Modulo bias over 2^64 samples is < 1e-15 for a 10000-sized range —
	// negligible against gameplay chance granularity.
	return int64(binary.BigEndian.Uint64(digest[:8]) % BasisPointScale)
}

// ChanceRoll reports whether a chance given in basis points succeeds at the
// given coordinates: chanceBP <= 0 never succeeds, chanceBP >= 10000 always
// succeeds.
func ChanceRoll(key []byte, purpose string, chanceBP int64, coordinates ...uint64) bool {
	if chanceBP <= 0 {
		return false
	}
	if chanceBP >= BasisPointScale {
		return true
	}
	return RollValue(key, purpose, coordinates...) < chanceBP
}
