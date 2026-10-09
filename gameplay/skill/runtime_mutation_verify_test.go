package skill

// Every incremental mutation commit performed by this package's tests is
// replayed against the reference full-snapshot diff and the checkpoint
// baseline invariant (see verifyIncrementalCommitLocked): the whole suite acts
// as the equivalence corpus for the write-point fast path. Benchmarks that
// measure the production path reset this flag locally.
func init() { stateMutationVerifyIncremental = true }
