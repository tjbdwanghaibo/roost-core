package skill

import (
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
)

func canonicalDefinitionDigest(definition *Definition) string {
	// Definition is a closed Wire tree. encoding/json emits struct fields in
	// declaration order and sorts map keys, so it provides a deterministic
	// source representation without reflecting over semantic variants.
	payload, err := json.Marshal(definition)
	if err != nil {
		panic(err)
	}
	return stableDigest("roost.skill/v2/source-document", payload)
}

func stableDigest(domain string, payload []byte) string {
	hash := sha256.New()
	writeDigestPart := func(part []byte) {
		var length [8]byte
		binary.BigEndian.PutUint64(length[:], uint64(len(part)))
		_, _ = hash.Write(length[:])
		_, _ = hash.Write(part)
	}
	writeDigestPart([]byte(domain))
	writeDigestPart(payload)
	return hex.EncodeToString(hash.Sum(nil))
}
