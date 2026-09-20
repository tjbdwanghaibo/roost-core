package split

import (
	"github.com/tjbdwanghaibo/roost-core/app"
	kitredis "github.com/tjbdwanghaibo/roost-kit/redis"

	"github.com/tjbdwanghaibo/roost-kit/service/mail"
)

// MailProcess wires the process that OWNS mail.
//
// It registers two things that look similar and are not:
//
//	mail.NewMod(...)     an app.Mod — PUTS the mail capability into the registry
//	mail.NewServer()     an app.Service — TAKES it out and then blocks
//
// A process has many Mods and exactly one Service. The app runs every Mod's
// Init, Provide and Start, and only then the Service's Init and Serve — which
// is why the Server is where the RPC handlers get registered: that is the
// first moment every dependency is started and the process is about to serve.
// Registering them in the Mod would publish them while later Mods were still
// starting.
//
// mail.NewMod's arguments are the things configuration cannot supply. The
// broadcast deliverer is nil here, and that is safe rather than sloppy: Send
// refuses AudienceBroadcast outright when it is absent, so a deployment that
// only sends direct mail needs no fanout and one that forgot to wire it finds
// out on its first broadcast.
func MailProcess() *app.App {
	mailMod := mail.NewMod(
		nil, // broadcast Deliverer: absent, so broadcasts are refused rather than dropped
		nil, // metrics Reporter: nil means no reporting and never fails an operation
	)
	return app.New("roost-example", "0.0.1").
		RegisterServer(
			app.ServiceName(mail.ServiceType),
			mail.NewServer(),
			kitredis.NewRedisMod(),
			mailMod,
		)
}

// Configuration this process needs, beyond the framework's own:
//
//	mail:
//	  key_prefix: roost:mail   # required, no default — two deployments sharing
//	                           # one Redis would otherwise share state
//	  send_ttl: 24h            # required — must exceed the longest client retry
//	                           # horizon, or a retried send becomes a second mail
//	  claim_lease: 30s         # optional
//
// A key prefix has no default on purpose. A default is the same string in
// every deployment, so staging beside production, or two shards, or a replay
// harness, would quietly share mailboxes.
