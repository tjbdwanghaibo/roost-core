package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// demoAllianceService is the alliance service the demo adds beside the game
// (phase 3 of docs/maintenance/FEATURE-2026-11-04-alliance-service.md): an
// entity service that owns every Alliance and AllianceMember. Nothing in the
// game calls it yet — the demo's Guild keeps working as before — and phase 4
// moves the guild onto it.
const demoAllianceService = "alliance"

// demoAllianceSteps builds the alliance service with the generator commands
// that exist today, then writes the demo's bodies over the scaffolds.
//
// The generator does the plumbing (phases 2 and 2R of the plan):
// service=alliance on the entity markers registers the kinds' handlers only
// in the alliance service, makes them foreign to every other service's Remote
// manager and registers their routes in Redis; preload=startup on Alliance
// loads an instance's alliances before it serves; //roost:rpc route generates
// the client that goes to an entity's owner and the handler's route check;
// roost add rpc -entity-service makes the owner Mod start after Nest and the
// preload. The request receipts, the rules, the founding and the two-step
// join are the demo's own code.
func demoAllianceSteps() []demoScaffoldStep {
	return []demoScaffoldStep{
		{add: &AddOptions{Kind: "service", Name: demoAllianceService, Mods: []string{"dataengine", "etcd", "mongo", "nats", "nest", "redis", "remote_entity"}},
			why: "the alliance service: its own process with Nest, the Data Engine and the Remote runtime, owning every alliance (plan §2.1)"},
		{add: &AddOptions{Kind: "entity", Name: "Alliance", ID: 6}, why: "an alliance: remote=managed, written only by the instance that owns it, preloaded at start"},
		{add: &AddOptions{Kind: "entity", Name: "AllianceMember", ID: 7}, why: "a player's alliance guard: one player, one alliance across instances"},
		{add: &AddOptions{Kind: "entity", Name: "AllianceShard", ID: 8}, why: "the instance's timer host: Remote entities carry no persistent timers"},
		{add: &AddOptions{Kind: "component", Name: "Roster", Entity: "Alliance", ID: 2008}, why: "the alliance's rules and receipts"},
		{add: &AddOptions{Kind: "component", Name: "Guard", Entity: "AllianceMember", ID: 2009}, why: "reserve, join, resolve, clear"},
		{add: &AddOptions{Kind: "dao", Name: "Alliance", Entity: "Alliance"}, why: "the roster, applications, wars and receipts in one document"},
		{add: &AddOptions{Kind: "dao", Name: "AllianceMember", Entity: "AllianceMember"}, why: "the guard: membership, the open reservation, receipts"},
		{add: &AddOptions{Kind: "dao", Name: "AllianceShard", Entity: "AllianceShard"}, why: "the persisted timer nodes"},
		{add: &AddOptions{Kind: "timer", Entity: "AllianceShard", Service: demoAllianceService}, why: "the shard's persistent timer component and tick handler"},
		{write: "db/def/alliance.go", why: "members, applications, wars and request receipts; the roster is the authority on membership"},
		{write: "db/def/alliance_member.go", why: "the guard and its reservation; not a second roster"},
		{write: "db/def/alliance_shard.go", why: "the timer nodes add timer declared, with what they are for"},
		{write: "game/entities/alliance/entity.go", why: "remote=managed, EntityCategoryRemote, owned by one service instance, preload=startup"},
		{write: "game/entities/alliance/roster_component.go", why: "check everything, then change: a rejection commits nothing but its receipt"},
		{write: "game/entities/alliance_member/entity.go", why: "the guard's kind: its id is the player's"},
		{write: "game/entities/alliance_member/guard_component.go", why: "one player, one alliance: reserve, confirm or release by the alliance's answer, clear only the membership that ended"},
		{write: "game/entities/alliance_shard/entity.go", why: "one per instance, a plain local entity"},
		{write: "game/entities/alliance_shard/timer_handlers.go", why: "a reservation's deadline records an effect; the consumer asks the alliance"},
		{add: &AddOptions{Kind: "lifecycle", Name: "Alliance", Service: demoAllianceService}, why: "a founded alliance is created in the store before Nest locks it"},
		{add: &AddOptions{Kind: "lifecycle", Name: "AllianceMember", Service: demoAllianceService}, why: "a player's guard is created on first use"},
		{add: &AddOptions{Kind: "lifecycle", Name: "AllianceShard", Service: demoAllianceService}, why: "the instance's shard is created before the first request"},
		{write: "internal/errors/alliance.go", why: "the alliance's business rejections, recorded in receipts"},
		{write: "game/alliancerule/alliancerule.go", why: "the receipt rule shared by both entities, and the shapes a write answers with"},
		{add: &AddOptions{Kind: "effect", Name: "alliance.member_changed", Consumer: demoAllianceService}, why: "the outbox of every membership change: finishes the guard and notifies the game"},
		{add: &AddOptions{Kind: "effect", Name: "alliance.reservation_due", Consumer: demoAllianceService}, why: "a reservation's deadline, settled with the alliance outside the shard's lock"},
		{add: &AddOptions{Kind: "effect", Name: "alliance.war_declared", Consumer: demoAllianceService}, why: "the enemy side of a cross-instance declaration"},
		{write: "game/effects/alliance_member_changed.go", why: "who, which alliance, which membership, and whether the guard is already settled"},
		{write: "game/effects/alliance_reservation_due.go", why: "player, alliance, request id"},
		{write: "game/effects/alliance_war_declared.go", why: "declarer, enemy, request id"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceFound", Entity: "Alliance", Component: "Roster"}, why: "the alliance and its founder's guard in one transaction"},
		{write: "game/handler/alliance_found.go", why: "the receipt on the guard: the alliance id did not exist before the request; Establish when the guard is elsewhere"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceApply", Entity: "Alliance", Component: "Roster"}, why: "an application"},
		{write: "game/handler/alliance_apply.go", why: "every write: receipt first, rules, then the change"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceReject", Entity: "Alliance", Component: "Roster"}, why: "the leader drops an application"},
		{write: "game/handler/alliance_reject.go", why: "leader only"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceAdmit", Entity: "Alliance", Component: "Roster"}, why: "alliance and guard on one instance: one transaction"},
		{write: "game/handler/alliance_admit.go", why: "the guard check and the seat serialised on the guard's lock"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceAdmitReserved", Entity: "Alliance", Component: "Roster"}, why: "step 2 of a cross-instance join"},
		{write: "game/handler/alliance_admit_reserved.go", why: "the seat under the reservation's request id; the effect confirms the guard"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceRemove", Entity: "Alliance", Component: "Roster"}, why: "leave and kick"},
		{write: "game/handler/alliance_remove.go", why: "the roster first, then the guard: same transaction or from the effect"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceTransfer", Entity: "Alliance", Component: "Roster"}, why: "hand over the leadership"},
		{write: "game/handler/alliance_transfer.go", why: "leader only"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceDonate", Entity: "Alliance", Component: "Roster"}, why: "funds and contribution"},
		{write: "game/handler/alliance_donate.go", why: "the saga step after the game's debit (phase 4)"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceSettle", Entity: "Alliance", Component: "Roster"}, why: "what became of a request, made final"},
		{write: "game/handler/alliance_settle.go", why: "an undecided request is recorded as expired, so a late delivery cannot seat the player"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceInfo", Entity: "Alliance", Component: "Roster"}, why: "the authoritative read and the receipt lookup"},
		{write: "game/handler/alliance_info.go", why: "Info and Result"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceWar", Entity: "Alliance", Component: "Roster"}, why: "two alliances in one transaction"},
		{write: "game/handler/alliance_war.go", why: "same instance: both sides or neither; across instances: two steps"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceGuard", Entity: "AllianceMember", Component: "Guard"}, why: "the guard's writes and reads"},
		{write: "game/handler/alliance_guard.go", why: "reserve with its deadline, resolve, clear, read"},
		{add: &AddOptions{Kind: "rpc", Name: "Alliance", Service: demoAllianceService, EntityService: true}, why: "the alliance RPC as an entity service: request ids, Result, the owner Mod after Nest"},
		{write: "internal/rpc/alliance/alliance.go", why: "the write contract: request ids, caller identity, the four answers, route methods and the create method"},
		{write: "internal/rpc/alliance/service.go", why: "the route check, one Nest call per write, framework errors mapped to answers"},
		{write: "internal/rpc/alliance/membership.go", why: "found (anchored on the founder's guard), approve / join (one transaction or two steps), leave / kick"},
		{write: "internal/rpc/alliance/guard.go", why: "the guard calls between instances and the effect consumers' logic"},
		{write: "internal/rpc/alliance/war.go", why: "one transaction on one instance, two steps across"},
		{write: "internal/rpc/alliance/settings.go", why: "alliance.* declared once: the roster limit (default 100, hard 500), reservation ttl, receipt retention"},
		{write: "internal/rpc/alliance/wiring.go", why: "the service bound to the process: lifecycles, the id counter, the routed client, notices"},
		{write: "internal/rpc/alliance/transfer.go", why: "the operator's transfer through TransferRemoteOwnership"},
		{write: "internal/rpc/alliance/mod.go", why: "the generated owner Mod (after Nest and the preload), binding the service to the process in Start"},
		{write: "internal/rpc/alliance/harness_test.go", why: "instances in one process: real Nest, handlers, rules, generated client and handlers over a stand-in bus and Remote runtime"},
		{write: "internal/rpc/alliance/service_test.go", why: "receipts replay, permissions, the roster limit"},
		{write: "internal/rpc/alliance/membership_test.go", why: "one player one alliance on one and two instances, reservation deadlines, one-transaction wars, the route check, the four answers, transfer, founding retried elsewhere, concurrent guard claims"},
		{write: "internal/service/alliance/service.go", why: "the alliance process: config declaration, effect consumers, operator commands"},
		{write: "internal/service/alliance/admin.go", why: "transfer / info / guard on the ops admin endpoint"},
		{write: "internal/service/alliance/alliance_member_changed_consumer.go", why: "finish the guard, notify the game server"},
		{write: "internal/service/alliance/alliance_reservation_due_consumer.go", why: "settle with the alliance, then resolve the guard"},
		{write: "internal/service/alliance/alliance_war_declared_consumer.go", why: "the enemy side of a cross-instance declaration"},
		{run: demoAllianceConfig, why: "the alliance.* section in the alliance service's configs, and a dev WAL directory of its own"},
	}
}

// demoAllianceConfig writes the alliance.* section into the alliance
// service's three configs (dev, production example, k8s Secret example),
// gives the dev config its own WAL directory, and lists the game's sids in the
// activity groups file again.
//
// The groups file: the production compose numbers services 1000 + their
// index in name order, so a service named before the game moves the game's
// compose sid (1003 → 1004 here). The file was written when the project was
// created and is the project's from then on; it is written once more here so
// it lists the sids the generated deployments now start the game with.
//
// The WAL directory: a dev config leaves dataengine.wal.dir unset, which
// means data/wal/dataengine/<sid> under the working directory. deploy/dev/run.sh
// starts every service with the same sid from the same directory, and the game
// service already holds data/wal/dataengine/<sid> — the second process to lock
// it would exit. So the alliance's dev config names a directory of its own;
// one alliance process per working directory on a developer machine.
// Production is unaffected: each instance has its own volume.
func demoAllianceConfig(root, _ string) error {
	const block = "alliance:\n" +
		"  # No routing settings: an alliance is served by the instance its Remote\n" +
		"  # ownership record names, found through the route registration.\n" +
		"  max_members: 100\n" +
		"  reservation_ttl: 30s\n" +
		"  receipt_retention: 168h\n" +
		"  rejoin_cooldown: 0s\n"
	if err := appendDemoGameConfig(root, demoAllianceService, "alliance", block, block); err != nil {
		return err
	}
	path := filepath.Join(root, "configs", "service", "config."+demoAllianceService+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	const marker = "\n  wal:\n"
	if strings.Count(string(raw), marker) != 1 {
		return fmt.Errorf("%s: expected one dataengine.wal block", path)
	}
	text := strings.Replace(string(raw), marker, marker+
		"    # Its own directory: the game service holds data/wal/dataengine/<sid> under the\n"+
		"    # same working directory and sid in deploy/dev/run.sh. One alliance process per\n"+
		"    # working directory; production gives each instance its own volume.\n"+
		"    dir: data/wal/alliance\n", 1)
	if err := writeAtomic(path, []byte(text), 0o644); err != nil {
		return err
	}
	m, err := LoadManifest(root)
	if err != nil {
		return err
	}
	if !hostsActivityCoordinator(m) {
		return nil
	}
	return writeAtomic(filepath.Join(root, filepath.FromSlash(activityGroupsFile)), []byte(renderActivityGroups(m)), 0o644)
}
