package roost

import (
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-codegen/demo"
)

// demoModulePlaceholder is replaced with the project's module path.
const demoModulePlaceholder = "{{MODULE}}"

// demoGameServicePlaceholder is replaced with the game service's name as it
// appears in paths and app.ServiceName; demoGameServicePackagePlaceholder
// with its Go package identifier. The demo's files under
// internal/service/game/ are written to internal/service/<name>/ so a
// project created with a different first service still gets them.
const (
	demoGameServicePlaceholder        = "{{GAME_SERVICE}}"
	demoGameServicePackagePlaceholder = "{{GAME_SERVICE_PKG}}"
	demoProjectPlaceholder            = "{{PROJECT}}"
	demoGameServiceDir                = "internal/service/game/"
)

// demoVars is what a template may refer to besides its own text.
type demoVars struct {
	module      string
	gameService string
	project     string
}

func (vars demoVars) apply(body string) string {
	body = strings.ReplaceAll(body, demoModulePlaceholder, vars.module)
	body = strings.ReplaceAll(body, demoProjectPlaceholder, vars.project)
	body = strings.ReplaceAll(body, demoGameServicePackagePlaceholder, safeIdent(vars.gameService))
	return strings.ReplaceAll(body, demoGameServicePlaceholder, vars.gameService)
}

// demoDestination maps a shipped template path to where it lands in the
// project: the same path, except under the game service's own directory.
func demoDestination(rel, gameService string) string {
	if rest, ok := strings.CutPrefix(rel, demoGameServiceDir); ok {
		return "internal/service/" + gameService + "/" + rest
	}
	return rel
}

// demoTemplateName is the opt-in value of `roost project new … -template`.
const demoTemplateName = "game-demo"

// writeDemoFile renders one embedded template into the project. The result is
// application-owned: it carries no generated header and nothing rewrites it
// afterwards, so `project sync` and `project upgrade` leave the developer's
// edits alone. Several of these deliberately overwrite a scaffold that `Add`
// has just produced — the scaffold supplies the wiring (the Entity field, the
// DAO binding, the manifest entry), and the demo supplies the body.
func writeDemoFile(root string, vars demoVars, rel string) (string, error) {
	raw, err := demo.Files.ReadFile(rel + ".tmpl")
	if err != nil {
		return "", fmt.Errorf("template %s: read demo source: %w", demoTemplateName, err)
	}
	body := vars.apply(string(raw))
	destination := demoDestination(rel, vars.gameService)
	if strings.HasSuffix(rel, ".go") {
		formatted, formatErr := format.Source([]byte(body))
		if formatErr != nil {
			return "", fmt.Errorf("template %s: format %s: %w", demoTemplateName, rel, formatErr)
		}
		body = string(formatted)
	}
	if err := writeAtomic(filepath.Join(root, filepath.FromSlash(destination)), []byte(body), 0o644); err != nil {
		return "", err
	}
	return destination, nil
}

// demoSourceFiles lists every template this package ships, so a test can hold
// the embedded set and the scaffold steps to the same list.
func demoSourceFiles() ([]string, error) {
	var out []string
	err := fs.WalkDir(demo.Files, ".", func(p string, entry fs.DirEntry, err error) error {
		if err != nil || entry.IsDir() {
			return err
		}
		out = append(out, strings.TrimSuffix(p, ".tmpl"))
		return nil
	})
	return out, err
}

// applyDemoTemplate is the game template plus what the demo's own code needs
// from the manifest: protocol (its request message), dao (its persistent
// Player state), config (the item table) and errcode (its coded failures).
// All are on by default; a caller who narrowed -features would otherwise get
// a project that cannot build.
func applyDemoTemplate(m *Manifest, gameService string) error {
	if err := applyGameTemplate(m, gameService); err != nil {
		return err
	}
	// The game process reads the platform service's grant records out of the
	// same Redis that service writes them to (game/purchase, and the drain in
	// internal/service/<game>/purchase_drain.go). It is the only cross-process
	// handover in the demo that is not a bus call, and it is one because the
	// delivering side has to make it durable before it may answer "delivered".
	game := m.Services[gameService]
	if resolved, err := resolveMods(game.Mods); err == nil && !contains(resolved, "redis") {
		game.Mods = append(game.Mods, "redis")
		m.Services[gameService] = game
	}
	// The guild is a remote-managed entity: it belongs to whichever process
	// holds its distributed lock, not to the one that created it. That is the
	// only way a roster shared by players on different processes can be
	// written correctly, and it needs the remote-entity runtime (which brings
	// room with it).
	game = m.Services[gameService]
	if resolved, err := resolveMods(game.Mods); err == nil && !contains(resolved, "remote_entity") {
		game.Mods = append(game.Mods, "remote_entity")
		m.Services[gameService] = game
	}
	for _, feature := range []string{"protocol", "entity", "nest", "dao", "config", "errcode", "attribute"} {
		if !contains(m.Features, feature) {
			m.Features = append(m.Features, feature)
		}
	}
	sort.Strings(m.Features)
	return nil
}

// demoScaffoldStep is one step of the demo build-out: either a generator
// command or a template write. They are ordered, and the order carries
// meaning — `add endpoint` parses the handler's parameters and the protocol
// message's fields and refuses to wire a pair that does not match, so both
// bodies must be on disk before it runs.
type demoScaffoldStep struct {
	add   *AddOptions
	write string
	run   func(root, gameService string) error
	why   string
}

