// Package rewards is the game's encoding of a mail attachment. The mail
// service carries attachments as opaque bytes and delivers each one exactly
// once (ReserveClaim → CommitClaim); what the bytes mean is the game's
// business, so the producer (the level-up mailer) and the consumer (the
// ClaimMail endpoint) share this package rather than agreeing informally.
package rewards

import (
	"encoding/json"
	"fmt"

	"github.com/tjbdwanghaibo/roost-core/hotcode"
)

// ClaimWindowClosed reports whether a mail recorded as granted, whose
// envelope expires at expiresAtUnix, may be forgotten at nowUnix.
//
// The ledger's memory is bounded by the MAIL's own expiry, which the mail
// service hands over with the reservation (`Claim.ExpiresAtUnix`). After that
// moment the mail cannot be reserved at all, so no replay can reach the grant
// path and the record is dead weight — and not one moment sooner.
//
// The first version of this bounded the ledger by a fixed 31 days and argued
// that it outlived `mail.send_ttl` (720h in the generated configuration).
// That was wrong and review caught it (RR-20260918-05): `send_ttl` only has
// to be positive, so a deployment that keeps mail for 40 days is legal and
// the ledger forgets while the envelope is still claimable. **Asset
// correctness cannot be bound to another service's configuration** — it has
// to be bound to that service's authoritative moment, which is exactly what
// the envelope's expiry is.
//
// A record with no expiry cannot be placed in time, so it is kept rather than
// forgotten: refusing to forget is the safe direction, and an operator can
// see a ledger that stops shrinking.
func ClaimWindowClosed(expiresAtUnix, nowUnix int64) bool {
	if expiresAtUnix <= 0 {
		return false
	}
	return nowUnix > expiresAtUnix
}

// ClaimResult is what the granting transaction reports back. Claimed is false
// for a replay: this mail was already paid for, and the transaction changed
// nothing. BagCount is what the player holds of the item either way, so the
// client's answer is true in both cases.
type ClaimResult struct {
	Claimed  bool
	BagCount int32
}

// Reward is what a mail attachment grants: one stack of one item.
type Reward struct {
	ItemID int64 `json:"item_id"`
	Count  int32 `json:"count"`
}

// LevelUpPatchPoint is the name this game's level-up reward is registered
// under for hot patching (roost-core/hotcode). The service registers the
// point at startup; this package only names it, so a caller cannot register
// it twice by importing something.
const LevelUpPatchPoint = "rewards.level_up"

// LevelUpReward is what reaching a level is worth: one Mana Potion (item
// 1002 in configs/table/item.csv) per level-up. A real game reads a reward
// table; the point here is that the reward is decided where the mail is
// sent, not where it is claimed.
//
// It goes through a PATCH POINT rather than being called directly, and the
// reason is the kind of incident a hot patch is for: a reward table that is
// wrong in production, where the fix is three lines and the deploy is an
// hour. `hotcode.Resolve` hands back the registered function — the original
// until an operator loads a replacement, the replacement afterwards, and the
// original again after a revert — with the fallback used when no point is
// registered at all (a test, a tool, a process that did not install them).
//
// What must NOT go through one: anything whose two versions would have to
// agree about state in flight. A patch swaps the function between calls, so a
// point is safe exactly when each call stands alone, as this one does — it
// reads a level and returns a value.
func LevelUpReward(level int32) Reward {
	return hotcode.Resolve(LevelUpPatchPoint, OriginalLevelUpReward)(level)
}

// OriginalLevelUpReward is what the patch point is registered with, and what
// a revert goes back to. Exported so the service can register it without this
// package deciding when registration happens.
func OriginalLevelUpReward(level int32) Reward { return Reward{ItemID: 1002, Count: 1} }

// Encode renders a reward as the attachment bytes.
func Encode(reward Reward) ([]byte, error) {
	if reward.ItemID <= 0 || reward.Count <= 0 {
		return nil, fmt.Errorf("rewards: item %d x%d is not a grantable reward", reward.ItemID, reward.Count)
	}
	return json.Marshal(reward)
}

// Decode parses attachment bytes; anything that is not a grantable reward is
// refused, so a claim never grants a zero or negative stack.
func Decode(attachment []byte) (Reward, error) {
	var reward Reward
	if err := json.Unmarshal(attachment, &reward); err != nil {
		return Reward{}, fmt.Errorf("rewards: attachment is not a reward: %w", err)
	}
	if reward.ItemID <= 0 || reward.Count <= 0 {
		return Reward{}, fmt.Errorf("rewards: item %d x%d is not a grantable reward", reward.ItemID, reward.Count)
	}
	return reward, nil
}
