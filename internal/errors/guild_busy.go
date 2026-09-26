package errors

import "github.com/tjbdwanghaibo/roost-core/errcode"

// ErrGuildBusy: the guild's distributed lock could not be taken in time.
//
// It is a RETRYABLE refusal, and telling it apart from a real failure matters:
// the commonest cause is a process that just restarted, whose predecessor's
// lock lease has not expired yet (up to remote_entity.lock_ttl). Reporting
// that as an internal error tells the client "something is broken" about a
// situation that fixes itself in seconds.
var ErrGuildBusy = errcode.Define(100020, "guild_busy", "guild is busy, try again")