// enableDemoMatchSweep lists the demo's duel queue under match.sweep_queues in
// the match service's config, the way a deployment does. The match process
// then resolves expired duel tickets in the background; without it a lapsed
// ticket is only resolved when touched, and the process says so at start.
func enableDemoMatchSweep(root, _ string) error {
	path := filepath.Join(root, "configs", "service", "config.match.yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	const before, after = "  sweep_queues: []\n", "  sweep_queues:\n    - duel:2:default\n    - ranked:2:default\n"
	if !strings.Contains(string(raw), before) {
		return fmt.Errorf("%s: expected %q to replace", path, strings.TrimSpace(before))
	}
	return writeAtomic(path, []byte(strings.Replace(string(raw), before, after, 1)), 0o644)
}

// enableDemoAdmin turns the ops admin endpoint on in the game service's DEV
// config with a dev token: GET /admin/commands and POST /admin/execute answer
// on ops.addr with X-Admin-Token: dev-gm-token. The production example config
// is untouched — it keeps admin off, and config check --production refuses a
// dev- token anyway, so a real deployment mints its own.
func enableDemoAdmin(root, gameService string) error {
	path := filepath.Join(root, "configs", "service", "config."+gameService+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	const before = "  admin_enabled: false\n  admin_token: \"\"\n  allow_dev_token: false\n"
	const after = "  admin_enabled: true\n  admin_token: dev-gm-token\n  allow_dev_token: true\n"
	if !strings.Contains(string(raw), before) {
		return fmt.Errorf("%s: expected the ops admin block to replace", path)
	}
	return writeAtomic(path, []byte(strings.Replace(string(raw), before, after, 1)), 0o644)
}

// enableDemoPlayerTCP flips player_access.tcp.enabled in the game service's
// config, the same edit `roost config enable player-tcp` makes. A demo whose
// listener is off cannot be connected to, and `roost project doctor
// -workflow player-tcp` would fail on the project it just generated.
func enableDemoPlayerTCP(root, gameService string) error {
	_, err := ensurePlayerTCPConfig(root, gameService, "", true)
	return err
}

// demoPaymentSecrets gives the platform service its two secrets in the DEV
// configs and tells the game process the same payment secret.
//
// The game process holding a payment secret is the demo playing the payment
// provider (game/controllers/player/purchase.go says so at length): a real
// store signs callbacks with a key no game process has. What is not a demo
// shortcut is where the keys live — the platform Mod refuses an empty
// session_secret or payment_secret at Init, so a starter config that omits
// them is a process that cannot start. The production example keeps CHANGE_ME.
func demoPaymentSecrets(root, gameService string) error {
	platformPath := filepath.Join(root, "configs", "service", "config.platform.yaml")
	raw, err := os.ReadFile(platformPath)
	if err != nil {
		return err
	}
	const before = "  session_secret: CHANGE_ME\n  payment_secret: CHANGE_ME\n"
	const after = "  session_secret: dev-platform-session-secret\n  payment_secret: dev-platform-payment-secret\n"
	if !strings.Contains(string(raw), before) {
		return fmt.Errorf("%s: expected the platform secrets to replace", platformPath)
	}
	if err := writeAtomic(platformPath, []byte(strings.Replace(string(raw), before, after, 1)), 0o644); err != nil {
		return err
	}
	// The game side: the key prefix it reads grants under, and the secret it
	// signs its simulated callbacks with. Appended rather than templated
	// because the game process does not host the platform service, so nothing
	// generates a platform block for it.
	gamePath := filepath.Join(root, "configs", "service", "config."+gameService+".yaml")
	gameRaw, err := os.ReadFile(gamePath)
	if err != nil {
		return err
	}
	if strings.Contains(string(gameRaw), "\nplatform:\n") {
		return nil
	}
	block := "platform:\n  key_prefix: " + blockKeyPrefix(string(raw), "platform") + "\n  payment_secret: dev-platform-payment-secret\n"
	return writeAtomic(gamePath, append(append([]byte(nil), gameRaw...), []byte(block)...), 0o644)
}

// gameRoutePrefix is the game's own Redis namespace, derived from a prefix the
// project already has so a deployment does not have to keep two in step: the
// activity service's `roost:<project>:activity` becomes `roost:<project>:route`.
func gameRoutePrefix(serviceKeyPrefix string) string {
	if cut := strings.LastIndex(serviceKeyPrefix, ":"); cut > 0 {
		return serviceKeyPrefix[:cut] + ":route"
	}
	return "roost:route"
}

// blockKeyPrefix reads a service's key prefix out of that service's own
// config, so two processes cannot be given different ones by an edit to one
// file.
func blockKeyPrefix(serviceConfig, service string) string {
	for _, line := range strings.Split(serviceConfig, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), "key_prefix:"); ok {
			return strings.TrimSpace(rest)
		}
	}
	return "roost:" + service
}

// demoActivityKeys tells the game process where the activity coordinator's
// keyspace is and which game servers this deployment may run.
//
// The prefix is read from the coordinator's own config rather than written
// twice: the game keeps its contributor board BESIDE the coordinator's keys,
// and two files that can disagree about where that is means a settlement
// reading an empty board. The sid list is what the candidate set for
// LiveGames is drawn from — a deployment with three game processes lists all
// three here, and the ones that are actually up are the ones an activity
// waits for.
func demoActivityKeys(root, gameService string) error {
	activityPath := filepath.Join(root, "configs", "service", "config.activity.yaml")
	activityRaw, err := os.ReadFile(activityPath)
	if err != nil {
		return err
	}
	gamePath := filepath.Join(root, "configs", "service", "config."+gameService+".yaml")
	gameRaw, err := os.ReadFile(gamePath)
	if err != nil {
		return err
	}
	if strings.Contains(string(gameRaw), "\nactivity:\n") {
		return nil
	}
	// The game's own keyspace, for facts that are the GAME's rather than a
	// framework service's: which process owns which player (game/playerroute).
	// It is per deployment, like every other prefix, so two deployments on one
	// Redis do not decide each other's ownership.
	routeBlock := "game_route:\n  key_prefix: " + gameRoutePrefix(blockKeyPrefix(string(activityRaw), "activity")) + "\n"
	gameRaw = append(append([]byte(nil), gameRaw...), []byte(routeBlock)...)
	block := "activity:\n  key_prefix: " + blockKeyPrefix(string(activityRaw), "activity") +
		"\n  # Every game server this deployment may run. LiveGames narrows it to\n" +
		"  # the ones holding a lease, and those are what an activity waits for.\n" +
		"  game_sids:\n    - 1000\n"
	return writeAtomic(gamePath, append(append([]byte(nil), gameRaw...), []byte(block)...), 0o644)
}

