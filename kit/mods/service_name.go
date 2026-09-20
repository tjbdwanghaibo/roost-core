// Package servicemods holds the app.ModName constants for every service in
// this repository, and the small helpers their Mods share.
//
// The names live in one table rather than one per package for the same reason
// roost-kit keeps its own: a capability name is a global key in the registry,
// so a collision is a runtime failure that two separately-correct packages can
// cause together. One table is where a collision is visible by reading.
//
// The names are prefixed "service." so they cannot collide with roost-kit's
// infrastructure capabilities — a service named "chat" and a hypothetical
// kit transport named "chat" would otherwise be the same key.
package mods

import "github.com/tjbdwanghaibo/roost-core/app"

// Capability names for the services in this repository.
const (
	ModAccount   app.ModName = "service.account"
	ModChat      app.ModName = "service.chat"
	ModDirectory app.ModName = "service.directory"
	ModGlobal    app.ModName = "service.global"
	// ModGlobalActivity is the cross-server activity coordination service. It
	// is a separate capability from ModGlobal because the two share no state
	// — routing and leases answer "where does this game belong and is it
	// alive", activity coordination answers "has every game reached the phase
	// yet" — and joining them would give the activity side a reason to reach
	// into the lease store.
	//
	// The name keeps "global" because that is what the service coordinates
	// across, and because it was published under this name. The CODE no
	// longer lives in package global: two capabilities in one package meant
	// two things that deploy separately sharing one Go package, since
	// app.Service is one per process. It is package global/activity, and
	// activity.CapabilityName is checked against this constant in the
	// integration tests.
	ModGlobalActivity app.ModName = "service.global.activity"
	ModMail           app.ModName = "service.mail"
	ModMatch          app.ModName = "service.match"
	ModPlatform       app.ModName = "service.platform"
	ModRank           app.ModName = "service.rank"
	ModSession        app.ModName = "service.session"
)

// All is every consumer-facing capability name this repository can register.
//
// It exists so a test can assert that the table has no duplicates — the one
// failure a name table is supposed to prevent, and the one that a table
// nobody checks does not prevent at all.
//
// It lists the CONSUMER-facing names only. A service whose transport is
// generated also publishes an owner-only name derived from this one
// (<name>.local), which is how a process knows it holds the implementation
// rather than a client to it; that name is not a lookup any consumer writes,
// so listing it here would invite one.
//
// Each generated service also declares its own CapabilityName constant, so its
// transport does not depend on this file. That is two literals of the same
// name, so integration/ asserts they are equal rather than trusting it.
var All = []app.ModName{
	ModAccount, ModChat, ModDirectory, ModGlobal, ModGlobalActivity,
	ModMail, ModMatch, ModPlatform, ModRank, ModSession,
}
