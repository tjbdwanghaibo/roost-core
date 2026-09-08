package skill

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
)

func deriveCastRandomKey(seed [32]byte, gameplayDigest string, caster EntityID, sequence uint64) [32]byte {
	hash := hmac.New(sha256.New, seed[:])
	writeRandomPart(hash.Write, []byte("roost.skill/v2/cast-random"))
	writeRandomPart(hash.Write, []byte(gameplayDigest))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(caster))
	writeRandomPart(hash.Write, number[:])
	binary.BigEndian.PutUint64(number[:], sequence)
	writeRandomPart(hash.Write, number[:])
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func randomCandidateScore(key [32]byte, site RandomSiteIndex, invocation, stableID uint64) [32]byte {
	hash := hmac.New(sha256.New, key[:])
	writeRandomPart(hash.Write, []byte("roost.skill/v2/random-site"))
	var number [8]byte
	binary.BigEndian.PutUint64(number[:], uint64(site))
	writeRandomPart(hash.Write, number[:])
	binary.BigEndian.PutUint64(number[:], invocation)
	writeRandomPart(hash.Write, number[:])
	binary.BigEndian.PutUint64(number[:], stableID)
	writeRandomPart(hash.Write, number[:])
	var result [32]byte
	copy(result[:], hash.Sum(nil))
	return result
}

func writeRandomPart(write func([]byte) (int, error), value []byte) {
	var length [8]byte
	binary.BigEndian.PutUint64(length[:], uint64(len(value)))
	_, _ = write(length[:])
	_, _ = write(value)
}

func boundedRandom(key [32]byte, domain []byte, bound uint64) uint64 {
	if bound == 0 {
		return 0
	}
	threshold := -bound % bound
	for counter := uint64(0); ; counter++ {
		hash := hmac.New(sha256.New, key[:])
		writeRandomPart(hash.Write, domain)
		var encoded [8]byte
		binary.BigEndian.PutUint64(encoded[:], counter)
		writeRandomPart(hash.Write, encoded[:])
		value := binary.BigEndian.Uint64(hash.Sum(nil)[:8])
		if value >= threshold {
			return value % bound
		}
	}
}