func demoScaffoldSteps(gameService string) []demoScaffoldStep {
	return []demoScaffoldStep{
		{add: &AddOptions{Kind: "component", Name: "Profile", Entity: "Player"}, why: "Player's identity state"},
		{add: &AddOptions{Kind: "component", Name: "Bag", Entity: "Player"}, why: "Player's item state"},
		{add: &AddOptions{Kind: "dao", Name: "Player", Entity: "Player"}, why: "persistence for both components"},
		{write: "db/def/player.go", why: "the persistent fields the generated mutators are built from"},
		{write: "game/entities/player/profile_component.go", why: "rename and level-up through generated mutators; level-up emits an effect"},
		{write: "game/entities/player/bag_component.go", why: "add-item: table lookup, coded errors, generated map mutators"},
		{write: "game/entities/player/entity.go", why: "EntityCategoryPlayer: the kind's lock rank, decided before the first document is persisted"},
		{write: "game/effects/level_up.go", why: "the level-up effect: topic, payload, and the Emit onto the current transaction"},
		{add: &AddOptions{Kind: "component", Name: "Stats", Entity: "World"}, why: "World's one job: server-wide counters"},
		{add: &AddOptions{Kind: "dao", Name: "World", Entity: "World"}, why: "persistence for the counters"},
		{write: "db/def/world.go", why: "PlayersEntered and MatchesFormed"},
		{write: "game/entities/world/stats_component.go", why: "count logins and matches through generated mutators; snapshot for reads"},
		{write: "game/entities/world/entity.go", why: "EntityCategoryWorld: locked before Player in the two-entity AddExp"},
		{write: "game/matchmaking/queue.go", why: "the duel and ranked queues, their grouping policies, and how a player is named in them"},
		{add: &AddOptions{Kind: "table", Name: "Item"}, why: "the item config table"},
		{add: &AddOptions{Kind: "table", Name: "FeatureFlag"}, why: "the kill switches: config data, because \"should this be on right now\" is an operational question with a live answer"},
		{write: "configs/schema/feature_flag.go", why: "name, enabled and a note for the operator who finds it off at 3am"},
		{write: "configs/table/feature_flag.csv", why: "three switches the demo actually reads: the store, respawns, opening activity windows"},
		{write: "game/flags/flags.go", why: "the flag names as constants, and the rule: a switch belongs at an entry point, never inside a transaction"},
		{write: "configs/schema/item.go", why: "the table's columns and rules"},
		{write: "configs/table/item.csv", why: "the rows; converted to configs/data/item.json by generate"},
		{add: &AddOptions{Kind: "errcode", Name: "ItemUnknown", ID: 100001}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "ItemCount", ID: 100002}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "BagFull", ID: 100003}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "ExpAmount", ID: 100004}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "NameEmpty", ID: 100005}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "ItemShort", ID: 100006}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "BattleNotFound", ID: 100007}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "BattleNotSeated", ID: 100008}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "BattleBusy", ID: 100009}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "DungeonRun", ID: 100010}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "MailClaim", ID: 100011}, why: "a coded failure in the manifest's errcode space"},
		{write: "internal/errors/item_unknown.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/item_count.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/bag_full.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/exp_amount.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/name_empty.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/item_short.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/battle_not_found.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/battle_not_seated.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/battle_busy.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/dungeon_run.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/mail_claim.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/scene_position.go", why: "one code for out of bounds / taken / not on a map: a client that learns which points are occupied has everyone's positions"},
		{write: "internal/errors/dungeon_claim_window.go", why: "a reward refused for being too old has to be a named refusal, not a quiet zero"},
		{write: "internal/errors/purchase_grant.go", why: "a grant with no order id or no payment moment cannot be made exactly-once"},
		{write: "internal/errors/player_elsewhere.go", why: "a login that lands on a process which does not own the player is refused by name, not served from a second copy of their Entity"},
		{write: "game/gameplay/attribute/combat.go", why: "the attribute profile: three attributes, one derived by formula, plus the dirty mask the generator writes through"},
		{write: "game/gameplay/attribute/combat_test.go", why: "the derived attribute follows its inputs and a container snapshot is a copy"},
		{write: "game/equipment/equipment.go", why: "which slots exist and what may go in one: a game decision, not the DAO's"},
		{write: "game/entities/player/equipment_component.go", why: "the demo's one nested write path: a struct in the DAO holding a map of pointers to another struct"},
		{write: "game/entities/player/equipment_component_test.go", why: "the nested write path both ways: a change two levels down reaches storage, a piece that came off stops reaching it"},
		{write: "db/migrations/player.go", why: "the v1 flat weapon id becomes a v2 equipment slot: what a zero value cannot cover"},
		{write: "db/migrations/player_test.go", why: "an old document keeps its weapon, one without a weapon still upgrades, and every numeric spelling of the old field is accepted"},
		{write: "db/migrations/restore_test.go", why: "the step runs on the path production loads through: a step that is correct and never called is the failure mode"},
		{write: "game/entities/player/map_component.go", why: "where the player is: the only door to the position, read and write both landing on the DAO"},
		{write: "game/entities/player/attribute_component.go", why: "the attribute container wired into an Entity: layers decided, composed, persisted and replicated"},
		{write: "game/entities/player/attribute_component_test.go", why: "the composition rule as a test: Final = Base + Gear with the derived attribute computed once over the composed inputs"},
		{write: "game/entities/player/sync_packer.go", why: "the client-facing half of sync=true: who serializes a subject, by profile and by dirty mask"},
		{add: &AddOptions{Kind: "entity", Name: "Scene"}, why: "the map is an Entity: an id, a lifecycle the framework drives, addressable by GM and by another process later"},
		{add: &AddOptions{Kind: "lifecycle", Name: "Scene", Service: gameService}, why: "the create/load boundary for scenes"},
		{add: &AddOptions{Kind: "entity", Name: "Monster"}, why: "the scene spawns things that are seen but do not see; the pipeline must not have a special case for them"},
		{add: &AddOptions{Kind: "component", Name: "Body", Entity: "Monster"}, why: "where a monster is and how much of it is left"},
		{add: &AddOptions{Kind: "dao", Name: "Monster", Entity: "Monster"}, why: "a DAO whose every field is nopersist,sync: nothing to store, everything to replicate"},
		{add: &AddOptions{Kind: "lifecycle", Name: "Monster", Service: gameService}, why: "the spawner's create/destroy boundary"},
		{write: "db/def/monster.go", why: "template, position and hp — all nopersist,sync, so a position still lives in a DAO"},
		{write: "game/entities/monster/entity.go", why: "ephemeral, replicated, rebuilt by the spawner on every start"},
		{write: "game/entities/monster/body_component.go", why: "the same position rule as the Player: read and written here, nowhere else"},
		{write: "game/entities/monster/sync_packer.go", why: "the Player's packer with a different DAO"},
		{write: "game/entities/monster/body_component_test.go", why: "the gate for a fully nopersist DAO: it must replicate and must never register a persistence mutation"},
		{write: "configs/schema/spawn.go", why: "the scene's population as config data: how many, where, how long before one comes back"},
		{write: "configs/table/spawn.csv", why: "three of one monster near the spawn point, back twenty seconds after dying"},
		{write: "game/scene/scene.go", why: "the contract between the Scene Entity and its systems: the map's shape, and what a system promises"},
		{write: "game/scene/runtime/runtime.go", why: "the scene runtime as a composition of named systems, started in order and stopped in reverse"},
		{write: "game/scene/runtime/terrain.go", why: "the map: bounds, walkability and occupancy, with its own lock"},
		{write: "game/scene/runtime/pathfind.go", why: "routing and placement: bounded A*, and the nearest free point searched in rings"},
		{write: "game/scene/runtime/runtime_test.go", why: "the map's promises: edges, occupancy, the nearest free point, no route through a wall, and systems that answer after they stop"},
		{write: "game/scene/runtime/relations.go", why: "a set-driven interest source: the shape every social relationship shares, fed by whoever owns the relationship"},
		{write: "game/scene/runtime/interest.go", why: "the AOI plus the relation sources, aggregated: the first source subscribes, the last one unsubscribes"},
		{write: "game/scene/runtime/interest_test.go", why: "the promises the AOI batch adds: self through a relation, hysteresis that does not churn, and a relation outliving the distance that also held the pair"},
		{write: "game/scene/runtime/refresh.go", why: "the population policy: count what exists rather than what was asked for, so a failed spawn is retried instead of leaked"},
		{write: "game/scene/runtime/refresh_test.go", why: "the population policy: asked once, an unreported spawn asked again, a death waits out the table's delay"},
		{write: "game/entities/scene/entity.go", why: "the map as an Entity: no DAO, rebuilt from configuration, exporting its systems by interface"},
		{write: "game/lifecycle/scene_singleton.go", why: "the one map this process runs, built from configuration before anyone can stand on it"},
		{write: "game/ranking/ranking.go", why: "the game's side of the rank service: which board, what a point is, and the run id that makes a submit idempotent"},
		{write: "game/dungeon/dungeon.go", why: "what a clear is worth and why paying for one exactly once needs a claim ledger, not the request's own flag"},
		{write: "game/dungeon/dungeon_test.go", why: "the claim window as a table test, shipped with the project"},
		{write: "game/battle/battle.go", why: "the lockstep contract: tick rate, frame budget, input encoding, seats and the deterministic simulation both clients run"},
		{add: &AddOptions{Kind: "saga", Name: "GiftItem", Service: gameService, Steps: []string{"debit", "deliver"}}, why: "the gift saga's definition and step subscriptions; the saga mod joins the game service"},
		{write: "game/gift/gift.go", why: "the game's side of the gift: state, id, mail text"},
		{add: &AddOptions{Kind: "handler", Name: "AddItem", Entity: "Player", Component: "Bag"}, why: "the write transaction"},
		{write: "game/handler/add_item.go", why: "handler parameters and result; the Sender and endpoint are generated from them"},
		{add: &AddOptions{Kind: "handler", Name: "AddExp", Entity: "Player", Component: "Profile"}, why: "the transaction that starts the event chain"},
		{write: "game/handler/add_exp.go", why: "two entity parameters (Player, World) in one transaction; the Sender becomes MultiSync_AddExp"},
		{add: &AddOptions{Kind: "handler", Name: "RecordEnter", Entity: "World", Component: "Stats"}, why: "World counts a login"},
		{write: "game/handler/record_enter.go", why: "a separate Nest call after the Player one"},
		{add: &AddOptions{Kind: "handler", Name: "RecordMatch", Entity: "World", Component: "Stats"}, why: "World counts a formed match"},
		{write: "game/handler/record_match.go", why: "called by the matchmaker after the remote Commit"},
		{add: &AddOptions{Kind: "handler", Name: "WorldStats", Entity: "World", Component: "Stats"}, why: "a read under the lock"},
		{write: "game/handler/world_stats.go", why: "returns a value, not the Entity"},
		{add: &AddOptions{Kind: "handler", Name: "PlayerLevel", Entity: "Player", Component: "Profile"}, why: "the ranked queue's score, read under the Player's lock"},
		{write: "game/handler/player_level.go", why: "a read handler on the Player"},
		{add: &AddOptions{Kind: "handler", Name: "StartGift", Entity: "Player", Component: "Bag"}, why: "the saga start: check under the lock, emit the start intent in the same WAL record"},
		{write: "game/handler/start_gift.go", why: "no mutation, one effect: saga.EmitStart"},
		{add: &AddOptions{Kind: "handler", Name: "GiftDebit", Entity: "Player", Component: "Bag"}, why: "the saga's debit step as a Nest transaction"},
		{write: "game/handler/gift_debit.go", why: "RemoveItem plus the command's receipt and completion, all in one WAL record"},
		{add: &AddOptions{Kind: "handler", Name: "GiftRefund", Entity: "Player", Component: "Bag"}, why: "debit's compensation, with the same saga identity"},
		{write: "game/handler/gift_refund.go", why: "AddItem plus the receipt and completion; a compensation that runs twice is as wrong as a step that does"},
		{add: &AddOptions{Kind: "handler", Name: "ClaimDungeon", Entity: "Player", Component: "Profile"}, why: "the clear reward, paid at most once per run"},
		{write: "game/handler/claim_dungeon.go", why: "the claim ledger and the reward commit together, so a replay pays nothing"},
		{add: &AddOptions{Kind: "handler", Name: "ClaimMailReward", Entity: "Player", Component: "Bag"}, why: "the mail attachment, granted at most once per mail"},
		{add: &AddOptions{Kind: "handler", Name: "EquipItem", Entity: "Player", Component: "Equipment"}, why: "taking an item out of the bag and putting it on must commit together, or the item is lost or duplicated"},
		{write: "game/handler/equip_item.go", why: "one transaction over three components: bag, equipment and the attribute layer that follows them"},
		{write: "game/handler/claim_mail_reward.go", why: "the same ledger shape for the claim the mail service cannot make atomic"},
		{write: "game/handler/claim_mail_reward_test.go", why: "the mail ledger asserted through the real handler in a real Nest transaction: the crash window a client cannot open"},
		{write: "game/handler/claim_dungeon_test.go", why: "the clear reward's ledger and its window asserted together: a pruned run is refused, not paid again"},
		{add: &AddOptions{Kind: "access", Name: "player", Service: gameService}, why: "the player request boundary"},
		{add: &AddOptions{Kind: "transport", Name: "tcp"}, why: "a transport a client can actually connect to"},
		{write: "internal/access/player/tcp/auth.go", why: "session tickets validated by the account service, plus a terminal shortcut"},
		{run: enableDemoPlayerTCP, why: "a listener that is actually on; doctor's player-tcp workflow passes on the generated project"},
		{add: &AddOptions{Kind: "protocol", Name: "AddItem", Group: "game", Handler: "player"}, why: "the wire message"},
		{write: "protocol/def/add_item.go", why: "request fields matching the handler parameters; response with code and count"},
		{add: &AddOptions{Kind: "endpoint", Name: "AddItem", Handler: "player"}, why: "protocol boundary to Nest sender"},
		{write: "game/controllers/player/add_item.go", why: "the error boundary: coded errors become the response, not a dropped connection"},
		{add: &AddOptions{Kind: "protocol", Name: "AddExp", Group: "game", Handler: "player"}, why: "the wire message"},
		{write: "protocol/def/add_exp.go", why: "request field matching the handler parameter; response with code and levels gained"},
		{write: "game/controllers/player/add_exp.go", why: "hand-written: add endpoint wires single-entity handlers, AddExp addresses two"},
		{add: &AddOptions{Kind: "protocol", Name: "EnterGame", Group: "game", Handler: "player"}, why: "the first message after the handshake"},
		{write: "protocol/def/enter_game.go", why: "no Nest handler behind it: creating a Player is a lifecycle operation"},
		{write: "game/controllers/player/controller.go", why: "the controller also holds the Player lifecycle"},
		{write: "game/controllers/player/enter_game.go", why: "hand-written endpoint: GetOrCreate the Player, then answer"},
		{add: &AddOptions{Kind: "protocol", Name: "JoinQueue", Group: "game", Handler: "player"}, why: "the cross-service call: game → match"},
		{write: "protocol/def/join_queue.go", why: "no Nest handler behind it either: the match service is another process"},
		{write: "game/controllers/player/join_queue.go", why: "Enqueue through the typed match client, frame sequence as idempotency key"},
		{add: &AddOptions{Kind: "protocol", Name: "PollMatch", Group: "game", Handler: "player"}, why: "reading the ticket and the match"},
		{write: "protocol/def/poll_match.go", why: "state, match id, members"},
		{write: "game/controllers/player/poll_match.go", why: "ownership-checked reads through the match client"},
		{add: &AddOptions{Kind: "protocol", Name: "WorldStats", Group: "game", Handler: "player"}, why: "the World's counters"},
		{write: "protocol/def/world_stats.go", why: "two counters"},
		{write: "game/controllers/player/world_stats.go", why: "a Nest read handler on the World"},
		{add: &AddOptions{Kind: "protocol", Name: "MatchFound", Group: "game", Handler: "player"}, why: "the server push announcing a match"},
		{write: "protocol/def/match_found.go", why: "a notify: no request, the bind registers its encoder"},
		{write: "game/chatroom/chatroom.go", why: "the game's side of the chat contract: channels, the text type, who is online"},
		{add: &AddOptions{Kind: "protocol", Name: "SendChat", Group: "game", Handler: "player"}, why: "the cross-service call: game → chat"},
		{write: "protocol/def/send_chat.go", why: "kind, target, text; the stored sequence comes back"},
		{write: "game/controllers/player/send_chat.go", why: "Publish through the typed chat client, then fan the stored line out"},
		{add: &AddOptions{Kind: "protocol", Name: "ChatHistory", Group: "game", Handler: "player"}, why: "the reconnect path: page a channel forwards"},
		{write: "protocol/def/chat_history.go", why: "cursor in, lines and cursor out"},
		{write: "game/controllers/player/chat_history.go", why: "History through the typed chat client, viewer checked by the service"},
		{add: &AddOptions{Kind: "protocol", Name: "ChatMessage", Group: "game", Handler: "player"}, why: "the server push carrying one chat line"},
		{write: "protocol/def/chat_message.go", why: "a notify with the same shape history returns"},
		{write: "game/controllers/player/chat_push.go", why: "Message → push, and who receives it: presence for world, the pair for private"},
		{write: "game/rewards/rewards_test.go", why: "补丁点的三条：没注册时用原函数、替换立刻生效、revert 回到原样"},
		{write: "game/rewards/rewards.go", why: "what a mail attachment means to this game; shared by the mailer and the claim endpoint"},
		{add: &AddOptions{Kind: "protocol", Name: "ListMail", Group: "game", Handler: "player"}, why: "the mailbox, paged"},
		{write: "protocol/def/list_mail.go", why: "cursor in, envelopes with this player's state out"},
		{write: "game/controllers/player/list_mail.go", why: "List through the typed mail client"},
		{add: &AddOptions{Kind: "protocol", Name: "ClaimMail", Group: "game", Handler: "player"}, why: "the attachment into the Bag"},
		{write: "protocol/def/claim_mail.go", why: "mail id in, what was granted out"},
		{write: "game/controllers/player/claim_mail.go", why: "ReserveClaim → Sync_AddItem → CommitClaim: exactly-once delivery around one Nest transaction"},
		{add: &AddOptions{Kind: "protocol", Name: "EnterDungeon", Group: "game", Handler: "player"}, why: "the cross-service call: game → session"},
		{write: "protocol/def/enter_dungeon.go", why: "a bounded run: id, state, deadline"},
		{write: "game/controllers/player/enter_dungeon.go", why: "Enter through the typed session client, frame sequence as idempotency key"},
		{add: &AddOptions{Kind: "protocol", Name: "FinishDungeon", Group: "game", Handler: "player"}, why: "resolving the run"},
		{write: "protocol/def/finish_dungeon.go", why: "the verdict in, terminal state and the clear's exp out"},
		{write: "game/controllers/player/finish_dungeon.go", why: "Finish through the session client, then the clear's exp through the AddExp transaction"},
		{add: &AddOptions{Kind: "handler", Name: "EnterScene", Entity: "Player", Component: "Map"}, why: "placing a player: the scene resolves the point, the player writes it"},
		{write: "game/handler/enter_scene.go", why: "a remembered position that is no longer usable becomes the nearest free one, not a failed login"},
		{add: &AddOptions{Kind: "handler", Name: "PlayerPosition", Entity: "Player", Component: "Map"}, why: "a refused move answers with where the player still is; there is no cached copy to read instead"},
		{write: "game/handler/player_position.go", why: "the read, under the Player's lock"},
		{add: &AddOptions{Kind: "handler", Name: "MovePlayer", Entity: "Player", Component: "Map"}, why: "moving holds the Scene and the Player together: the stored position and the ground handed out have to agree"},
		{write: "game/handler/move_player.go", why: "the two-entity move; the terrain check stays in the component that owns the write"},
		{add: &AddOptions{Kind: "protocol", Name: "Move", Group: "game", Handler: "player"}, why: "the movement endpoint"},
		{write: "protocol/def/move.go", why: "ask for a point, get back the one the server settled on"},
		{write: "game/controllers/player/move.go", why: "resolve the scene, then the move transaction"},
		{add: &AddOptions{Kind: "protocol", Name: "Equip", Group: "game", Handler: "player"}, why: "wearing an item: the slot is the item's property, not the client's choice"},
		{write: "protocol/def/equip.go", why: "ask with an item id, get back the slot it went in and what came off"},
		{write: "game/controllers/player/equip.go", why: "the endpoint only addresses the transaction; every decision is under the Player's lock"},
		{write: "game/activity/activity.go", why: "what this game means by a timed server-wide event: window ids derived from the clock, what a point is worth, what settlement pays, and the board the game keeps because the coordinator has no enumeration"},
		{write: "game/effects/activity_phase.go", why: "the timer's effect: a bus call recorded inside the World's lock and made outside it"},
		{write: "game/entities/world/timer_component.go", why: "the World's timer heap: persisted, rebuilt on load, fired inside the transaction that removes the node"},
		{write: "game/entities/world/timer_component_test.go", why: "the part only a test can show: a deadline armed before a restart still fires after one"},
		{add: &AddOptions{Kind: "handler", Name: "TickWorldTimers", Entity: "World", Component: "Timer"}, why: "the tick comes from a ticker; the firing happens under the World's lock"},
		{write: "game/handler/tick_world_timers.go", why: "one call, and what it returns is how many deadlines are still armed"},
		{add: &AddOptions{Kind: "handler", Name: "ArmActivity", Entity: "World", Component: "Timer"}, why: "every process arms the same window, so arming is idempotent per activity"},
		{write: "game/handler/arm_activity.go", why: "a second node for one window is a heap that grows by one per restart"},
		{add: &AddOptions{Kind: "handler", Name: "SettleActivity", Entity: "World", Component: "Stats"}, why: "what the coordinator's result PAYS is the game's, recorded once per activity"},
		{write: "game/handler/settle_activity.go", why: "mail first, record second, ack last — each step safe to repeat"},
		{add: &AddOptions{Kind: "handler", Name: "ActivitySettled", Entity: "World", Component: "Stats"}, why: "the standing endpoint reads it through the lock rather than from process memory"},
		{write: "game/handler/activity_settled.go", why: "an in-process flag starts empty after a deploy and tells a paid player they were not paid"},
		{add: &AddOptions{Kind: "protocol", Name: "ActivityStanding", Group: "game", Handler: "player"}, why: "where this player stands in the current window"},
		{write: "protocol/def/activity_standing.go", why: "the coordinator's aggregation and this server's own settlement flag, kept apart"},
		{write: "game/controllers/player/activity_standing.go", why: "two sources, deliberately not merged: complete is not paid"},
		{write: "game/purchase/purchase.go", why: "what the game process and the platform process both have to agree on about a paid order: catalogue, key layout, grant record, claim window"},
		{add: &AddOptions{Kind: "handler", Name: "GrantPurchase", Entity: "Player", Component: "Bag"}, why: "a paid order becomes items: the order id lands in the same WAL record as the grant"},
		{write: "game/handler/grant_purchase.go", why: "the third instance of one shape: authoritative moment in, identity recorded in the same transaction, admission and pruning the same predicate"},
		{write: "game/handler/grant_purchase_test.go", why: "the replay a client cannot produce: the same order drained twice grants once, and a grant past its window is refused rather than repeated"},
		{add: &AddOptions{Kind: "entity", Name: "Guild"}, why: "the demo's first entity that does not belong to one process"},
		{write: "db/def/guild.go", why: "the roster: a map of members, written under a distributed lock"},
		{write: "game/entities/guild/entity.go", why: "remote=managed and EntityCategoryRemote: the distributed lock is taken before any local mutex, so a remote kind must rank first"},
		{add: &AddOptions{Kind: "lifecycle", Name: "Guild", Service: gameService}, why: "founding creates the aggregate before anything can lock it: a remote entity is loaded from the store, so it has to be in the store"},
		{write: "game/entities/guild/roster_component.go", why: "found / join / snapshot, all under the guild's lock — a handler cannot tell it crossed a network"},
		{write: "internal/errors/guild_name.go", why: "a coded failure in the manifest's errcode space"},
		{write: "internal/errors/guild_exists.go", why: "founding is insert-only on the guild's own state: two processes racing produce one guild and one refusal"},
		{write: "internal/errors/guild_missing.go", why: "\"you joined nothing\" and \"the guild is gone\" are different things to a player"},
		{write: "internal/errors/guild_full.go", why: "the roster is one locked document per write, so its size is a real limit"},
		{write: "internal/errors/guild_busy.go", why: "拿不到远端锁是可重试的暂时答案（最常见的原因是进程刚重启、前任的租约还没过期），不是内部错误"},
		{add: &AddOptions{Kind: "handler", Name: "FoundGuild", Entity: "Guild", Component: "Roster"}, why: "two entities, guild first: the remote lock is taken at the top of the dispatch"},
		{write: "game/handler/found_guild.go", why: "the player's copy of \"which guild\" is written in the same transaction as the roster"},
		{add: &AddOptions{Kind: "handler", Name: "JoinGuild", Entity: "Guild", Component: "Roster"}, why: "the demo's one operation that genuinely crosses processes"},
		{write: "game/handler/join_guild.go", why: "a remote entity and a local one held together; the handler cannot tell the difference"},
		{add: &AddOptions{Kind: "handler", Name: "GuildInfo", Entity: "Guild", Component: "Roster"}, why: "a read still takes the distributed lock; saying so beats pretending it is free"},
		{write: "game/handler/guild_info.go", why: "what a mirror would be for, and why this demo does not use one"},
		{add: &AddOptions{Kind: "handler", Name: "PlayerGuild", Entity: "Player", Component: "Profile"}, why: "\"which guild am I in\" costs a local lock, not a distributed one"},
		{write: "game/handler/player_guild.go", why: "the player's own copy, written by the join transaction"},
		{add: &AddOptions{Kind: "protocol", Name: "Guild", Group: "game", Handler: "player"}, why: "the guild endpoints: found, join, read"},
		{write: "protocol/def/guild.go", why: "three messages behind one response shape, carrying which process served the call"},
		{write: "game/controllers/player/guild.go", why: "read against the dungeon endpoints: the remote entity changes nothing at this layer"},
		{add: &AddOptions{Kind: "protocol", Name: "Purchase", Group: "game", Handler: "player"}, why: "buying: the demo plays the payment provider, everything around that is real"},
		{write: "protocol/def/purchase.go", why: "a product id in; the order, the receipt's replay flag and the bag count out"},
		{write: "game/controllers/player/purchase.go", why: "sign a callback, let the platform service record and deliver it, then drain the grant into the bag"},
		{add: &AddOptions{Kind: "protocol", Name: "RankTop", Group: "game", Handler: "player"}, why: "reading a leaderboard: a bounded page, and the board is the server's choice"},
		{write: "protocol/def/rank_top.go", why: "the board page on the wire, with this player's own rank alongside it"},
		{write: "game/controllers/player/rank_top.go", why: "Page + Rank through the typed rank client"},
		{add: &AddOptions{Kind: "skill", Name: "Fireball"}, why: "one skill definition and the embedded catalog"},
		{write: "game/skills/fireball.json", why: "the definition with its contract written down; compiled at startup"},
		{add: &AddOptions{Kind: "protocol", Name: "SkillCatalog", Group: "game", Handler: "player"}, why: "what the server compiled"},
		{write: "protocol/def/skill_catalog.go", why: "skill ids and the warning count"},
		{write: "game/controllers/player/skill_catalog.go", why: "compile once, list the programs"},
		{add: &AddOptions{Kind: "protocol", Name: "SendGift", Group: "game", Handler: "player"}, why: "the saga start from a client"},
		{write: "protocol/def/send_gift.go", why: "recipient, item, count in; the saga id out"},
		{write: "game/controllers/player/send_gift.go", why: "StartGift through the Nest sender; the saga id is sender + session + frame sequence"},
		{add: &AddOptions{Kind: "protocol", Name: "BattleInput", Group: "game", Handler: "player"}, why: "one client's contribution to one lockstep frame"},
		{write: "protocol/def/battle_input.go", why: "input, keyframe hash and catch-up request on one per-frame message"},
		{write: "game/controllers/player/battle_input.go", why: "hands the message to the battle room's goroutine; no Entity is locked"},
		{add: &AddOptions{Kind: "protocol", Name: "BattleFrame", Group: "game", Handler: "player"}, why: "the server push carrying one lockstep broadcast packet"},
		{write: "protocol/def/battle_frame.go", why: "a notify: the packet the room encoded, redundancy included"},
		{write: "protocol/def/entity_sync.go", why: "the scene's state frames on the wire: reliable snapshots and fragmented deltas in one push"},
		{add: &AddOptions{Kind: "protocol", Name: "GiftStatus", Group: "game", Handler: "player"}, why: "how the saga went"},
		{write: "protocol/def/gift_status.go", why: "the coordinator's status, step and last error"},
		{write: "game/controllers/player/gift_status.go", why: "Engine.Get through the saga capability; unknown until the start has been consumed"},
		{run: enableDemoMatchSweep, why: "the match process sweeps the duel queue for expired tickets"},
		{write: "loadtest/playertcp/conn.go", why: "the robot transport that speaks the generated server's frame"},
		{write: "loadtest/scenarios/demo.yaml", why: "one robot's life, as a scenario tree"},
		{write: "cmd/loadtest/main.go", why: "robots + thresholds: the load test that is also the regression test"},
		{write: "deploy/dev/observability/docker-compose.yaml", why: "Prometheus + Grafana for a developer machine, apart from the regenerated infrastructure compose"},
		{write: "deploy/dev/observability/prometheus.yml", why: "scrape the three ops endpoints and the load test"},
		{write: "deploy/dev/observability/grafana/provisioning/datasources/prometheus.yaml", why: "the provisioned datasource"},
		{write: "deploy/dev/observability/grafana/provisioning/dashboards/dashboards.yaml", why: "load dashboards from the mounted directory"},
		{write: "deploy/dev/observability/grafana/dashboards/roost-demo.json", why: "the dashboard, one row per step of the chain"},
		{write: "deploy/dev/observability/README.md", why: "metric ↔ chain step ↔ what to look at"},
		{write: "internal/service/game/service.go", why: "the game service starts the effect consumer in Init and drains it in Shutdown"},
		{write: "internal/service/game/level_up_mail.go", why: "the consumer: JetStream durable + Mongo inbox → mail.Send keyed by EffectID"},
		{write: "internal/service/game/matchmaker.go", why: "Candidates → Grouping → Commit on a ticker, then the World records the match and the players are pushed MatchFound"},
		{write: "internal/service/game/battle_test.go", why: "the room's start grace and lifetime, asserted against the real lockstep room with a recording push lane"},
		{write: "internal/service/game/battle.go", why: "the lockstep rooms: one goroutine owns each room, the player TCP push is its broadcast lane, the matchmaker opens one per match"},
		{write: "internal/service/game/scene.go", why: "the other half of sync=true: a room that holds every online player as a subject and pushes their deltas to the others, gated on the Data Engine's durable watermark"},
		{write: "internal/service/game/scene_test.go", why: "the replication claim end to end: one player's DAO change decodes as a delta on another player's wire"},
		{write: "internal/service/game/gift_saga.go", why: "the gift saga's four step consumers: debit / refund as Nest transactions, deliver as a mail, each idempotent per command through the Mongo step inbox"},
		{write: "game/runtimeid/runtimeid.go", why: "ids for entities the process creates at run time: the sid goes in the id, so two processes cannot mint the same one"},
		{write: "game/runtimeid/runtimeid_test.go", why: "two shards never collide, an unencodable sid fails at startup, exhaustion refuses instead of wrapping"},
		{write: "internal/service/game/spawner.go", why: "the population policy turned into Entities: create, place, replicate, and only then count"},
		{write: "internal/service/game/spawner_test.go", why: "where the sid comes from: the registry's config, not the runtime-config slot the config-data Mod overwrites"},
		{write: "internal/service/game/gm.go", why: "GM commands on the admin registry: add item / add exp / send mail / world stats, served by ops over HTTP behind a token"},
		{run: enableDemoAdmin, why: "the dev config enables the ops admin endpoint with a dev token, so the GM commands are reachable on a developer machine"},
		{write: "cmd/accountctl/main.go", why: "the operator surface account keeps off the bus: register the game server so CreateRole works"},
		{write: "internal/service/game/flags_test.go", why: "启动即表里的值、reload 跟着变、表里删掉的开关消失、缺表时拒绝发布"},
		{write: "internal/service/game/flags.go", why: "the table is the source and the store is rebuilt from it on every reload; also the one place that says what may be hot-patched"},
		{write: "game/playerroute/playerroute.go", why: "who owns which player: a Redis claim with a lease, because two processes sharing one database are two writers of the same documents"},
		{write: "game/playerroute/playerroute_test.go", why: "the ownership rules: one owner at a time, refresh and release only our own, a lapsed lease frees the player"},
		{write: "internal/service/game/playerowner.go", why: "claim at login, refresh while online, release on the last close, and the question the shared consumers ask before touching a player"},
		{write: "internal/service/game/playerowner_test.go", why: "租约丢了要停服务：失去时围栏、不确定时让准入自然走到头、未经确认的 sid 不算所有权"},
		{write: "internal/service/game/gift_handoff_test.go", why: "the handoff decisions: admit the owner's own step, refuse and forward a foreign one, never claim an idle player to have somewhere to send it"},
		{write: "internal/service/game/presence.go", why: "the other half of RR-20260918-06: chat presence follows the same session-close source the scene does"},
		{write: "internal/service/game/activity.go", why: "this server's lease, the World tick, the window loop, the phase effect consumer and the settlement: mail → record → ack"},
		{write: "internal/service/game/purchase_drain.go", why: "the game side of the platform handover: grant under the Player's lock, then delete the record — never the other order"},
		{write: "internal/service/platform/collaborators.go", why: "a platform service that verifies a demo channel, resolves the player and records a durable grant instead of pretending it can reach an Entity"},
		{run: demoActivityKeys, why: "the game keeps its contributor board beside the coordinator's keys, and the candidate sid set is the deployment's"},
		{run: demoPaymentSecrets, why: "the platform service refuses to start without its two secrets; the game process signs its simulated callbacks with the same payment secret"},
		{write: "internal/service/account/collaborators.go", why: "an account service that can log a demo user in and mint ids from Redis"},
		{write: "internal/service/chat/collaborators.go", why: "a chat service with a written-down policy, one text type and a granted system path"},
		{write: "internal/service/session/collaborators.go", why: "a session service whose releaser frees the demo's (resource-less) dungeon"},
	}
}

