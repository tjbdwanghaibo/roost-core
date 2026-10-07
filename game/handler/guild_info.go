package handler

import (
	guild "example.com/planet/game/entities/guild"
)

// handlerGuildInfo reads a guild.
//
// It is a read and it still takes the distributed lock, because a
// remote-managed entity has one owner at a time and reading it anywhere else
// would be reading a copy with no statement about how old it is. A game that
// wants cheap, bounded-stale reads of a shared object uses a mirror instead: a
// plain struct carrying the roost mirror marker (entityKind=guild.EntityKindGuild
// coll=guild) gets a generated New<DTO>Reader, read through
// kit/remoteentity.MirrorSource (RemoteMirrorMod in a read-only service). This
// handler answers a player who is about to act on the guild, so it pays for
// the lock; saying so is more useful than pretending the read is free.
//
//roost:nest rollback=undo durability=strict
func handlerGuildInfo(target guild.IRosterEntity) (guild.Info, error) {
	return target.RosterComp().Snapshot(), nil
}
