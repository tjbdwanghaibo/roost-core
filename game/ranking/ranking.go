package ranking

import (
	svcrank "github.com/tjbdwanghaibo/roost-core/service/rank"
)

// Package ranking is the game's side of the rank service: which boards exist,
// what a point means, and what makes a submit safe to retry.
//
// The rank service ranks numbers; it has no opinion about what they count.
// Deciding that is this file's whole job, and the decision worth reading is
// ClearsScore.

// ClearsBoardID counts cleared dungeon runs. Global scope, one season for
// now: a season boundary is an operator action (the service deliberately
// keeps Reset off the bus) and the demo has no calendar. The count behind the
// board is kept per season on the Player (PlayerDao.DungeonClears), so moving
// CurrentSeason starts every player at zero on the new board instead of
// carrying the old season's count over.
const (
	ClearsBoardID = "clears"
	CurrentSeason = int64(1)
)

func ClearsBoard() svcrank.Board {
	return svcrank.Board{ID: ClearsBoardID, Scope: svcrank.ScopeGlobal, Season: CurrentSeason}
}

// ClearsScore is a player's count of paid clears this season, as the claim
// transaction reported it (dungeon.ClaimResult.Clears). It is submitted with
// UpdateMax and no request id: a SNAPSHOT of a count the Player owns, not a
// "+1". Tie is left zero on purpose: the store fills it with the submit time,
// and UpdateMax keeps the earlier tie for an equal value, so players on the
// same number of clears are ordered by who got there first.
//
// Why not "+1 deduplicated on the run id", which this board used first: the
// rank service remembers only the last few request ids per owner
// (service/rank maxAppliedRequests = 8). That is a redelivery guard, not a
// ledger, and the FinishDungeon endpoint has to submit on every replay so a
// point lost to a crash can be filled. A run replayed inside its claim window
// after eight later clears had fallen out of the ring and counted again (nine
// clears scored ten). Making the ring longer only moves the cliff; tying it to
// the claim window would be a third bounded ledger whose forgetting has to be
// argued separately from the reward's. A count that moves inside the reward
// transaction needs neither: the reward ledger and its window are the only
// admission, the count is never pruned, and UpdateMax makes duplicates and
// out-of-order snapshots harmless.
func ClearsScore(playerID, clears int64) svcrank.Score {
	return svcrank.Score{OwnerID: playerID, Value: clears}
}

// PageSize is what the demo's endpoint reads. The service caps a page;
// "the whole board" is not an option the API offers, which is the right
// default for something that grows with the player base.
const PageSize = 10
