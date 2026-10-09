package platform

import "github.com/tjbdwanghaibo/roost-core/framework/app"

// RegistryBound is implemented by a collaborator that needs a capability the
// platform server's own Mods publish — the Redis client its pending-order
// index keeps its entries in, the platform service itself when the index has
// to read an order back before retiring it.
//
// Collaborators are constructed before the app exists (bootstrap builds them
// and passes them to NewMod / WithPendingOrders), so a constructor cannot
// receive a registry. The Mod calls BindRegistry from Provide, after the
// process's other Mods have published their capabilities and before the
// service is built. An error stops the process from starting, which is the
// right outcome for "the store I was promised is not here": the alternative is
// a platform process whose paid-but-undelivered orders are indexed nowhere.
//
// The service's own capability is published after Provide returns, so a
// collaborator that needs it keeps the registry and looks it up on first use
// rather than during BindRegistry.
type RegistryBound interface {
	BindRegistry(r *app.Registry) error
}
