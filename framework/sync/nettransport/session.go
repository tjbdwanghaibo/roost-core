package nettransport

// SessionID names one receiver on a transport. What it identifies — a
// connection, a player, a bot — is the caller's business; a transport only
// needs it to be non-zero, stable for the session's lifetime and unique among
// open sessions. entitysync and lockstep both address receivers with it.
type SessionID uint64

// SessionInfo is what a transport learns when a session is registered.
type SessionInfo struct {
	ID      SessionID
	OwnerID int64
	TeamID  int64
	Roles   uint64
}
