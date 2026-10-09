package account

import "github.com/tjbdwanghaibo/roost-core/framework/app"

// RegistryBound is implemented by a collaborator that needs a capability the
// account server's own Mods publish — a Redis or Mongo client for a durable id
// counter, an HTTP client the platform verifier shares with the process.
//
// Collaborators are constructed before the app exists (bootstrap builds them
// and passes them to NewMod), so a constructor cannot receive a registry; and
// PlayerIDAllocator demands a counter that is durable and shared while, until
// this hook, nothing handed a collaborator anything durable or shared. The Mod
// calls BindRegistry from Provide, after the server's other Mods have published
// their capabilities and before the service is built. An error stops the
// process from starting, which is the right outcome for "the counter I was
// promised is not here".
type RegistryBound interface {
	BindRegistry(r *app.Registry) error
}