// scaffoldDemoTemplate builds the demo on top of the game template: Player
// gains Profile and Bag components backed by a DAO, an item table and three
// coded errors, one Nest write transaction adds an item, a TCP endpoint
// carries it and turns failures into coded responses, and the account service
// gets collaborators that work. Everything it writes is application-owned and
// written once.
func scaffoldDemoTemplate(root string, m Manifest, gameService string) ([]string, error) {
	created, err := scaffoldGameTemplate(root, m, gameService)
	if err != nil {
		return created, err
	}
	vars := demoVars{module: m.Project.Module, gameService: gameService, project: m.Project.Name}
	for _, step := range demoScaffoldSteps(gameService) {
		if step.add != nil {
			files, addErr := Add(root, *step.add)
			if addErr != nil {
				return created, fmt.Errorf("template %s: add %s %s (%s): %w", demoTemplateName, step.add.Kind, step.add.Name, step.why, addErr)
			}
			created = append(created, files...)
			continue
		}
		if step.run != nil {
			if runErr := step.run(root, gameService); runErr != nil {
				return created, fmt.Errorf("template %s: %s: %w", demoTemplateName, step.why, runErr)
			}
			continue
		}
		written, writeErr := writeDemoFile(root, vars, step.write)
		if writeErr != nil {
			return created, fmt.Errorf("template %s: write %s (%s): %w", demoTemplateName, step.write, step.why, writeErr)
		}
		created = append(created, written)
	}
	if err := Generate(root, GenerateOptions{Stdout: io.Discard}); err != nil {
		return created, fmt.Errorf("template %s: regenerate: %w", demoTemplateName, err)
	}
	return created, nil
}
