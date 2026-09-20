// Package purchase is what the game process and the platform process both
// have to agree on about a paid order: which products exist, where the
// platform leaves a grant it has recorded, and how long a grant may still be
// claimed.
//
// It is a package rather than two copies because the two ends run in
// different processes. The platform process writes a grant; the game process
// reads it, grants the items and deletes it. A product id that means one
// thing on one side and another on the other is a player paying for something
// they do not receive, and that mistake is invisible until it happens.
package purchase

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Channel is the one payment channel this demo accepts. A real project has
// one per store, each with its own signature scheme and its own verifier.
const Channel = "demo"

// A paid grant does not expire, and the ledger that deduplicates it is not
// pruned. Both were wrong in the first version of this file and are worth
// stating as a rule (RR-20260919-06):
//
//	A dedup record for a PAID asset may not be dropped before the asset is
//	confirmed delivered.
//
// The first version gave a grant thirty days and pruned the ledger on the same
// number, reasoning — correctly, for mail — that admission and pruning must be
// one predicate. What it missed is that a mail envelope has an authoritative
// expiry after which no replay can reach the grant path at all, and a paid
// order has no such moment: the platform service marked it delivered when the
// grant became durable, so a player who does not log in for a month had a paid
// order that was settled on one side and refused on the other. Permanently.
//
// So the window is gone. The cost is a ledger that only grows, and the way to
// bound it is NOT a timer: it is a second authoritative fact — an idempotent
// "this order was fulfilled" the game reports back to the platform service
// after the items are in the bag. Once an order is fulfilled and its grant is
// gone, its ledger entry can be compacted, because nothing can produce that
// grant again. That handshake does not exist yet; until it does, remembering
// is the only safe direction.

// Product is one thing that can be bought. AmountMinor is the price in the
// currency's minor unit, as an integer: money is never a float here.
type Product struct {
	ID          string
	ItemID      int64
	Count       int32
	AmountMinor int64
	Currency    string
}

// catalog is the demo's store. A real project reads this from a config table
// and reconciles it against what the store console says the product costs —
// an amount the server does not check is an amount the client chooses.
var catalog = map[string]Product{
	"potion_pack":  {ID: "potion_pack", ItemID: 1001, Count: 10, AmountMinor: 99, Currency: "USD"},
	"mana_pack":    {ID: "mana_pack", ItemID: 1002, Count: 10, AmountMinor: 99, Currency: "USD"},
	"starter_gear": {ID: "starter_gear", ItemID: 2001, Count: 1, AmountMinor: 499, Currency: "USD"},
}

// Lookup returns a product. An unknown product id is not an error the caller
// may paper over: it means the payment was for something this build cannot
// deliver, and the order has to stay undelivered and visible.
func Lookup(productID string) (Product, bool) {
	product, ok := catalog[strings.TrimSpace(productID)]
	return product, ok
}

// Products lists the catalog in a stable order, for a client that asks what
// is for sale.
func Products() []Product {
	out := make([]Product, 0, len(catalog))
	for _, product := range catalog {
		out = append(out, product)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// GrantsKey is one player's unclaimed grants: order id → Grant. A hash per
// player rather than one global hash, so the game process reads exactly the
// grants of the player it has in hand and never pages a global structure.
func GrantsKey(prefix string, playerID int64) string {
	return prefix + ":grants:" + strconv.FormatInt(playerID, 10)
}

// Grant is a delivery the platform service has accepted responsibility for:
// the payment is recorded, the goods are not in the bag yet.
//
// It carries the product as RESOLVED at delivery time — item id and count,
// not just the product id — so a catalog edit between payment and claim
// cannot change what the player paid for.
type Grant struct {
	OrderID    string `json:"order_id"`
	PlayerID   int64  `json:"player_id"`
	ProductID  string `json:"product_id"`
	ItemID     int64  `json:"item_id"`
	Count      int32  `json:"count"`
	PaidAtUnix int64  `json:"paid_at_unix"`
}

// Encode renders a grant for storage.
func Encode(grant Grant) (string, error) {
	raw, err := json.Marshal(grant)
	if err != nil {
		return "", fmt.Errorf("purchase: encode grant %s: %w", grant.OrderID, err)
	}
	return string(raw), nil
}

// Decode reads a stored grant.
func Decode(raw string) (Grant, error) {
	var grant Grant
	if err := json.Unmarshal([]byte(raw), &grant); err != nil {
		return Grant{}, fmt.Errorf("purchase: decode grant: %w", err)
	}
	if strings.TrimSpace(grant.OrderID) == "" || grant.ItemID <= 0 || grant.Count <= 0 {
		return Grant{}, fmt.Errorf("purchase: stored grant is incomplete: %+v", grant)
	}
	return grant, nil
}

// GrantResult is what the granting transaction reports back. Granted is false
// for a replay — this order was already claimed and the transaction changed
// nothing — and BagCount is what the player holds either way, so the caller's
// answer is true in both cases.
//
// It lives here rather than in the handler package because a Nest handler's
// return type is part of the generated sender's signature, and the generated
// code cannot see a type declared in the handler package itself.
type GrantResult struct {
	Granted  bool
	BagCount int32
}

// Settlement is one grant the drain dealt with: what it was and what the
// player holds afterwards. The endpoint that triggered a drain reports the
// count for the order it just made; everything else in the slice is a grant
// that had been waiting.
type Settlement struct {
	OrderID  string
	ItemID   int64
	Count    int32
	BagCount int32
	Granted  bool
}

// OrderID is the id the demo's "provider" assigns. A real provider assigns
// its own and the server never invents one; here the game plays the provider,
// so the id has to be derived from something the client cannot vary between
// retries of the same purchase — the session and the frame sequence, the same
// identity every other idempotent endpoint in this demo uses.
func OrderID(playerID int64, sessionID string, sequence uint32) string {
	return fmt.Sprintf("%s-%d-%s-%d", Channel, playerID, sessionID, sequence)
}
