//go:build protocoldef

package protocoldef

// EntitySyncPush carries one server-authoritative state frame for this
// session: the subjects this client is subscribed to, as encoded by
// roost-core's entitysync manager. One push is one complete frame — a
// statesync frame whose Epoch/Tick are this session's own clock — and a
// client decodes it with entitysync.DecodeFrame, then
// entitysync.DecodeSubjectUpdate per object: the subject id says whose state
// it is, and the payload is what that entity's packer produced. The demo
// pushes it over the player's TCP connection; a QUIC or KCP deployment sends
// the same bytes on its reliable lane.
type EntitySyncPush struct {
	Payload []byte `pb:"1"`
}

//roost:protocol group=game handler=player
type EntitySyncProtocol interface {
	//roost:msg id=10103
	EntitySync() EntitySyncPush
}
