package roost

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// demoAllianceService is the alliance service the demo adds beside the game
// (phases 3 and 4 of docs/maintenance/FEATURE-2026-11-04-alliance-service.md):
// an entity service that owns every Alliance and AllianceMember. The game's
// guild is this service: the game process has no Remote write runtime, calls
// the alliance RPC for every guild write (directly, or as the guild step of a
// saga) and reads guilds through Remote mirrors.
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
func demoAllianceSteps(gameService string) []demoScaffoldStep {
	return []demoScaffoldStep{
		{add: &AddOptions{Kind: "service", Name: demoAllianceService, Mods: []string{"dataengine", "etcd", "mongo", "nats", "nest", "redis", "remote_entity"}},
			why: "the alliance service: its own process with Nest, the Data Engine and the Remote runtime, owning every alliance (plan §2.1)"},
		{add: &AddOptions{Kind: "entity", Name: "Alliance", ID: 6}, why: "an alliance: remote=managed, written only by the instance that owns it, preloaded at start"},
		{add: &AddOptions{Kind: "entity", Name: "AllianceMember", ID: 7}, why: "a player's alliance guard: one player, one alliance across instances; a cache of the rosters, healed when found stale"},
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
		{write: "game/entities/alliance_member/entity.go", why: "the guard's kind: its id is the player's; snapshots=none, nothing reads its Remote snapshot"},
		{write: "game/entities/alliance_member/guard_component.go", why: "one player, one alliance: reserve, confirm or release by the alliance's answer, clear only the membership that ended"},
		{write: "game/entities/alliance_shard/entity.go", why: "one per instance, a plain local entity"},
		{write: "game/entities/alliance_shard/timer_handlers.go", why: "a reservation's deadline sends a best-effort effect; the consumer heals the guard from the alliance"},
		{add: &AddOptions{Kind: "lifecycle", Name: "Alliance", Service: demoAllianceService}, why: "a founded alliance is created in the store before Nest locks it"},
		{add: &AddOptions{Kind: "lifecycle", Name: "AllianceMember", Service: demoAllianceService}, why: "a player's guard is created on first use"},
		{add: &AddOptions{Kind: "lifecycle", Name: "AllianceShard", Service: demoAllianceService}, why: "the instance's shard is created before the first request"},
		{write: "internal/errors/alliance.go", why: "the alliance's business rejections, recorded in receipts"},
		{write: "game/alliancerule/alliancerule.go", why: "the receipt rule shared by both entities, and the shapes a write answers with"},
		{add: &AddOptions{Kind: "effect", Name: "alliance.member_changed", Consumer: demoAllianceService, Delivery: "best_effort"}, why: "every membership change, sent best effort after it is durable: the notice to the player's game server"},
		{add: &AddOptions{Kind: "effect", Name: "alliance.reservation_due", Consumer: demoAllianceService, Delivery: "best_effort"}, why: "a reservation's deadline, best effort: the guard is healed from the alliance outside the shard's lock (or by the next request that finds it)"},
		{add: &AddOptions{Kind: "effect", Name: "alliance.war_declared", Consumer: demoAllianceService}, why: "the enemy side of a cross-instance declaration"},
		{write: "game/effects/alliance_member_changed.go", why: "who, which alliance, which change, the alliance's version; best effort"},
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
		{write: "game/handler/alliance_admit_reserved.go", why: "the seat under the reservation's request id; the service confirms the guard (or the next request heals it)"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceRemove", Entity: "Alliance", Component: "Roster"}, why: "leave and kick"},
		{write: "game/handler/alliance_remove.go", why: "the roster first, then the guard: same transaction, or a direct clear the next request heals if it is lost"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceTransfer", Entity: "Alliance", Component: "Roster"}, why: "hand over the leadership"},
		{write: "game/handler/alliance_transfer.go", why: "leader only"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceDonate", Entity: "Alliance", Component: "Roster"}, why: "funds and contribution"},
		{write: "game/handler/alliance_donate.go", why: "the saga step after the game's debit (phase 4)"},
		{add: &AddOptions{Kind: "handler", Name: "AllianceSettle", Entity: "Alliance", Component: "Roster"}, why: "what became of a request, made final"},
		{write: "game/handler/alliance_settle.go", why: "what a request did and whether the player is seated; deciding, an undecided request is recorded as expired so a late delivery cannot seat the player"},
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
		{write: "internal/rpc/alliance/guard.go", why: "the guard calls between instances, the guard's healing from the roster, the notice consumers"},
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
		{write: "internal/service/alliance/alliance_member_changed_consumer.go", why: "notify the game server (best effort)"},
		{write: "internal/service/alliance/alliance_reservation_due_consumer.go", why: "heal the guard from the alliance at its deadline (best effort)"},
		{write: "internal/service/alliance/alliance_war_declared_consumer.go", why: "the enemy side of a cross-instance declaration"},
		{run: demoAllianceConfig, why: "the alliance.* section in the alliance service's configs, and a dev WAL directory of its own"},
		{run: enableDemoAllianceAdmin, why: "the alliance dev config enables the ops admin endpoint: alliance.transfer / info / guard / stop_founding on a developer machine"},
		{write: "internal/service/alliance/admin_test.go", why: "stop_founding on a real NATS bus: after it, founding requests land on the other instance and route RPCs still reach this one"},
		{write: "deploy/dev/second-alliance.sh", why: "a second alliance instance on this machine (its own sid, ops port, advertise address and WAL directory): founding spreads over both"},

		// The game side (phase 4): the guild is the alliance service's.
		{write: "game/entities/alliance/mirror.go", why: "the alliance mirror DTO a game server reads (//roost:mirror), in the shape a write answers with"},
		{write: "game/guild/guild.go", why: "the game's side: the price of a guild, the guild sagas' payload, the service's four answers as client codes"},
		{add: &AddOptions{Kind: "saga", Name: "AllianceFound", Service: gameService, Steps: []string{"debit", "found"}}, why: "found a guild: the fee from the Bag (native), then Found on the alliance service (Mongo step)"},
		{add: &AddOptions{Kind: "saga", Name: "AllianceDonate", Service: gameService, Steps: []string{"debit", "donate"}}, why: "donate: the coins from the Bag (native), then Donate (Mongo step)"},
		{add: &AddOptions{Kind: "saga", Name: "AllianceReward", Service: gameService, Steps: []string{"claim", "grant"}}, why: "the guild reward: ClaimReward on the guard (Mongo step), then the grant (native)"},
		{write: "saga/alliance_found/steps.go", why: "registration lives with the service (alliance_saga.go): bound-sid admission and the alliance RPC"},
		{write: "saga/alliance_donate/steps.go", why: "registration lives with the service (alliance_saga.go)"},
		{write: "saga/alliance_reward/steps.go", why: "registration lives with the service (alliance_saga.go)"},
		{add: &AddOptions{Kind: "handler", Name: "ShowGuild", Entity: "Player", Component: "Profile"}, why: "which guild the player is shown in: the player's own persisted, synced copy"},
		{write: "game/handler/show_guild.go", why: "set after the player's own join / leave / founding, on a notice, hidden at login; a leave notice hides only the guild it is about"},
		{add: &AddOptions{Kind: "handler", Name: "PlayerGuild", Entity: "Player", Component: "Profile"}, why: "read the player's copy under the Player's lock"},
		{write: "game/handler/player_guild.go", why: "the guild endpoints' advisory pre-checks and which guild to leave, donate to or claim from"},
		{add: &AddOptions{Kind: "handler", Name: "StartGuildOp", Entity: "Player", Component: "Bag"}, why: "a guild saga starts inside the player's transaction"},
		{write: "game/handler/start_guild_op.go", why: "check the coins under the lock, emit the saga start in the same WAL record"},
		{add: &AddOptions{Kind: "handler", Name: "GuildDebit", Entity: "Player", Component: "Bag"}, why: "the guild sagas' native Bag debit"},
		{write: "game/handler/guild_debit.go", why: "fee, donation, taking a late reward back: receipt and Bag in one WAL record"},
		{add: &AddOptions{Kind: "handler", Name: "GuildCredit", Entity: "Player", Component: "Bag"}, why: "the guild sagas' native Bag credit"},
		{write: "game/handler/guild_credit.go", why: "refunds and the reward grant: a full stack refuses, and the coordinator compensates"},
		{write: "game/handler/guild_steps_test.go", why: "the native steps on a real Nest: the debit takes once however often it is delivered, the refund puts it back, a full bag refuses the grant"},
		{write: "db/guild_display_test.go", why: "PlayerDao.GuildID is the player's persisted copy: the guards are not mirrored to game servers"},
		{run: demoGameUsesAlliance, why: "the game calls the alliance RPC (uses_rpcs: [alliance]) and prices a guild (guild.found_cost)"},
		{run: demoGameMirrorStaleness, why: "the game reads guilds through Remote mirrors: remote_entity.cached_max_staleness 5s bounds how stale a read can be when pushes stop"},
		{add: &AddOptions{Kind: "protocol", Name: "Guild", Group: "game", Handler: "player"}, why: "the guild endpoints: found / donate / claim (sagas), join / leave (RPC), info (mirror), op status"},
		{write: "protocol/def/guild.go", why: "one response shape for writes and reads, the version to read after, the saga key"},
		{add: &AddOptions{Kind: "protocol", Name: "GuildNotice", Group: "game", Handler: "player"}, why: "the server push for a membership notice"},
		{write: "protocol/def/guild_notice.go", why: "a notify: guild, change, version"},
		{write: "game/controllers/player/guild.go", why: "every write is the alliance RPC or a saga whose guild step is one; guild reads are the alliance mirror; which guild is the player's copy"},
		{write: "game/controllers/player/guild_test.go", why: "invalid names refused before anything moves, the four answers as codes, mirror reads and MinVersion, the login check of the copy"},
		{write: "internal/service/game/alliance_saga.go", why: "the guild sagas' twelve steps: Player steps native on the bound sid, guild steps through the RPC with the IdempotencyKey as request id"},
		{write: "internal/service/game/alliance_saga_test.go", why: "a refused founding refunds the fee; not executed and unknown redeliver; the reward claim and its release"},
		{write: "internal/service/game/alliance_notice.go", why: "membership notices update an online player's display and reach their client; offline players catch up at login"},
		{write: "internal/service/game/alliance_notice_test.go", why: "a notice reaches an online member and loads nobody offline; a late leave does not hide a newer guild"},
	}
}

// demoGameUsesAlliance makes the game service a caller of the alliance RPC
// (services.<game>.uses_rpcs: [alliance]: the generated ClientMod, whose route
// methods find an entity's owner through the remote_mirror Mod's lookup) and
// writes the guild.* section the game declares (game/settings Guild). It runs
// after roost add rpc, because a manifest naming an rpc nobody owns is refused.
func demoGameUsesAlliance(root, gameService string) error {
	manifestBefore, err := os.ReadFile(filepath.Join(root, ManifestName))
	if err != nil {
		return err
	}
	m, err := LoadManifest(root)
	if err != nil {
		return err
	}
	spec := m.Services[gameService]
	if !contains(spec.UsesRpcs, demoAllianceService) {
		spec.UsesRpcs = append(append([]string(nil), spec.UsesRpcs...), demoAllianceService)
		m.Services[gameService] = spec
		// Synced like roost add mod: the bootstrap assembles the ClientMod and
		// the shutdown budget counts it.
		if err := commitManifestSync(root, manifestBefore, m); err != nil {
			return err
		}
	}
	const block = "guild:\n" +
		"  # Gold coins (item 9001) the found_guild saga takes from the founder;\n" +
		"  # refunded when the alliance service refuses the founding.\n" +
		"  found_cost: 100\n"
	return appendDemoGameConfig(root, gameService, "guild", block, block)
}

// demoGameMirrorStaleness sets remote_entity.cached_max_staleness to 5s in
// the game service's three configs (dev, production example, k8s Secret
// example). The game reads every guild through Remote mirrors (plan §1.3):
// while pushes flow a read is fresh within tens of milliseconds; when they
// stop (an owner restarting, a reader past its push quota) a cached snapshot
// is re-confirmed once it is older than this. Measured in phase 5: the
// re-confirmations are the distinct keys being read divided by this bound —
// about 20,000 Redis HGET per second for 100,000 players online at 5s.
func demoGameMirrorStaleness(root, gameService string) error {
	const (
		before = "确认不了就不交出。不配置时等于 snapshot_cache_ttl（这里写成与它相同）；配置了必须为正。\n"
		after  = "确认不了就不交出。不配置时等于 snapshot_cache_ttl；配置了必须为正。game-demo：game 经 Mirror 读公会，\n"
		value  = "cached_max_staleness: 30s\n"
		next   = "推送中断时最长 5s 回源一次（回源量 = 在读的不同 key 数 / 5s，联盟服务方案 §1.3）。\n"
	)
	for _, rel := range []string{
		"configs/service/config." + gameService + ".yaml",
		"configs/service/config." + gameService + ".prod.example.yaml",
		"deploy/k8s/base/secret." + gameService + ".example.yaml",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		text := string(raw)
		at := strings.Index(text, before)
		if at < 0 || strings.Count(text, before) != 1 {
			return fmt.Errorf("%s: expected one remote_entity.cached_max_staleness comment", path)
		}
		lineStart := strings.LastIndex(text[:at], "\n") + 1
		indent := text[lineStart:at]
		indent = indent[:len(indent)-len(strings.TrimLeft(indent, " "))]
		old := before + indent + value
		if !strings.Contains(text, old) {
			return fmt.Errorf("%s: expected cached_max_staleness: 30s after its comment", path)
		}
		text = strings.Replace(text, old, after+indent+"# "+next+indent+"cached_max_staleness: 5s\n", 1)
		if err := writeAtomic(path, []byte(text), 0o644); err != nil {
			return err
		}
	}
	return nil
}

// enableDemoAllianceAdmin turns the ops admin endpoint on in the alliance
// service's dev config, as enableDemoAdmin does for the game: the alliance
// operator commands answer on its ops.addr with the dev token.
func enableDemoAllianceAdmin(root, _ string) error {
	return enableDemoAdmin(root, demoAllianceService)
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
		"  rejoin_cooldown: 0s\n" +
		"  # The alliance reward (prototype defaults): reward_count of reward_item_id\n" +
		"  # per member per reward_period; the game hands it over in its reward saga.\n" +
		"  reward_item_id: 1002\n" +
		"  reward_count: 5\n" +
		"  reward_period: 24h\n"
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
