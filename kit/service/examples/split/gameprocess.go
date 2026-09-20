package split

import (
	"context"

	"github.com/tjbdwanghaibo/roost-core/app"

	"github.com/tjbdwanghaibo/roost-kit/service/mail"
)

// GameProcess wires a process that CALLS mail without owning it.
//
// The only difference from MailProcess is which mail Mod it registers:
//
//	owner:  mail.NewMod(...)        + mail.NewServer() as the app.Service
//	caller: mail.NewClientMod(...)  + its own app.Service
//
// Both Mods publish the same capability name and the same interface type, so
// consumer.go is byte-identical in the two processes. That is the whole
// deliverable of this design: which services share a process is a deployment
// decision, and it is expressed here and nowhere else.
//
// Registering BOTH in one process fails at startup — they publish the same
// name and the registry refuses the second. A process cannot end up holding a
// local service and a client to itself with the winner decided by
// registration order.
func GameProcess() *app.App {
	return app.New("roost-example", "0.0.1").
		RegisterServer(
			"game",
			&GameService{},
			// No Redis Mod: this process does not own mail's store, so it has
			// no key prefix to get wrong. The client reads no store settings
			// at all.
			//
			// A bus Mod belongs here in a real wiring; it is omitted because
			// this example is about the mail side.
			mail.NewClientMod(),
		)
}

// GameService is this process's app.Service — the game's own, not mail's.
//
// Its Init is where the business logic takes the mail capability out of the
// registry, by interface. It does not know whether the capability is a local
// service or a client, and that is the point.
type GameService struct {
	rewards *RewardFlow
}

// Name implements app.Service.
func (g *GameService) Name() app.ServiceName { return "game" }

// Init implements app.Service.
func (g *GameService) Init(r *app.Registry) error {
	rewards, err := NewRewardFlow(r)
	if err != nil {
		return err
	}
	g.rewards = rewards
	return nil
}

// Serve implements app.Service.
func (g *GameService) Serve(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// Shutdown implements app.Service.
func (g *GameService) Shutdown(context.Context) error { return nil }

// Configuration this process needs for mail — three lines, and none of them
// is a store setting:
//
//	mail:
//	  service_type: mail   # optional, defaults to mail.ServiceType
//	  call_timeout: 3s     # optional; a call with no timeout is a goroutine
//	                       # waiting on a process that may never answer
//
// It has no key_prefix and no send_ttl, because a client has no store. A
// caller therefore cannot be misconfigured with a prefix that disagrees with
// the owner's: there is no prefix here to disagree.

var _ app.Service = (*GameService)(nil)
