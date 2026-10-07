package dungeon

// Package dungeon is the game's side of the session service's bounded runs:
// what a clear is worth, and how the game makes sure it pays for one exactly
// once.
//
// The exactly-once part is the interesting one. `session.Finish` is
// idempotent by design: finishing an already-finished run returns that run
// with no error (it releases what the run held and leaves the finish time
// alone), and a run whose deadline passed comes back as expired whatever the
// caller asked for. So "the Finish call succeeded" does NOT mean "this call
// is the one that resolved the run", and the request's own Success flag
// means nothing at all — only the returned run.State is authoritative
// (RR-20260917-08).
//
// That settles which runs deserve a reward. It does not settle how many
// times one run is paid: two concurrent replays can both see a succeeded run.
// The claim ledger does that: the reward transaction records the run id on
// the Player and pays only when it is the transaction that recorded it.

// ClearExp is what a cleared dungeon is worth. It is enough to level the
// demo's Player up, so a clear also exercises the level-up → reward mail
// chain.
const ClearExp int64 = 100

// ClaimWindowSeconds is how long after a run resolves its clear reward may
// still be claimed — and, by the same number, how long the claim ledger
// remembers that it paid. One constant for both is not tidiness; it is the
// argument.
//
// The first version of this bounded the ledger by "a run cannot be replayed
// forever anyway": `session.run_ttl` is 30m, so a claim held 4h outlived
// every replay. That is wrong, and review caught it (RR-20260918-04). Runs
// and claims have **no storage TTL** in the session service — run_ttl is how
// long a run may stay *open*. A run that succeeded stays succeeded, and
// `session.Finish` keeps answering with that terminal state years later. So
// forgetting a claim never became safe on its own: the next claim's pruning
// dropped the old run's record, a replay of that old run found the ledger
// empty, and it was paid a second time.
//
// The fix is to stop reasoning about what the session service forgets and
// give the reward its own deadline, computed from a time the game did not
// invent: the run's own resolution time, minted and stored by the session
// service. A claim is refused once the run is older than this window, and
// the ledger stores that same resolution time, so:
//
//	a record is pruned  ⟺  now - resolvedAt > ClaimWindowSeconds
//	                    ⟺  a claim for that run is refused
//
// Pruned therefore implies refused, whatever order the calls arrive in, with
// no assumption about any other service's storage. Raising or lowering this
// value keeps the property; it only moves how long a client (or an operator
// replaying by hand) has to collect.
//
// It must comfortably exceed how long a legitimate claim can be delayed: a
// run resolved at its own deadline, plus reconnects and retries. 4h against
// a 30m run_ttl leaves that margin.
const ClaimWindowSeconds int64 = 4 * 60 * 60

// ClaimWindowClosed reports whether a run resolved at resolvedAtUnix is past
// the point where its clear reward may still be paid at nowUnix. It is the
// admission check on the reward path and the pruning predicate for the
// ledger — deliberately the same function, because two predicates that drift
// apart is exactly how RR-20260918-04 happened.
//
// A non-positive timestamp is a run whose resolution time cannot be proven
// (a hand-edited document, a record written by an older build). It is
// refused rather than paid: the ledger cannot promise exactly-once for a run
// it cannot place in time, and refusing is the direction an operator can
// correct.
func ClaimWindowClosed(resolvedAtUnix, nowUnix int64) bool {
	if resolvedAtUnix <= 0 {
		return true
	}
	return nowUnix-resolvedAtUnix > ClaimWindowSeconds
}

// ClaimResult is what the reward transaction reports back. Claimed is false
// for a replay: the run was already paid, and this call changed nothing. A
// run past its claim window is not reported here at all — it comes back as a
// coded error, so "you were paid already" and "this is too old to pay" never
// look the same to the caller.
type ClaimResult struct {
	Claimed      bool
	ExpGranted   int64
	LevelsGained int32
}
