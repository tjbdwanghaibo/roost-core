package roost

import (
	"context"
	"fmt"
	"go/format"
	"io"
	"io/fs"
	"net"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/tjbdwanghaibo/roost-core/demo"
	"gopkg.in/yaml.v3"
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
	// demo 只在 project new 时写出，目录是新建的：用新工程的小写包名（D8）。
	body = strings.ReplaceAll(body, demoGameServicePackagePlaceholder, defaultServicePackage(vars.gameService))
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
	if moduleIDPlaceholder.MatchString(body) {
		// demo 覆盖模块的业务文件时沿用模块安装时分配到的号（roost.yaml 的 modules 记录）。
		if body, err = applyInstalledModuleIDs(root, rel, body); err != nil {
			return "", err
		}
	}
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

// writeDemoActorBody 让分区世界示例与 actor 模块共用同一份移动机制。
// demo 的 MapComponent 仍负责其分区、地形与连接规则，因此不安装另一套闲置的 Actor 组件和 DAO 状态。
func writeDemoActorBody(root, gameService string) error {
	manifest, err := LoadManifest(root)
	if err != nil {
		return err
	}
	const rel = "game/actor/body.go"
	body, err := renderModuleFile(manifest, gameService, ModuleSpec{Name: "actor"}, "files", rel, nil)
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(root, filepath.FromSlash(rel)), body, 0o644)
}

// applyDemoTemplate is the game template plus what the demo's own code needs
// from the manifest: protocol (its request message), dao (its persistent
// Player state), config (the item table) and errcode (its coded failures).
// All are on by default; a caller who narrowed -features would otherwise get
// a project that cannot build.
func applyDemoTemplate(m *Manifest, gameService string) error {
	// demo 调用了全部九个托管框架服务（账号、聊天、邮件、匹配、活动、路由、支付、排行、会话），
	// 所以显式托管全部；-template game 缺省一个都不带。
	if err := applyGameTemplate(m, gameService, frameworkServiceNames()); err != nil {
		return err
	}
	// The game process reads the platform service's grant records out of the
	// same Redis that service writes them to (the purchase module). It is the only cross-process
	// handover in the demo that is not a bus call, and it is one because the
	// delivering side has to make it durable before it may answer "delivered".
	game := m.Services[gameService]
	if resolved, err := resolveMods(game.Mods); err == nil && !contains(resolved, "redis") {
		game.Mods = append(game.Mods, "redis")
		m.Services[gameService] = game
	}
	// The alliance is the alliance service's (demo_alliance.go): every alliance
	// and every player's membership guard is owned and written by that
	// service, and the game reads them through the Remote mirror the
	// remote_entity Mod also serves. The game has remote_entity for the one
	// Remote kind it owns itself: a
	// partitioned scene's Team (service=<game>, shared between the game
	// processes that are its members' Bases; FEATURE-2026-11-03-seamless-world
	// §14). The alliance kinds are foreign to it (service=alliance), so its
	// Remote runtime refuses to claim or write them
	// (entity.ErrRemoteKindNotOwned): the alliance RPC stays the only path to
	// a alliance write (decision S of the alliance plan), held by the framework's
	// guard rather than by the game lacking the runtime. A caller who narrowed
	// -mods still gets it: the Team cannot be written without it. uses_rpcs:
	// [alliance] is added once the rpc exists (demoGameUsesAlliance).
	game = m.Services[gameService]
	writer := make([]string, 0, len(game.Mods)+1)
	for _, mod := range game.Mods {
		if mod != "remote_mirror" { // the read-only mirror and remote_entity exclude each other
			writer = append(writer, mod)
		}
	}
	if !contains(writer, "remote_entity") {
		writer = append(writer, "remote_entity")
	}
	game.Mods = writer
	m.Services[gameService] = game
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
	// modules 安装业务模块（roost module add）：demo 与模块共用同一份实现的功能由它装进来，
	// demo 自己不再带一份。之后的 write 步骤可以覆盖模块写下的业务文件（demo 在模块之上的定制）。
	modules []string
	why     string
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
	// Line by line: the ops section is rendered from the ops Mod's declaration
	// and its help comments sit between the keys.
	text := string(raw)
	for _, edit := range []struct{ before, after string }{
		{"  admin_enabled: false\n", "  admin_enabled: true\n"},
		{"  admin_token: \"\"\n", "  admin_token: dev-gm-token\n"},
		{"  allow_dev_token: false\n", "  allow_dev_token: true\n"},
	} {
		if strings.Count(text, "\n"+edit.before) != 1 {
			return fmt.Errorf("%s: expected the ops admin block to replace", path)
		}
		text = strings.Replace(text, "\n"+edit.before, "\n"+edit.after, 1)
	}
	return writeAtomic(path, []byte(text), 0o644)
}

// enableDemoPlayerTCP flips player_access.tcp.enabled in the game service's
// config, the same edit `roost config enable player-tcp` makes. A demo whose
// listener is off cannot be connected to, and `roost project doctor
// -workflow player-tcp` would fail on the project it just generated.
func enableDemoPlayerTCP(root, gameService string) error {
	_, err := ensurePlayerTCPConfig(root, gameService, "", true)
	if err != nil {
		return err
	}
	// 这个模板依赖 PlayerOwners 闸门。TCP Mod 先启动，Service.Init 后注册闸门；
	// 三份新工程配置都显式要求它，避免这段启动窗口执行未归属的玩家请求。
	for _, rel := range []string{
		"configs/service/config." + gameService + ".yaml",
		"configs/service/config." + gameService + ".prod.example.yaml",
		"deploy/k8s/base/secret." + gameService + ".example.yaml",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) && strings.HasPrefix(rel, "deploy/") {
			continue
		}
		if err != nil {
			return err
		}
		text := string(raw)
		at := strings.Index(text, "player_access:\n")
		if at < 0 {
			return fmt.Errorf("%s: missing player_access block", rel)
		}
		start := strings.LastIndexByte(text[:at], '\n') + 1
		indent := text[start:at]
		text = strings.Replace(text, "player_access:\n", "player_access:\n"+indent+"  require_write_gate: true\n", 1)
		if err := os.WriteFile(path, []byte(text), 0644); err != nil {
			return err
		}
	}
	return nil
}

// demoGiftRefundBudget gives the gift saga's debit step — whose compensation
// is the refund — a retry budget that outlasts a crash restart of the
// sender's sid.
//
// Step budgets are configuration (saga.step_defaults, overridden per step
// under saga.steps.<type>.<step>), so this edits the game service's
// configs rather than the generated definition. The default budget is 5
// attempts of 5s, backoff 100ms..5s: at least ~26s before the step counts as
// exhausted. The refund only runs on the sid the sender is bound to; while
// that process restarts every attempt just times out, and an exhausted
// compensation is manual_required. A restart can take singleton.startup_wait
// (30s) + singleton.ttl (15s) + the time to be started and run Init (45s
// allowed): 90s, hence max_attempts 15. A step's budget is shared by both
// directions, so the debit's forward attempts get the same 15 — harmless: at
// most one attempt of one operation takes effect, and a debit that never ran
// took nothing. The relation is pinned by
// internal/service/<game>/gift_saga_budget_test.go, which reads these configs.
func demoGiftRefundBudget(root, gameService string) error {
	for _, rel := range []string{
		"configs/service/config." + gameService + ".yaml",
		"configs/service/config." + gameService + ".prod.example.yaml",
		"deploy/k8s/base/secret." + gameService + ".example.yaml",
	} {
		path := filepath.Join(root, filepath.FromSlash(rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) && strings.HasPrefix(rel, "deploy/") {
			continue
		}
		if err != nil {
			return err
		}
		const marker = "# steps.<saga type>.<step name>.<timeout|max_attempts|backoff_min|backoff_max>.\n"
		at := strings.Index(string(raw), marker)
		if at < 0 {
			return fmt.Errorf("%s: expected the saga step override block", path)
		}
		lineStart := strings.LastIndexByte(string(raw[:at]), '\n') + 1
		indent := string(raw[lineStart:at])
		before := marker + indent + "steps: {}\n"
		after := marker +
			indent + "# debit's compensation is the refund, which only runs on the sender's sid: 15\n" +
			indent + "# attempts (>= 98s with backoff) outlast a crash restart of that sid\n" +
			indent + "# (startup_wait + ttl + 45s = 90s); see internal/service/" + gameService + "/gift_saga.go.\n" +
			indent + "steps:\n" + indent + "  gift_item:\n" + indent + "    debit:\n" + indent + "      max_attempts: 15\n"
		if !strings.Contains(string(raw), before) {
			return fmt.Errorf("%s: expected an empty saga.steps block to replace", path)
		}
		if err := writeAtomic(path, []byte(strings.Replace(string(raw), before, after, 1)), 0o644); err != nil {
			return err
		}
	}
	return nil
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
	// generates a platform block for it. The production configs get the same
	// block with the secret left as CHANGE_ME, like the platform service's own
	// production example: the two must be set to the same real value.
	block := "platform:\n  key_prefix: " + blockKeyPrefix(string(raw), "platform") + "\n  payment_secret: %s\n"
	return appendDemoGameConfig(root, gameService, "platform",
		fmt.Sprintf(block, "dev-platform-payment-secret"), fmt.Sprintf(block, "CHANGE_ME"))
}

// appendDemoGameConfig appends one top-level section the demo's game code
// reads at Init to all three of the game service's repository configs: the
// dev config, the production example, and the k8s Secret example that embeds
// the production example under config.yaml. Only the dev
// config used to get these sections, so a game started from the production
// example — or from the Secret — stopped at Init ("platform.payment_secret is
// empty", then "activity.key_prefix is empty"). The production text goes through productionizeConfig like the rest
// of the example. A file that already has the key is left alone, so hand
// edits stay; a missing file (no k8s deploy target) is skipped.
func appendDemoGameConfig(root, gameService, key, dev, production string) error {
	production = productionizeConfig(production)
	for _, target := range []struct {
		rel, marker, block string
	}{
		{"configs/service/config." + gameService + ".yaml", "\n" + key + ":\n", dev},
		{"configs/service/config." + gameService + ".prod.example.yaml", "\n" + key + ":\n", production},
		// The Secret's config.yaml is a literal block indented by four spaces
		// and is the last thing in the file (renderKubernetesSecretExample).
		{"deploy/k8s/base/secret." + gameService + ".example.yaml", "\n    " + key + ":\n", indentText(production, "    ")},
	} {
		path := filepath.Join(root, filepath.FromSlash(target.rel))
		raw, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		body := string(raw)
		if strings.Contains(body, target.marker) {
			continue
		}
		if body != "" && !strings.HasSuffix(body, "\n") {
			body += "\n"
		}
		if err := writeAtomic(path, []byte(body+target.block), 0o644); err != nil {
			return err
		}
	}
	return nil
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
// keyspace is and which activity groups file says what group it is in.
//
// The prefix is read from the coordinator's own config rather than written
// twice: the game keeps its contributor board BESIDE the coordinator's keys,
// and two files that can disagree about where that is means a settlement
// reading an empty board. The group comes from the activity groups file the
// coordinator reads too (decision C4); it replaced a per-service
// activity.game_sids list, so the game servers of one activity are written
// down once.
func demoActivityKeys(root, gameService string) error {
	activityPath := filepath.Join(root, "configs", "service", "config.activity.yaml")
	activityRaw, err := os.ReadFile(activityPath)
	if err != nil {
		return err
	}
	// The same text for the production configs: neither value is a secret,
	// and the image and the shell release carry the groups file at the same
	// relative path.
	block := "activity:\n  key_prefix: " + blockKeyPrefix(string(activityRaw), "activity") +
		"\n  # The activity groups file: this server takes part in the group that\n" +
		"  # lists its sid, and the members whose process is up (the App singleton\n" +
		"  # lock's Live query) are what an activity waits for. The game refuses to\n" +
		"  # start when its sid is in no group or the file breaks a rule in it.\n" +
		"  groups_file: " + activityGroupsFile + "\n"
	return appendDemoGameConfig(root, gameService, "activity", block, block)
}

func demoScaffoldSteps(gameService string) []demoScaffoldStep {
	steps := []demoScaffoldStep{
		{add: &AddOptions{Kind: "dao", Name: "Player", Entity: "Player"}, why: "persistence for the Player's components"},
		{write: "db/restore_test.go", why: "current schema restores; mismatched schemas leave DAO unchanged"},
		{write: "db/def/player.go", why: "the persistent fields the generated mutators are built from"},
		{write: "db/migrations/player.go", why: "the offline schema 1 -> current step (flat weapon_id becomes an equipment slot) and its check through the generated RestorePersisted"},
		{write: "db/migrations/offline.go", why: "offline migration on a stopped collection: dry-run, backup, conditional rewrite, verify and rollback"},
		{write: "db/migrations/player_test.go", why: "plan writes nothing, apply backs up and rewrites, any failed check writes nothing, a live writer stops the run, rollback refuses documents written afterwards; the same flow on real Mongo"},
		{write: "cmd/schemamigrate/main.go", why: "the operator tool for the migration: plan | apply | verify | rollback against the game's Mongo"},
		// 档案（名字、等级、经验）是 profile 模块的：ProfileComponent 与 PlayerDao.Profile / ProfilePrivate 在最后的模块步骤
		// 里装进来（demoModuleSteps）。副本领奖账本不属于档案，是 demo 自己的组件 DungeonLedger，状态字段在 demo 自己的
		// PlayerDao 里；它的接线（领奖事务的 IDungeonClaimEntity 引用 ProfileComponent）与后面引用 ProfileComp() 的 handler
		// 一样，等模块装好之后才一起编译。
		{write: "game/components/dungeonledger/dungeonledger.go", why: "the exactly-once ledger for dungeon clear rewards and the per-season clear count, pruned on the write path"},
		{write: "game/entities/player/dungeon_ledger_component.go", why: "wiring only: the component type, its name on Player (DungeonLedger), the factory registration, and the claim transaction's Player (ledger and profile in one lock); the logic is game/components/dungeonledger/dungeonledger.go"},
		{write: "game/view/component_dungeonledger.go", why: "the DungeonLedger interface other game packages use and its Holder (view.PlayerEntity embeds it)"},
		{write: "game/entities/player/entity.go", why: "EntityCategoryPlayer: the kind's lock rank, decided before the first document is persisted"},
		{add: &AddOptions{Kind: "component", Name: "Stats", Entity: "World"}, why: "World's one job: server-wide counters"},
		{add: &AddOptions{Kind: "dao", Name: "World", Entity: "World"}, why: "persistence for the counters"},
		{write: "db/def/world.go", why: "PlayersEntered and MatchesFormed"},
		{write: "game/components/stats/stats.go", why: "count logins and matches through generated mutators; snapshot for reads"},
		{write: "game/entities/world/stats_component.go", why: "wiring only: the component type, its name on World (Stats), the factory registration; the logic is game/components/stats/stats.go"},
		{write: "game/view/component_stats.go", why: "the Stats interface other game packages use and its Holder (view.WorldEntity embeds it)"},
		{write: "game/entities/world/entity.go", why: "EntityCategoryWorld: locked before Player in the two-entity AddExp"},
		{write: "game/matchmaking/queue.go", why: "the duel and ranked queues, their grouping policies, and how a player is named in them"},
		{add: &AddOptions{Kind: "table", Name: "FeatureFlag"}, why: "the kill switches: config data, because \"should this be on right now\" is an operational question with a live answer"},
		{write: "configs/schema/feature_flag.go", why: "name, enabled and a note for the operator who finds it off at 3am"},
		{write: "configs/table/feature_flag.csv", why: "three switches the demo actually reads: the store, respawns, opening activity windows"},
		{write: "game/flags/flags.go", why: "the flag names as constants, and the rule: a switch belongs at an entry point, never inside a transaction"},
		{add: &AddOptions{Kind: "errcode", Name: "ExpAmount", ID: 100004}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "BattleNotFound", ID: 100007}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "BattleNotSeated", ID: 100008}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "BattleBusy", ID: 100009}, why: "a coded failure in the manifest's errcode space"},
		{add: &AddOptions{Kind: "errcode", Name: "DungeonRun", ID: 100010}, why: "a coded failure in the manifest's errcode space"},
		{write: "internal/errors/exp_amount.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/battle_not_found.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/battle_not_seated.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/battle_busy.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/dungeon_run.go", why: "client-facing message instead of the TODO placeholder"},
		{write: "internal/errors/scene_position.go", why: "one code for out of bounds / taken / not on a map: a client that learns which points are occupied has everyone's positions"},
		{write: "internal/errors/dungeon_claim_window.go", why: "a reward refused for being too old has to be a named refusal, not a quiet zero"},
		{write: "internal/errors/player_elsewhere.go", why: "a login that lands on a server the player is not bound to is refused by name, not served from a second copy of their Entity"},
		{write: "internal/errors/login_timeout.go", why: "a login whose claim or cold load outlived its budget is answered by name and retried, not held on the connection"},
		{write: "game/entities/player/equipment_component_test.go", why: "the nested write path both ways: a change two levels down reaches storage, a piece that came off stops reaching it"},
		{write: "game/components/mapview/mapview.go", why: "where the player is and how it walks: movement.MapBasic over the DAO, a Behavior whose MoveTo / Follow run on the player's own heartbeat, and the teleport kept as the reference path"},
		{write: "game/components/mapview/cell_base.go", why: "the partitioned scene's Base side of the Map component: the cell.base beat for the watchdog and the lease, and the battle panel sent when the attributes change"},
		{write: "game/entities/player/map_component.go", why: "wiring only: the component type, its name on Player (Map), the factory registration; the logic is game/components/mapview/mapview.go"},
		{write: "game/view/component_mapview.go", why: "the Map interface other game packages use and its Holder (view.PlayerEntity embeds it)"},
		{run: writeDemoActorBody, why: "the same Body implementation as the actor module; the partitioned-world demo only keeps its own adapters"},
		{write: "game/actor/actor.go", why: "the demo's heartbeat driver, action vocabulary and partitioned-world rules around the shared Body"},
		{write: "game/actor/patrol.go", why: "the monster's AI: walk the corners on its own heartbeat, and never ask whether it is rooted — the state gate does"},
		{write: "game/actor/patrol_test.go", why: "the patrol decides only when the move group is idle and not frozen, and a refused start retries the same corner"},
		{write: "game/entities/player/attribute_component_test.go", why: "the composition rule as a test: Final = Base + Gear with the derived attribute computed once over the composed inputs"},
		{write: "game/clientsync/decode.go", why: "typed DAO Protobuf patches for client and robot views"},
		{write: "game/entities/player/sync_packer.go", why: "the client-facing half of sync=true: who serializes a subject, by profile and by dirty mask"},
		{add: &AddOptions{Kind: "entity", Name: "Scene"}, why: "the map is an Entity: an id, a lifecycle the framework drives, addressable by GM and by another process later"},
		{add: &AddOptions{Kind: "dao", Name: "Scene", Entity: "Scene"}, why: "the scene's replicated state: its time domain, //roost:dao nocoll, all nopersist,sync"},
		{add: &AddOptions{Kind: "entity", Name: "Monster"}, why: "the scene spawns things that are seen but do not see; the pipeline must not have a special case for them"},
		{add: &AddOptions{Kind: "component", Name: "Body", Entity: "Monster"}, why: "where a monster is and how much of it is left"},
		{add: &AddOptions{Kind: "component", Name: "Actor", Entity: "Monster"}, why: "what makes a monster act: movement, AI and the state gate, on its own heartbeat"},
		{add: &AddOptions{Kind: "dao", Name: "Monster", Entity: "Monster"}, why: "a DAO with no collection (//roost:dao nocoll) whose every field is nopersist,sync: nothing to store, everything to replicate"},
		{write: "db/def/monster.go", why: "template, position and hp — //roost:dao nocoll, all nopersist,sync: a position still lives in a DAO, and no Mongo collection is invented for it"},
		{write: "game/entities/monster/entity.go", why: "ephemeral, replicated, rebuilt by the spawner on every start"},
		{write: "game/components/monsterbody/monsterbody.go", why: "the same position rule as the Player: read and written here, nowhere else"},
		{write: "game/entities/monster/body_component.go", why: "wiring only: the component type, its name on Monster (MonsterBody), the factory registration; the logic is game/components/monsterbody/monsterbody.go"},
		{write: "game/view/component_monsterbody.go", why: "the MonsterBody interface other game packages use and its Holder (view.MonsterEntity embeds it)"},
		{write: "game/components/monsteractor/monsteractor.go", why: "MapBasic, Move, Behavior, an AI heartbeat with a patrol, a root buff derived into a state that freezes the move group"},
		{write: "game/components/monsteractor/cell.go", why: "the cell beat a partitioned scene's monster runs instead of its AI, walk, state and buffs"},
		{write: "game/entities/monster/actor_component.go", why: "wiring only: the component type, its name on Monster (MonsterActor), the factory registration; the logic is game/components/monsteractor/monsteractor.go"},
		{write: "game/view/component_monsteractor.go", why: "the MonsterActor interface other game packages use and its Holder (view.MonsterEntity embeds it)"},
		{write: "game/entities/monster/sync_packer.go", why: "the Player's packer with a different DAO"},
		{write: "game/entities/monster/body_component_test.go", why: "the gate for a nocoll DAO: it must replicate, must never register a persistence mutation, and has no storage capability at all"},
		{write: "configs/schema/spawn.go", why: "the scene's population as config data: how many, where, how long before one comes back"},
		{write: "configs/table/spawn.csv", why: "three of one monster near the spawn point, back twenty seconds after dying"},
		{write: "game/scene/scene.go", why: "the contract between the Scene Entity and its systems: the map's shape and its main view (the demo's scale without terrain, the game's scale on 32m blocks with terrain, plan §28), and what a system promises"},
		{write: "game/scene/runtime/runtime.go", why: "the scene runtime as a composition of named systems, started in order and stopped in reverse"},
		{write: "game/scene/runtime/terrain.go", why: "the map: bounds, walkability and occupancy, with its own lock"},
		{write: "game/scene/runtime/pathfind.go", why: "routing and placement: bounded A*, and the nearest free point searched in rings"},
		{write: "game/scene/runtime/runtime_test.go", why: "the map's promises: edges, occupancy, the nearest free point, no route through a wall, and systems that answer after they stop"},
		{write: "game/scene/runtime/refresh.go", why: "the population policy: count what exists rather than what was asked for, so a failed spawn is retried instead of leaked"},
		{write: "game/scene/runtime/refresh_test.go", why: "the population policy: asked once, an unreported spawn asked again, a death waits out the table's delay"},
		{write: "game/entities/scene/entity.go", why: "the map as an Entity: rebuilt from configuration, exporting its systems by interface, deciding its population on its own 1Hz heartbeat and owning its time domain"},
		{write: "db/def/scene.go", why: "the time domain's three numbers, nopersist,sync: a dungeon's 2x dies with the process and the client extrapolates from them"},
		{write: "game/entities/scene/sync_packer.go", why: "the scene's own subject, seen by every member through the scene view"},
		{write: "game/entities/scene/singleton.go", why: "the one map this process runs, built from configuration before anyone can stand on it"},
		{write: "game/settings/settings.go", why: "the config keys the game service's own code reads (activity.*, platform.*, sid), declared like a Mod's and read with app.LoadConfig: the App checks them at start and doctor knows them (maintainer decision)"},
		{write: "game/dungeon/dungeon.go", why: "what a clear is worth and why paying for one exactly once needs a claim ledger, not the request's own flag"},
		{write: "game/dungeon/dungeon_test.go", why: "the claim window as a table test, shipped with the project"},
		{write: "game/battle/battle.go", why: "the lockstep contract: tick rate, frame budget, input encoding, seats and the deterministic simulation both clients run"},
		{add: &AddOptions{Kind: "saga", Name: "GiftItem", Service: gameService, Steps: []string{"debit", "deliver"}, Routed: []string{"debit"}}, why: "the gift saga's definition and step topics; debit (and its refund) is routed to the sender's sid; the saga mod joins the game service"},
		{write: "game/entities/player/saga_receipts.go", why: "where debit and refund keep their receipts: PlayerDao.SagaReceipts behind saga.StepReceipts, so the generated Begin replays a redelivered step instead of debiting twice"},
		{write: "saga/gift_item/steps.go", why: "the generated single-process step registration replaced by a pointer to gift_saga.go: the demo's debit and refund run on the sender's sid and check the binding there"},
		{run: demoGiftRefundBudget, why: "debit's retry budget (saga.steps.gift_item.debit in the game configs) covers a crash restart of the sender's sid, so a refund waits it out instead of going to manual_required"},
		{write: "game/gift/gift.go", why: "the game's side of the gift: state, id, mail text"},
		{write: "game/gift/gift_test.go", why: "complete session identity keeps gifts on distinct logins apart while preserving retries"},
		// 建在模块带来的组件上的 Nest handler 没有 `roost add handler` 骨架步骤，直接写文件：AddItem、赠礼三步、邮件领取、
		// 充值发货、公会的 StartAllianceOp / AllianceDebit / AllianceCredit 锁 Bag 组件（bag 模块），AddExp、ClaimDungeon、PlayerTeam、
		// 公会的 ShowAlliance / PlayerAlliance 锁 Profile 组件（profile 模块）。组件在最后的模块步骤才落盘，而 `roost add handler`
		// 要求组件文件已经在盘上；它只写一个随即被 write 覆盖的骨架，不登记别的东西，所以去掉它不影响生成结果，
		// 这些文件也就不用挪到模块之后——模块那一步跑生成器时，全部 handler 都已经在盘上。
		{write: "game/handler/add_item.go", why: "handler parameters and result; the Sender and endpoint are generated from them"},
		{write: "game/handler/add_exp.go", why: "the transaction that starts the event chain: two entity parameters (Player, World) in one transaction; the Sender becomes MultiSync_AddExp"},
		{add: &AddOptions{Kind: "handler", Name: "RecordEnter", Entity: "World", Component: "Stats"}, why: "World counts a login"},
		{write: "game/handler/record_enter.go", why: "a separate Nest call after the Player one"},
		{add: &AddOptions{Kind: "handler", Name: "RecordMatch", Entity: "World", Component: "Stats"}, why: "World counts a formed match"},
		{write: "game/handler/record_match.go", why: "called by the matchmaker after the remote Commit"},
		{add: &AddOptions{Kind: "handler", Name: "WorldStats", Entity: "World", Component: "Stats"}, why: "a read under the lock"},
		{write: "game/handler/world_stats.go", why: "returns a value, not the Entity"},
		{write: "game/handler/start_gift.go", why: "the saga start: check under the lock, emit the start intent in the same WAL record; no mutation, one effect: saga.EmitStart"},
		{write: "game/handler/gift_debit.go", why: "the saga's debit step as a Nest transaction: Remove plus the command's receipt and completion, all in one WAL record"},
		{write: "game/handler/gift_refund.go", why: "debit's compensation, with the same saga identity: Add plus the receipt and completion; a compensation that runs twice is as wrong as a step that does"},
		{write: "game/handler/claim_dungeon.go", why: "the clear reward, paid at most once per run: the claim ledger and the reward commit together, so a replay pays nothing"},
		{write: "game/handler/add_item_test.go", why: "a grant that would overflow the stored count is refused before the bag is written, through the real handler and Sender"},
		{write: "game/handler/add_exp_test.go", why: "experience and level overflow is refused atomically across Player, World and reward ledger"},
		{write: "game/handler/claim_harness_test.go", why: "the harness the handler tests share: one entity behind a Getter, a committer that keeps the record, the project's own config data, and a business clock a test can pin"},
		{write: "game/handler/claim_dungeon_test.go", why: "the clear reward's ledger and its window asserted together: a pruned run is refused, not paid again"},
		{write: "game/handler/config_snapshot_test.go", why: "the config generation a handler reads is the one pinned at admission: a reload or rollback between two reads of one handler is not seen"},
		{add: &AddOptions{Kind: "access", Name: "player", Service: gameService}, why: "the player request boundary"},
		{add: &AddOptions{Kind: "transport", Name: "tcp"}, why: "a transport a client can actually connect to"},
		{write: "internal/access/player/required_gate_test.go", why: "required ownership gate refuses startup-window requests until Service.Init registers it"},
		{run: enableDemoPlayerTCP, why: "a listener that is actually on; doctor's player-tcp workflow passes on the generated project"},
		{add: &AddOptions{Kind: "protocol", Name: "AddItem", Group: "game", Handler: "player"}, why: "the wire message"},
		{write: "protocol/def/add_item.go", why: "request fields matching the handler parameters; response with code and count"},
		{add: &AddOptions{Kind: "endpoint", Name: "AddItem", Handler: "player"}, why: "protocol boundary to Nest sender"},
		{write: "game/controllers/player/add_item.go", why: "the error boundary: coded errors become the response, not a dropped connection"},
		{add: &AddOptions{Kind: "protocol", Name: "AddExp", Group: "game", Handler: "player"}, why: "the wire message"},
		{write: "protocol/def/add_exp.go", why: "request field matching the handler parameter; response with code and levels gained"},
		{write: "game/controllers/player/add_exp.go", why: "hand-written: add endpoint wires single-entity handlers, AddExp addresses two"},
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
		{write: "game/rewards/rewards_test.go", why: "补丁点的三条：没注册时用原函数、替换立刻生效、revert 回到原样；升级奖励是 mailbox 模块发得出去、领取端解得出来的附件"},
		{write: "game/rewards/rewards.go", why: "what reaching a level is worth, as a mailbox attachment behind a hot-patch point; the attachment encoding, the mailbox endpoints and the claim ledger are the mailbox module's"},
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
		{write: "game/handler/move_player.go", why: "the two-entity teleport, kept as the reference to the server-driven walk; the terrain check stays in the component that owns the write"},
		{write: "game/handler/move_target.go", why: "the server-driven walk: the route found once, a MoveTo or Follow started last, the steps taken on the player's heartbeat"},
		{write: "game/handler/spawn_monster.go", why: "the spawner's fourth step: on the map and acting only after it is visible"},
		{write: "game/handler/root_monster.go", why: "the buff is the only write; the state, the gate and the client's mirror follow in the same transaction"},
		{write: "game/handler/set_scene_time_rate.go", why: "the scene's rate changes in the scene's lock only, continuous, rolled back with the transaction"},
		{add: &AddOptions{Kind: "protocol", Name: "Move", Group: "game", Handler: "player"}, why: "the movement endpoint"},
		{write: "protocol/def/move.go", why: "ask for a point, get back the one the server settled on"},
		{write: "game/controllers/player/move.go", why: "resolve the scene, then the move transaction"},
		{add: &AddOptions{Kind: "protocol", Name: "MoveTarget", Group: "game", Handler: "player"}, why: "the server-driven movement endpoint: the client sends a goal"},
		{write: "protocol/def/move_target.go", why: "a point or a player to follow in, the walk's action id and the start position out"},
		{write: "game/controllers/player/move_target.go", why: "resolve the scene, start the walk, answer with where it starts"},
		{write: "game/activity/activity.go", why: "what this game means by a timed server-wide event: window ids derived from the clock, what a point is worth, what settlement pays, and the board the game keeps because the coordinator has no enumeration"},
		{write: "game/effects/activity_phase.go", why: "the timer's effect: a bus call recorded inside the World's lock and made outside it"},
		{write: "game/entities/world/timer_component.go", why: "the World's timers, hand-written on the framework's entity timer runtime: persisted in WorldDao, rebuilt per call, fired inside the transaction that removes the node, the earliest deadline indexed by the framework's driver (the demo does not use roost add timer); the callbacks are components' methods, reached through the owner"},
		{write: "game/entities/world/timer_component_test.go", why: "the part only a test can show: a deadline armed before a restart still fires after one"},
		{write: "game/components/activityphase/activityphase.go", why: "the business the World's timers carry: arm one close node per activity window (idempotent per id), find it, and on firing record the phase effect inside the World's transaction or ask to be retried"},
		{write: "game/entities/world/activity_phase_component.go", why: "wiring only: the component type, its name on World (ActivityPhase), the factory registration; the logic is game/components/activityphase/activityphase.go"},
		{write: "game/view/component_activityphase.go", why: "the ActivityPhase interface other game packages use and its Holder (view.WorldEntity embeds it)"},
		{add: &AddOptions{Kind: "handler", Name: "TickWorldTimers", Entity: "World", Component: "Timer"}, why: "the driver delivers the tick when the earliest deadline is due; the firing happens under the World's lock"},
		{write: "game/handler/tick_world_timers.go", why: "one call, and what it returns is how many deadlines are still armed"},
		{write: "game/handler/tick_world_timers_test.go", why: "the driver delivers by name: the handler Nest registered must be the one the driver was built for"},
		{add: &AddOptions{Kind: "handler", Name: "ArmActivity", Entity: "World", Component: "ActivityPhase"}, why: "every process arms the same window, so arming is idempotent per activity"},
		{write: "game/handler/arm_activity.go", why: "a second node for one window is a heap that grows by one per restart"},
		{add: &AddOptions{Kind: "handler", Name: "SettleActivity", Entity: "World", Component: "Stats"}, why: "what the coordinator's result PAYS is the game's, recorded once per activity"},
		{write: "game/handler/settle_activity.go", why: "mail first, record second, ack last — each step safe to repeat"},
		{add: &AddOptions{Kind: "handler", Name: "ActivitySettled", Entity: "World", Component: "Stats"}, why: "the standing endpoint reads it through the lock rather than from process memory"},
		{write: "game/handler/activity_settled.go", why: "an in-process flag starts empty after a deploy and tells a paid player they were not paid"},
		{add: &AddOptions{Kind: "protocol", Name: "ActivityStanding", Group: "game", Handler: "player"}, why: "where this player stands in the current window"},
		{write: "protocol/def/activity_standing.go", why: "the coordinator's aggregation and this server's own settlement flag, kept apart"},
		{write: "game/controllers/player/activity_standing.go", why: "two sources, deliberately not merged: complete is not paid"},
		{write: "game/controllers/player/enter_game_test.go", why: "a cold login stuck behind its projection answers login_timeout within the login budget and leaves the shared load running; a login without a server_id claim, or bound to another server, is refused as player_elsewhere before anything loads"},
		{add: &AddOptions{Kind: "entity", Name: "SceneActor"}, why: "a player's part on a partitioned scene's map: the entity the cell's process holds for it, handed over to the next cell's process with it (prototype, scene.partition)"},
		{add: &AddOptions{Kind: "dao", Name: "SceneActor", Entity: "SceneActor"}, why: "its cell state in a generated DAO: the setters record the undo, so a beat or handler that rolls back restores it (maintainer decision D8 = A)"},
		{write: "db/def/scene_actor.go", why: "position, velocity, HP, version, role, lease, write-back time, the damage dedup list and the handoff in progress — //roost:dao nocoll, all nopersist,nosync; the nested types the monster's cell state shares"},
		{write: "game/entities/scene_actor/entity.go", why: "noPersist ephemeral, created only by the partitioned scene's node inside its handler, moved by its own cell.actor beat; a cellworld.Actor over its DAO"},
		{write: "game/partition/partition.go", why: "where this process's cellworld node is installed, and the one mapping between a DAO and the cell / Base state for SceneActor, Monster and Player"},
		{write: "game/partition/partition_test.go", why: "the mapping is the whole state for both DAOs, a read is a copy, a failed transaction restores it through the generated undo, and the stored snapshot position moves only with a write-back"},
		{write: "game/entities/monster/cell.go", why: "a partitioned scene's monster is a walker of its cell (D5 = A): the cellworld.Actor over MonsterDao, starting the cell beat; AI, walk, state and buffs are not started"},
		{write: "game/entities/player/cell_base.go", why: "the player as its partitioned scene's Base (cellworld.BasePlayer): routing state in PlayerDao, the battle panel, starting the cell.base beat, and its copy of its team's roster for the team panel"},
		{write: "game/entities/player/cell_group_test.go", why: "the team copy on the Player: the team id persists, the version and members do not, a read is a copy, a failed transaction restores it"},
		{write: "game/settings/partition_test.go", why: "the partition rules refused at load: owners, the band, the Base's leave radius, the wake radius, the idle tier, spawn points and speeds"},
		{write: "game/settings/partition.go", why: "scene.partition declared once and checked at load: the grid, the owners, the radii the cell protocol needs (R + H <= G, W + H <= G, R covers the Base's leave radius), the idle tier and the per-cell spawn points"},
		{write: "internal/errors/scene_partition.go", why: "what a partitioned scene refuses (Move, MoveTarget, a root) and what an ordinary one does (MoveIntent, AttackTarget) are coded, not internal"},
		{write: "protocol/def/move_intent.go", why: "a velocity in, taken out: the cell integrates it and the position comes with the replicated state (D6 = A)"},
		{write: "game/controllers/player/partition.go", why: "the partitioned scene as an optional capability: its absence is how an endpoint knows the scene is ordinary"},
		{write: "game/controllers/player/move_intent.go", why: "forward the intent to the owning cell through the Player's transaction; wait on no other process"},
		{write: "protocol/def/attack_target.go", why: "a target in, the attack's sequence out; the verdict and the HP come later (D7 = A)"},
		{write: "game/controllers/player/attack_target.go", why: "one fixed-damage attack, applied once by the target's own cell however often it is retried"},
		{write: "protocol/def/attack_result.go", why: "the verdict pushed to the attacker: applied, duplicate, rejected with why, or unknown; the damage and the target's HP after it"},
		{write: "game/handler/team_group.go", why: "the roster as the Base's copy (cellworld.Group): each member's Player, SceneActor and Base sid"},
		{write: "game/handler/team_sync.go", why: "after a restart (the copy's version is 0) and every resync_every, so a lost change notice is caught"},
		{add: &AddOptions{Kind: "entity", Name: "TerrainChunk", ID: 11}, why: "an edited 64m terrain chunk (editable terrain plan P1): only chunks a player changed have one, overlaying the base pack; its id comes from the chunk's coordinates"},
		{write: "db/def/terrain_chunk.go", why: "base revision, version, 4096 heights and 4096 materials (both per-index deltas; schema 2: materials became a uint16 list; schema 3: the bounded applied-edit list is gone, store key 5 retired; schema 4: the lots of the buildings standing on the chunk, persisted, never synced), numeric store keys"},
		{write: "game/entities/terrain_chunk/entity.go", why: "kind 11 at the World rank, sync=true in the terrain namespace; the chunk key <-> entity id mapping (unique = key id + 1)"},
		{write: "game/entities/terrain_chunk/sync_packer.go", why: "one view with every synced field (the applied list is not one): per-index height and material deltas from the change tree, whole fields past the guards, a full snapshot on entering view"},
		{write: "game/entities/terrain_chunk/replica.go", why: "another cell's edited chunk as a read-only replica for this process's clients (partitioned scene, P3): the same id, namespace, kind and sync encoding as the entity, per-index deltas worked out from the changed points, no DAO and no transaction"},
		{write: "game/entities/terrain_chunk/replica_test.go", why: "a delta packed before and a snapshot packed after an update in one capture still bring every subscriber to the latest; the whole field past the guard; versions only move forward"},
		{write: "game/components/terrainsurface/terrainsurface.go", why: "apply point edits inside the chunk's transaction: the first edit fills the chunk from the base data, more than 1000 changed heights (or materials) go out as the whole field, materials are a byte in the terrain and a uint16 in the DAO (out of range refused), every commit bumps the version, every check before the first write; resetting a record built on an old base pack (its building lots kept); the building lots under the chunk's lock"},
		{write: "game/entities/terrain_chunk/surface_component.go", why: "wiring only: the component type, its name on TerrainChunk (TerrainSurface), the factory registration; the logic is game/components/terrainsurface/terrainsurface.go"},
		{write: "game/view/component_terrainsurface.go", why: "the TerrainSurface interface other game packages use and its Holder (view.TerrainChunkEntity embeds it)"},
		{write: "game/components/terrainsurface/lots.go", why: "a building's lot: a node rectangle packed into one int64, the chunks it covers, its nodes in a chunk, and the occupancy bitmap a chunk's lots make, worked out per call"},
		{write: "game/entities/terrain_chunk/surface_component_test.go", why: "the first edit starts from the base data, a record on another base revision is refused, a refused or rolled-back edit leaves the DAO as it was, the whole-field guards end where point writes end and sync the whole field, heights and materials share the change budget, a material outside a byte is refused, a discard resets only a stale record"},
		{add: &AddOptions{Kind: "entity", Name: "TerrainEditor", ID: 12}, why: "a player's terrain edit ledger (T01/T02): an edit is (player id, edit sequence), the ledger keeps the highest applied sequence and a 64-bit window below it, locked with the chunks in the brush transaction"},
		{write: "db/def/terrain_editor.go", why: "the last applied sequence and the 64-bit window as two int64s (all 64 bits kept, nothing truncated) and the player's standing building count (schema 2), persisted, never synced"},
		{write: "game/entities/terrain_editor/entity.go", why: "kind 12 at the PlayerScoped rank (after the chunks); its id is the player's unique id, so only GetOrCreate"},
		{write: "game/components/terrainledger/terrainledger.go", why: "the window rule in one pure value: newer is fresh, inside the window applied is a duplicate and unapplied is fresh, older is stale, far ahead is refused; recorded in the brush transaction"},
		{write: "game/entities/terrain_editor/ledger_component.go", why: "wiring only: the component type, its name on TerrainEditor (TerrainLedger), the factory registration; the logic is game/components/terrainledger/terrainledger.go"},
		{write: "game/view/component_terrainledger.go", why: "the TerrainLedger interface other game packages use and its Holder (view.TerrainEditorEntity embeds it)"},
		{write: "game/components/terrainledger/window_test.go", why: "the window against an unbounded model over random streams (a sequence is never fresh twice), and its edges"},
		{write: "game/entities/terrain_editor/ledger_component_test.go", why: "all 64 bits through the DAO, a rolled-back record undone"},
		{write: "configs/schema/building.go", why: "a prefab building as config data (plan §27): the client's model, the footprint, the placement rules (slope, cut, must level); the footprint of an existing id never changes in a reload"},
		{write: "configs/table/building.csv", why: "four demo buildings: hut, hall, tower, and a keep that must be levelled"},
		{write: "configs/schema/building_param.go", why: "a building's modifiable parameter slots: name, min, max, default, numbered from 0"},
		{write: "configs/table/building_param.csv", why: "the demo buildings' slots (colours, a banner, floors, a lamp); the keep has none"},
		{add: &AddOptions{Kind: "entity", Name: "Building", ID: 13}, why: "a building on the terrain (editable terrain P4, plan §27): a prefab config id plus parameter values, owner, anchor, facing, foundation; its id comes from its anchor node, so removing it clears the record instead of deleting it"},
		{write: "db/def/building.go", why: "state, owner, config id, parameter values, facing, anchor and foundation synced like any entity; lot sides and base revision persisted only (schema 2), numeric store keys"},
		{write: "game/entities/building/entity.go", why: "kind 13 at the World rank (locked with the chunks it covers, before the player's ledger), sync=true in the building namespace; the anchor node <-> entity id mapping, so only GetOrCreate"},
		{write: "game/entities/building/sync_packer.go", why: "one view with every synced field, whole fields by MarshalSync like the player's and the monster's"},
		{write: "game/components/buildingsite/spec.go", why: "the building and building_param tables joined into specs with the cross-row rules checked once; parameters filled with defaults and range-checked; a reload may not drop an id or change its footprint"},
		{write: "game/components/buildingsite/buildingsite.go", why: "a placement (config id, parameters, anchor, facing, level) and the building's state; place, change parameters, clear and rebase inside the transaction, each write handed to the subject"},
		{write: "game/entities/building/site_component.go", why: "wiring only: the component type, its name on Building (BuildingSite), the factory registration; the logic is game/components/buildingsite/buildingsite.go"},
		{write: "game/view/component_buildingsite.go", why: "the BuildingSite interface other game packages use and its Holder (view.BuildingEntity embeds it)"},
		{add: &AddOptions{Kind: "entity", Name: "TerrainCellEditor", ID: 14}, why: "a player's edit ledger on one cell of a partitioned scene (editable terrain P5): a cross-cell edit runs a part on each cell that writes its chunks, and each cell keeps (player id, edit sequence) for its own part in the same transaction as its chunks, so a resent or retried part writes once"},
		{write: "db/def/terrain_cell_editor.go", why: "the same last sequence, 64-bit window and building count as the player's ledger, dbscope=sid so each cell process writes its own database, persisted, never synced"},
		{write: "game/entities/terrain_cell_editor/entity.go", why: "kind 14 at the PlayerScoped rank (after the chunks and the building); its id is the player's unique id, so only GetOrCreate"},
		{write: "game/components/terraincellledger/terraincellledger.go", why: "the player's window rule without the far-ahead refusal (the player's base checked it; a player who edited elsewhere for long arrives far ahead), recorded in the part's transaction"},
		{write: "game/entities/terrain_cell_editor/ledger_component.go", why: "wiring only: the component type, its name on TerrainCellEditor (TerrainCellLedger), the factory registration; the logic is game/components/terraincellledger/terraincellledger.go"},
		{write: "game/view/component_terraincellledger.go", why: "the TerrainCellLedger interface other game packages use and its Holder (view.TerrainCellEditorEntity embeds it)"},
		{write: "game/entities/terrain_cell_editor/ledger_component_test.go", why: "a far-ahead sequence is fresh on a cell, duplicates and the window as on the player's ledger, all fields through the DAO, a rolled-back record undone"},
		{write: "game/settings/terrain.go", why: "scene.terrain.pack declared once (empty = no terrain), and with a partitioned scene the cell side checked to be a multiple of the 6400 chunk side at load"},
		{write: "game/terrainmap/terrainmap.go", why: "the process's terrain: the base pack, the view, the chunk records read from the Data Engine's store by id (edits built on an old base pack discarded), the player's brush (checks, rate, hooks, one transaction over up to four chunks and the player's edit ledger, at most once per player and edit sequence, never under a building), the last sequence for a client that lost count, and the view updated only after a commit"},
		{write: "game/terrainmap/building.go", why: "buildings in the ordinary scene: placement and removal in one transaction over the chunks of the lot, the building and the player's ledger (config and parameters, overlap checked under the chunks' locks, slope, water level, building limit, optional levelled foundation), parameter changes over the building and the ledger, the walkers' occupancy table and the replication notice after the commit, and the startup reconciliation that keeps buildings across a base pack revision and reports config drift"},
		{write: "game/terrainmap/relay.go", why: "a partitioned scene's edits across cells (P5): the player's base checks, charges the quota by the brush's footprint and issues the sequence in the player's ledger, splits the edit by the cells that write its chunks and forwards each part; a cell runs its part in one strict transaction with the player's ledger on that cell; applied, duplicate, refused or unknown per cell, refunds by what was applied, a building's lot on one cell only"},
		{add: &AddOptions{Kind: "handler", Name: "TerrainEdit", Entity: "TerrainChunk", Component: "Surface"}, why: "one chunk's point edits in one strict transaction"},
		{write: "game/handler/terrain_edit.go", why: "the point edit, the player's brush over a target group of chunks plus the player's ledger, the discard of a stale record, and a read of the ledger's last sequence; the view takes the new data after the commit"},
		{add: &AddOptions{Kind: "handler", Name: "Building", Entity: "Building", Component: "Site"}, why: "a building's placement, removal and reconciliation in strict transactions"},
		{write: "game/handler/building.go", why: "place, remove and rebase over the chunks of the lot, the building and (for a player) the edit ledger; parameters over the building and the ledger; the occupancy and the replication notice after the commit"},
		{write: "internal/service/game/terrain.go", why: "open the pack, load this process's cell (plus its ghost band) at Init — records as overlays, the rest from the pack — and publish it; on an ordinary scene take buildings and reconcile them with the chunks' lots; then the edited chunks become replication subjects; gm.terrain.probe reads the view (versions and data digests) for comparing processes"},
		{write: "internal/service/game/terrain_sync.go", why: "the terrain's own interest policy on the scene's manager: players watch, edited chunks are the subjects, the view follows the player's committed moves (and drives the building view); a bigger object limit and the snapshot budget"},
		{write: "internal/service/game/building_sync.go", why: "buildings replicated like any entity (plan §27, §27.10): seen like monsters, through the scene's ShowSubject / HideSubject into its main view (no interest policy, namespace limit or type schedule of their own), shown after a placement commits and hidden after a removal (latest anchor-chunk lot revision wins), on_change deltas for parameter changes, standing buildings registered at start"},
		{write: "internal/service/game/building_config.go", why: "the building config's cross-row rules checked at start and in every reload's validate phase; a reload that drops an id or changes a footprint is refused"},
		{write: "internal/service/game/terrain_partition.go", why: "partitioned scene (P3): a cell's ghost band follows the neighbouring cells' commits, and the chunks other cells write near this process's players reach their clients as replicas; over the cellworld node's ext messages, caught up by renewed subscriptions; partitionSurface is the cells' ground hook, so walks across a cell boundary follow the neighbour's committed terrain"},
		{write: "internal/service/game/terrain_relay.go", why: "partitioned scene (P5): a player's edit parts reach the cells that write their chunks over the cellworld node's ext messages and their answers come back; the base resends under one identity until an answer or a deadline (unknown), a cell runs each identity once at a time, caches its definite answers and drops what it cannot take; one versioned wire format"},
		{write: "internal/service/game/terrain_relay_test.go", why: "two cells and a base-only process over an in-memory net (P5): a cross-cell brush split into one transaction per cell equals the whole-map result, a cell's own part runs locally; a refusing cell keeps the other's commit and a retry with the same sequence finishes it; lost answers end unknown and a retry writes nothing twice; lost requests are run once on the retry; duplicated and replayed messages write once; a cell or base restart answers duplicate from the persisted ledgers; the base's quota keeps unknown parts and refunds what was not applied; a building on one cell, its parameters and removal forwarded, a lot across two cells refused before any write, a cross-cell brush refused under a building on its cell; the ghost band sees the neighbour's buildings and their removal, also after a restart; the wire format"},
		{write: "internal/service/game/terrain_partition_test.go", why: "two cells and a base-only process over an in-memory net: a border edit reaches the neighbour's ghost band and a player's client there per index, loss and reordering heal, either side restarts and catches up, a discarded chunk falls back to the base pack and an old revision's push does not bring it back; npcs integrated by real cellworld nodes on partitionSurface are held by a step on the neighbour's chunk before the handoff, cross once it is levelled and stop at a pit on the new cell"},
		{write: "internal/errors/terrain.go", why: "what a terrain edit refuses is coded: no terrain, a bad brush, another cell's ground, an unavailable chunk, too often, not allowed, a building, a sequence outside the ledger's window; and what a building refuses: a partitioned scene, a bad request, an unknown config, parameters out of their slots, a slope, water, the limit, nothing to remove"},
		{write: "protocol/def/terrain_edit.go", why: "a brush and the player's edit sequence in (sequence 0 asks for the last one); each chunk's new version, whether the edit had already been applied and the last applied sequence out; the change itself comes on the sync stream"},
		{write: "protocol/def/building.go", why: "place (config id, parameters, anchor, facing, levelled foundation), remove (anchor) or change parameters (anchor, parameters) with the player's edit sequence in; the building's state, each chunk's version and the last applied sequence out; other players see buildings on the sync stream"},
		{write: "game/controllers/player/building.go", why: "a placement, removal or parameter change through the terrain map, the refusals to codes (the building's own first, then the terrain's)"},
		{write: "game/controllers/player/terrain_edit.go", why: "wire integers to a brush without truncating, the terrain's refusals to codes"},
		{write: "game/controllers/player/terrain_edit_test.go", why: "the endpoint without terrain answers terrain_disabled, refuses wire values outside their fields, maps every refusal to its code, carries the last sequence with terrain_seq"},
		{write: "game/controllers/player/building_test.go", why: "the building endpoint without terrain answers terrain_disabled, an unknown op answers building_bad, every building refusal maps to its code before the terrain's"},
		{write: "internal/service/game/terrain_test.go", why: "edits read back after the commit and not after a rollback, the same heights and versions after a restart, a record on another base revision discarded by its cell, a record with a material outside a byte not loaded; brushes: duplicates and reordering apply once, an edit replayed after the window or after a restart is stale and writes nothing, edits that shared a truncated id both apply, refusals write nothing, a rolled-back multi-chunk edit changes neither chunks nor view nor ledger; the restart and the replay again on real Mongo when ROOST_TERRAIN_IT_MONGO_URI is set"},
		{write: "internal/service/game/terrain_building_test.go", why: "buildings in the ordinary scene: every refusal (overlap, water, slope, kind, map edge, limit) before any write, a levelled foundation across two chunks, once per player and sequence shared with brushes, removal by the owner only, brushes refused under a building, walkers routed around it, concurrent placements never overlapping, the same buildings after a restart, a base pack revision keeping them (foundations re-levelled, misfits kept and counted), a lot without its building reported, refused without EnableBuildings; the building restart again on real Mongo when ROOST_TERRAIN_IT_MONGO_URI is set"},
		{write: "internal/service/game/building_sync_test.go", why: "over real TCP: another player's client sees a building appear, its parameters change, its removal; it leaves with the main view and comes back; placed again before its remove went out; no replication config of their own; buildings taking main-view places and degrading with the crowd like players and monsters; what the demo-scale main view shows of each building; a restart registers the same buildings; parameter changes once per sequence and by the owner only; config refusals; the measured bytes"},
		{write: "internal/service/game/scene_view_scale_test.go", why: "the terrain scene's main view at the game's scale (plan §28): it fits the observer block budget on its blocks and not on 1m blocks; the player and monster sync bytes; a benchmark of a 1024m map with players, monsters and a town of buildings per block size and view (move batch cost, memory, blocks and objects per observer, the nearest-64 crowding, enter/leave downlink)"},
		{write: "internal/service/game/scene_sync_bytes_test.go", why: "a step's downlink: a player's or a monster's step reaches a watcher as a delta carrying only the position, with a byte bound per frame and per extra moving object in the frame (frame header, object and component headers, the subject envelope without identity, the DAO patch without the entity id)"},
		{write: "internal/service/game/terrain_sync_test.go", why: "over real TCP: a client applying the terrain chunks gets full and delta updates, matches the server point by point after a reconnect, keeps its session with the terrain view at its cap; the measured frame bytes"},
		{write: "internal/service/game/terrain_move_test.go", why: "walks, routes and placements on the height field end to end: held by a slope, routed through a gap and around a ditch dug after the last route, swimming a lake dug across the way, and the 1s terrain window corrected — the stop reaches the client before the chunk, the client agrees once the chunk arrives"},
		{write: "cmd/terrainpack/main.go", why: "write the demo's deterministic terrain base pack: 16 x 16 chunks (1024m, the ordinary scene's limit) of flat ground 1m above the water by default (walkable everywhere, a hole dug a little over 1m deep is water), the rolling procedural terrain with -shape generated; optional regional water levels (pack format 2)"},
		{write: "cmd/terrainpack/main_test.go", why: "the default flat pack (16 x 16 chunks, revision 2, every step walkable, dig a lake and fill it back), the generated shape equal to the old default, explicit overrides, regional water levels and safe CLI ranges"},
		{add: &AddOptions{Kind: "protocol", Name: "Purchase", Group: "game", Handler: "player"}, why: "buying: the demo plays the payment provider, everything around that is real"},
		{write: "protocol/def/purchase.go", why: "a product id in; the order, the receipt's replay flag and the bag count out"},
		{write: "game/handler/purchase_balance.go", why: "a read-only bag snapshot for the demo purchase response; the module owns durable delivery"},
		{write: "game/controllers/player/purchase.go", why: "sign a callback, let the platform service record and deliver it, then drain the grant into the bag"},
		{write: "game/controllers/player/finish_dungeon_test.go", why: "a clear counts once on the board, through the real endpoint, the leaderboard module's submit and the rank store on real Redis: a replay after more than the rank ring's 8 later clears, concurrent replays and a refilled failed submit; what the endpoint hands the rank service on every settlement (the season's clear count as a snapshot: max, no request id, on a replay too, never a zero, and a failed submit does not fail the settlement; no external dependency); and a clear contributes to the activity only from the call that paid it: a replay after the 300-second window rolls scores nothing in the new window, and a contribution that failed at payment is not refilled (no external dependency)"},
		{write: "protocol/def/scene_ready.go", why: "the client says its state decoder is installed; until then its session is held and receives no frames"},
		{write: "game/controllers/player/scene_ready.go", why: "hand the ready signal to the scene bridge"},
		{write: "internal/errors/scene_view.go", why: "an LOD level the scene does not configure and a view changed too often are the client's to fix, not internal errors"},
		{write: "protocol/def/scene_view.go", why: "the client's LOD / view request: a level from the server's table and a radius the server clamps and quantizes"},
		{write: "game/controllers/player/scene_view.go", why: "hand the request to the scene's interest policy like a move: no queue, no commit gate, rate-limited, answered with the view in force"},
		{write: "game/controllers/player/scene_view_test.go", why: "the request reaches the scene as asked and the answer carries the view in force; an unknown level, a request too soon and a player not in the scene are client codes, not internal errors"},
		{add: &AddOptions{Kind: "protocol", Name: "SendGift", Group: "game", Handler: "player"}, why: "the saga start from a client"},
		{write: "protocol/def/send_gift.go", why: "recipient, item, count in; the saga id out"},
		{write: "game/controllers/player/send_gift.go", why: "StartGift through the Nest sender; the saga id is sender + session + frame sequence, and this process's sid is the sender's bound sid the steps are routed by"},
		{write: "game/controllers/player/send_gift_test.go", why: "静态绑定：send_gift 把本进程 sid 作为发送方绑定的 sid 交给 StartGift"},
		{add: &AddOptions{Kind: "protocol", Name: "BattleInput", Group: "game", Handler: "player"}, why: "one client's contribution to one lockstep frame"},
		{write: "protocol/def/battle_input.go", why: "input, keyframe hash and catch-up request on one per-frame message"},
		{write: "game/controllers/player/battle_input.go", why: "hands the message to the battle room's goroutine; no Entity is locked"},
		{add: &AddOptions{Kind: "protocol", Name: "BattleFrame", Group: "game", Handler: "player"}, why: "the server push carrying one lockstep broadcast packet"},
		{write: "protocol/def/battle_frame.go", why: "a notify: the packet the room encoded, redundancy included"},
		{write: "protocol/def/entity_sync.go", why: "the scene's state frames on the wire: one complete frame per push"},
		{add: &AddOptions{Kind: "protocol", Name: "GiftStatus", Group: "game", Handler: "player"}, why: "how the saga went"},
		{write: "protocol/def/gift_status.go", why: "the coordinator's status, step and last error"},
		{write: "game/controllers/player/gift_status.go", why: "Engine.Get through the saga capability; unknown until the start has been consumed"},
		{run: enableDemoMatchSweep, why: "the match process sweeps the duel queue for expired tickets"},
		{write: "loadtest/playertcp/conn.go", why: "the robot transport that speaks the generated server's frame"},
		{write: "loadtest/scenarios/cellworld.yaml", why: "CellWorld robots over real account, TCP, PB and EntitySync"},
		{write: "cmd/loadtest/cellworld.go", why: "bounded CellWorld movement load and request/sync measurements"},
		{write: "loadtest/scenarios/demo.yaml", why: "one robot's life, as a scenario tree"},
		{write: "cmd/loadtest/main.go", why: "robots + thresholds: the load test that is also the regression test"},
		{write: "cmd/loadtest/scene_watch_test.go", why: "scene_watch reads state frames the way the server sends them: raw frame bytes under the RS v2 Sync flag, not a PB message"},
		{write: "deploy/dev/observability/docker-compose.yaml", why: "Prometheus + Grafana for a developer machine, apart from the regenerated infrastructure compose"},
		{write: "deploy/dev/observability/prometheus.yml", why: "scrape the three ops endpoints and the load test"},
		{write: "deploy/dev/observability/grafana/provisioning/datasources/prometheus.yaml", why: "the provisioned datasource"},
		{write: "deploy/dev/observability/grafana/provisioning/dashboards/dashboards.yaml", why: "load dashboards from the mounted directory"},
		{write: "deploy/dev/observability/grafana/dashboards/roost-demo.json", why: "the dashboard, one row per step of the chain"},
		{write: "deploy/dev/observability/README.md", why: "metric ↔ chain step ↔ what to look at"},
		{write: "internal/service/game/service.go", why: "the game service starts the effect consumer in Init and drains it in Shutdown"},
		{write: "internal/service/game/service_shutdown_test.go", why: "Shutdown closes the scene under the App's stop context, so stopping the reload is bounded by shutdown.total_timeout; a fail-stop or graceful Shutdown disconnects every served player first"},
		{write: "internal/service/game/syncbus_config_test.go", why: "the generated syncbus section is the transport the wiring Mod actually starts on, with no unread-config warnings"},
		{write: "internal/service/game/effect_settings.go", why: "activity reads the DataEngine effect settings; profile reward mail is owned by the mailbox module"},
		{write: "internal/service/game/matchmaker.go", why: "Candidates → Grouping → Commit on a ticker, then the World records the match and the players are pushed MatchFound"},
		{write: "internal/service/game/battle_test.go", why: "the room's start grace and lifetime, asserted against the real lockstep room with a recording push lane"},
		{write: "internal/service/game/battle.go", why: "the lockstep rooms: one goroutine owns each room, the player TCP push is its broadcast lane, the matchmaker opens one per match"},
		{write: "internal/service/game/scene_sync_observer.go", why: "bind the Nest commit observer before the scene exists, then attach its mixed-mode manager"},
		{write: "internal/service/game/scene_sync_observer_test.go", why: "verify real producer binding and prevent an unbound or duplicate scene observer"},
		{write: "internal/service/game/scene.go", why: "the other half of sync=true: a room that holds every online player as a subject and pushes their deltas to the others, gated on the Data Engine's durable watermark"},
		{write: "internal/service/game/scene_test.go", why: "the replication claim end to end: one player's DAO change decodes as a delta on another player's wire; under the production wiring (the Nest's commit observer) the profile module's own writes replicate too, and others get the name and level but never the exp"},
		{write: "internal/service/game/scene_connections_test.go", why: "the scene on the real player TCP transport: a stalled old connection is closed without resetting the live client, a player none of whose connections takes a frame leaves; a second live connection keeps the player when the first closes; a disconnect and reconnect inside one tick ends with the new connection replicated"},
		{write: "internal/service/game/gift_saga.go", why: "the gift saga's four steps on the framework's step runtime (kitsaga.Steps): debit / refund as native Nest-transaction steps, deliver as a Mongo step that mails; sid admission and handoff stay here"},
		{write: "game/runtimeid/runtimeid.go", why: "ids for entities the process creates at run time: the sid goes in the id, so two processes cannot mint the same one"},
		{write: "game/runtimeid/runtimeid_test.go", why: "two shards never collide, an unencodable sid fails at startup, exhaustion refuses instead of wrapping"},
		{write: "internal/service/game/spawner.go", why: "the scene's population decisions turned into Entities: create, place, replicate, put on the map, and only then count"},
		{write: "internal/service/game/partition_test.go", why: "the mirrors decode with the demo's own decoder as player and monster, and a zero config is the ordinary scene"},
		{write: "internal/service/game/partition.go", why: "this process's part of the partitioned scene: the cellworld node (Cell when its sid owns a cell, Base for its players), the Sync mirrors the witnesses feed, the cell's monsters"},
		{write: "internal/service/game/heartbeat.go", why: "the heartbeat driver bound to this process's Nest by hand, before any entity that takes a host from it"},
		{write: "internal/service/game/scene_movement_test.go", why: "the movement chain end to end on a real Nest and heartbeat driver: a goal walked to and seen by a watcher, a walk replaced, a patrol, a rooted monster, the population refilled by the scene's beat, a 2x scene"},
		{write: "internal/service/game/spawner_test.go", why: "where the sid comes from: the registry's config, not the runtime-config slot the config-data Mod overwrites"},
		{write: "internal/service/game/gm.go", why: "GM commands on the admin registry: add item / add exp / send mail / world stats, served by ops over HTTP behind a token"},
		{run: enableDemoAdmin, why: "the dev config enables the ops admin endpoint with a dev token, so the GM commands are reachable on a developer machine"},
		{write: "cmd/sidmove/main.go", why: "the checks between the steps of moving a game sid to another machine: the old WAL is fully projected, the singleton lock is free, nobody wrote the players in between"},
		{write: "cmd/sidmove/main_test.go", why: "a held or unacknowledged WAL fails the drain check, the lock holder is reported without its token, a write between two player snapshots is caught"},
		{write: "cmd/sidmerge/main.go", why: "the offline half of merging one server into another: preconditions (drained WALs, co-located shared databases, outbox, terminal sagas, persistent bus list), the copy of dbscope=sid data with a manifest, and its revert (wiring/sidmerge)"},
		{write: "cmd/sidmerge/main_test.go", why: "the merge checklist from this project's own config: the logical databases the DAOs declare, the shared ones that must be co-located, the persistent bus items"},
		{write: "internal/service/game/flags_test.go", why: "启动即表里的值、reload 跟着变、表里删掉的开关消失、缺表时拒绝发布"},
		{write: "internal/service/game/flags.go", why: "the table is the source and the store is rebuilt from it on every reload; also the one place that says what may be hot-patched"},
		{write: "internal/service/game/playerowner.go", why: "serve only the players bound to this sid (server_id from account, and the current binding for background work), a local resident table behind the write gate, the rehome module's Residency with the table of players moved out, and idle unload as plain memory management"},
		{write: "internal/service/game/rehome_checks.go", why: "the demo's own checks before a player may change servers (the rehome module's registration points): the demo partitioned scene refuses rehome; team and alliance are module-owned"},
		{write: "internal/service/game/rehome_checks_test.go", why: "demo 仅验证分区场景换服限制；队伍互斥与联盟路由修正由模块验收"},
		{write: "internal/service/game/playerowner_test.go", why: "Serve 的三种结果、只有通过绑定校验才有驻留记录、卸载进行中不准入、登录等卸载结束、闲置卸载与使用的界定"},
		{write: "internal/service/game/gift_saga_budget_test.go", why: "退款（debit 的补偿）的重试窗口覆盖发送方 sid 的一次崩溃重启：startup_wait + ttl + 拉起余量，按生成的 singleton 配置核对"},
		{write: "internal/service/game/gift_route_test.go", why: "赠礼与公会的玩家步骤按 sid 确切路由：定义只路由写 Player 的步骤；属主核对——本服的离线发送方照常执行，绑定别的服的退回协调器并指明其 sid、不在本进程接入，没有 sid 的拒收，卸载中的在本服重试"},
		{write: "internal/service/game/activity_test.go", why: "结算榜只升不降：两条贡献回复乱序时旧的累计分不能覆盖新的（真实 Redis）；贡献必须说出它落进了哪个窗口：同窗口看得见自己那一点，跨过 300 秒边界是另一个窗口且分数为 0，而那一点仍在原窗口里；expected 集合等于 App 单实例锁 Live 返回的活 sid、为空时只有自己、查询失败不开窗，取不到 Live 能力时 activity 拒绝启动；activity.game_sids 含重复 sid、超出 int32 或超过一次 Live 的上限时按键名拒绝启动"},
		{write: "internal/service/game/gift_recipient_test.go", why: "收件人检查读 Player DAO 自己的库和集合，与 dataengine.database 无关；从未进过游戏的收件人仍是业务拒绝"},
		{write: "internal/service/game/activity.go", why: "the World's timer driver wired by hand (business clock, Nest, start before the World loads), the window loop (expected servers from the App singleton lock's Live query), the phase effect consumer and the settlement: mail → record → ack"},
		{run: demoActivityKeys, why: "the game keeps its contributor board beside the coordinator's keys, and its activity group comes from the groups file the coordinator reads too"},
		{run: demoPaymentSecrets, why: "the platform service refuses to start without its two secrets; the game process signs its simulated callbacks with the same payment secret"},
		{write: "services/session/collaborators.go", why: "a session service whose releaser frees the demo's (resource-less) dungeon"},
	}
	return append(append(steps, demoModuleSteps()...), demoAllianceSteps(gameService)...)
}

// demoModuleSteps 是 demo 经业务模块装进来的功能。排在全部 demo 步骤之后：模块的协议号从
// roost.yaml 的 ids 段分配，要等 demo 里写死号的协议都在盘上之后再分配，才不会撞号。
// 不打开 access.player.self_sync：demo 的场景自己装着 entitysync Manager，一个 Nest 只有一个观察者。
//
// 全部模块在同一步里装（一次 roost module add）。安装结束时要跑生成器与 go mod tidy，那时工程必须是完整的：demo 的文件
// 在前面的步骤里就 import 模块的包（副本结算 import game/leaderboard）、引用模块的组件（BagComp()、ProfileComp()），
// 分成几步装的话，前一步结束时后一步的模块还不在盘上。号按列表里的顺序、每次取最小的空号分配，与分几步无关；
// 所以新换用的模块加在列表**末尾**，前面的模块拿到的号不变。demo 覆盖模块业务文件的 write 都排在这一步之后
// （落点已被占用时安装器拒绝安装）。
func demoModuleSteps() []demoScaffoldStep {
	return []demoScaffoldStep{
		{modules: []string{"enter", "chat", "leaderboard", "bag", "profile", "mailbox", "skillbook", "rehome", "attribute", "equipment", "gm", "purchase", "team", "alliance", "login", "portal", "servertime", "cellworld"}, why: "login is the login module (the handshake authenticator that hands enter:<ticket> and session:<player_id>:<token> to account, the account collaborators, cmd/accountctl); portal is the HTTP front door on the account process (login, server list, enter ticket); servertime is the server clock a client reads; enter is the enter module (the EnterGame protocol, the Player's first-time initialization and its record); chat is the chat module (channels, send, push to online players, history, system announcements, the chat service's collaborators); leaderboard is the leaderboard module (the top page with the caller's own standing, the caller's standing alone, and the snapshot submit the dungeon settlement calls); bag is the bag module (the Bag component, PlayerDao.Bag, the item table — which items exist and each one's stack cap —, the discard protocol, the server-side grant handler and the bag's error codes); profile is the profile module (name, level and experience: the Profile component, PlayerDao.Profile / ProfilePrivate, rename, the read-only check before a grant, the card the ranked queue scores by, and the level-up hook); mailbox is the mailbox module (MailboxList / MailboxClaim, the claim ledger PlayerDao.MailboxLedger, the attachment kinds, the sender SendTo, with enter the pull of server-wide mail on entering and with bag the item attachment) and brings the wallet module it requires (coins, WalletSpend); rehome is the rehome module (moving one player to another game sid: the RehomeApply protocol and the server-side entry the GM command calls, PlayerDao.Rehome, account's begin / commit / abort, and with enter the check before a login and the arrival after it) — the demo publishes its PlayerOwners as the module's Residency and answers saga admission by the current binding; gm supplies audited feature commands, while the demo keeps its Player + World experience transaction"},
		{write: "game/alliance/rules.go", why: "demo alliance currency is gold item 9001"},
		{write: "game/purchase/products.go", why: "the purchase module's project-owned catalogue: demo item packs plus the default coin product"},
		{write: "game/purchase/demo_provider.go", why: "the demo-only provider's channel and deterministic order identity"},
		{write: "services/platform/purchase_channels.go", why: "the demo accepts only its simulated channel after the framework's mandatory HMAC verification"},
		{write: "services/platform/demo_purchase_test.go", why: "the demo channel accepts its signed ingress and rejects other channels"},
		{write: "game/skills/fireball.json", why: "the demo skill definition; catalog startup and endpoint belong to skillbook"},
		// demo 在 enter 模块之上的定制：模块装好之后覆盖它的四个业务文件。协议多几个字段，
		// 两个要装载 Player 的端点交给 player controller——EnterGame（区服归属、登录预算、进场景、补单、公会、公告；
		// 拉全服邮件与聊天上线是进入钩子，由 mailbox、chat 各自与 enter 的联动登记）与 EnterCreate（区服归属、
		// 登录预算、接入驻留表之后才建档）。会话激活后的告知 enter_status.go 是模块的，不覆盖。
		{write: "protocol/def/enter_game.go", why: "the enter module's protocol with the demo's fields: client version in, player id / created / owner sid out; the message id is the one the module was allocated"},
		{write: "game/controllers/enter/controller.go", why: "the enter domain's controller holds the player controller: the demo's login needs everything that controller has"},
		{write: "game/controllers/enter/enter_game.go", why: "the endpoint hands the login to the player controller's EnterGame"},
		{write: "game/controllers/enter/enter_create.go", why: "the create request's endpoint hands over to the player controller's EnterCreate: creating a Player loads it, and the demo loads a Player only where its role is bound, after the ownership table took it into service"},
		{write: "game/controllers/player/enter_create.go", why: "the demo's two record operations behind the enter module's Create: Serve then GetOrCreate on the login budget, and the module's EnterCreate transaction"},
		{write: "game/controllers/player/enter_login_export_test.go", why: "the test-only constructor the external login scenario builds its player controller with"},
		{write: "game/controllers/player/enter_login_scenario_test.go", why: "the whole chain landing on the demo's own login: an enter-ticket handshake against the real account service, EnterStatus says create, EnterGame before the create is refused and writes nothing, EnterCreate, the demo's EnterGame, and a reconnect that is not asked to create again; the session-token path still loads or creates"},
		// 排行榜是 leaderboard 模块：读榜的两条协议（LeaderboardTop 带自己的名次、LeaderboardMe）与提交入口
		// SubmitSeason 都是模块的；demo 只覆盖模块留给工程的业务文件——哪张榜、哪个赛季、分数范围、一屏多长——
		// 与钉住这些数的测试。副本结算（finish_dungeon）在事务之外调模块的提交，它 import game/leaderboard。
		{write: "game/leaderboard/leaderboard_board.go", why: "the module's project-owned board rules with the demo's board: dungeon clears, global, per season, pages of ten; and why a point is the clear count snapshot (UpdateMax) rather than +1 deduplicated on the run id"},
		{write: "game/leaderboard/leaderboard_board_test.go", why: "the demo's board rules pinned with literal numbers, in place of the module's test of its default board"},
		// 背包是 bag 模块：组件（Add / Remove / Count / Has）、PlayerDao.Bag、道具表、丢弃协议、服务端发放 handler、
		// 错误码都来自模块，demo 不带自己的背包。demo 只换数据：道具表多两列（attack / hp，属性系统按它给穿着的装备算加成），
		// 行是 demo 的四种道具。背包的规则文件 game/components/bag/items.go 用模块的缺省实现（读这张表），不覆盖。
		{write: "configs/schema/item.go", why: "the bag module's item table with the demo's two extra columns: what a worn item adds to attack and hp"},
		{write: "configs/table/item.csv", why: "the demo's rows: two potions, a sword, the gold coin; converted to configs/data/item.json by generate"},
		{write: "configs/table/equipment_item.csv", why: "the demo equipment rules; transaction and attribute linkage belong to the modules"},
		// demo 自己的两个文件：充值订单的领取账本（给模块的 BagComponent 加方法；邮件的领取账本是 mailbox 模块的），
		// 以及背包变化经提交观察者同步给本人的断言。
		{write: "internal/service/game/bag_sync_test.go", why: "the module's Bag component never calls PublishSyncDirty: on a Nest built with the scene's commit observer a grant and a discard still reach the owner's client, a refused grant sends nothing, and a bystander never sees the bag"},
		// 档案、属性成长、升级邮件由模块联动；demo 只提供在升级事务里读取的奖励规则。
		{write: "game/components/mailbox/mailbox_profile_rules.go", why: "the mailbox/profile module owns reward effects; the demo chooses its hot-patchable attachment at upgrade time"},
		// 邮件是 mailbox 模块：列表与领取两条协议、领取账本（PlayerDao.MailboxLedger）、附件的种类与编码、
		// 发件入口 SendTo 都是模块的，demo 不带自己的 ListMail / ClaimMail / 领取账本，也不覆盖模块的任何文件。
		// demo 的发件方（升级奖励、GM 补偿、活动结算、送礼的送达）import game/mailbox。它依赖的 wallet 跟着装进来
		// （金币附件经它到账）。enter 与 bag 也在这一步里，所以两组联动一起写：进入游戏时拉全服邮件
		// （画像读会话的 claims）、道具附件进背包（demo 的奖励邮件带的都是道具）。
		//
		// 换服是 rehome 模块：申请协议、驱动器、档里的换服记录、与 enter 的联动（进入前检查、到达）都是模块的，
		// demo 不覆盖它的任何文件。demo 这一侧的接线在自己的文件里：驻留表实现模块的 Residency 并发布
		// （playerowner.go、service.go），saga 步骤按当前绑定准入（gift_saga.go、game/alliance/runtime），
		// 登录在装载玩家之前跑进入前检查（game/controllers/player/enter_game.go），GM 命令 gm.player.rehome，
		// 以及 demo 自己的换服前检查（rehome_checks.go）。
		//
		// 登录是 login 模块：握手的认证器（internal/access/player/tcp/auth.go 与 game/login）、account 的协作者
		// （开发登录、玩家号计数器、名字规则、指标）与 cmd/accountctl 都是模块的，demo 不带自己的认证器与 collaborators。
		// demo 只动模块留给工程的地方：渠道入口 login_channels.go 换成 demo 渠道（机器人、演练脚本与 WALKTHROUGH 用它登录），
		// accountctl 经模块的 registerCommand 多登记四个命令（另一个文件，模块的 main.go 不动）。
		// 登录入口是 portal 模块（挂在托管 account 的进程上），服务器时间是 servertime 模块，demo 都不覆盖。
		{write: "services/account/login_channels.go", why: "the login module's project-owned channel entry with the demo channel: credential demo:<open_id>, which the robots, the drills and the walkthrough log in with; every other channel is refused"},
		{write: "services/account/login_channels_test.go", why: "the demo channel on the real account service: the matching credential logs in (session token and enter ticket both), anything else is identity_denied and leaves no account, and the development channel still answers to login.dev_token only"},
		{write: "internal/access/player/tcp/login_claims_test.go", why: "the login module's authenticator read back by the demo's own reader: the bound sid BoundServerID reads, the broadcast profile of the mailbox enter hook, the bind epoch and a transfer in flight; and what the demo's former authenticator refused is still refused"},
		{write: "cmd/accountctl/operator.go", why: "the demo's operator commands registered beside the login module's own: record or undo a server merge (merge-server / unmerge-server), and the manual fallback for moving one role to another server (show-role, transfer-role begin / commit / abort)"},
		{write: "cmd/accountctl/operator_test.go", why: "transfer-role begin / commit / abort on the real account service over memory stores: begin presents the binding it read, commit records the operator's note, an incomplete command writes nothing; the four commands are reachable through the module's dispatch"},
		{write: "cmd/loadtest/portal_login_test.go", why: "the robot's -login portal credential path against the real front door handlers: three HTTP calls end in an enter ticket the handshake takes, a returning robot learns its player id from the server list, and an unregistered server, a refused login or an unreachable front door yield no handshake frame"},
		{write: "services/account/portal/dev_walkthrough_test.go", why: "the walkthrough's three calls on the project's own dev config: the front door's Start on configs/service/config.account.yaml, the demo channel and the development channel, an enter ticket with this machine's game address, and the handshake that takes it"},
		{run: demoLoginDevConfig, why: "the account service's DEV config gets the development login token and the address of this machine's game server, so the walkthrough's three curl calls end with an enter ticket and where to connect; the production example gets neither"},
	}
}

// demoDevLoginToken 是开发配置里的开发登录令牌（渠道 dev 的凭据）。WALKTHROUGH 与 README 里写的就是这个值。
const demoDevLoginToken = "dev-local"

// demoLoginDevConfig 给托管 account 的进程的**开发配置**补两段，让新生成的 demo 起来之后登录入口直接可用：
//
//	login.dev_token   开发登录（渠道 dev）的令牌。demo 渠道不看配置，这一段是给照着 portal 文档用 dev 渠道的人准备的。
//	portal.servers    本机这台 game 的地址：服号取 game 开发配置的 sid，端口取它的 player_access.tcp.addr。
//	                  不写这一段，清单与进服结果里的 addr 是空的。
//
// 只写 configs/service/config.account.yaml。生产示例与 k8s Secret 示例不动：生产配置里写了开发令牌 account 进程
// 不启动，区服地址是部署的事（Gate 的地址，或者动态目录）。已经有这两段的文件不动。
func demoLoginDevConfig(root, gameService string) error {
	gamePath := filepath.Join(root, "configs", "service", "config."+gameService+".yaml")
	raw, err := os.ReadFile(gamePath)
	if err != nil {
		return err
	}
	var game struct {
		Sid          int32 `yaml:"sid"`
		PlayerAccess struct {
			TCP struct {
				Addr string `yaml:"addr"`
			} `yaml:"tcp"`
		} `yaml:"player_access"`
	}
	if err := yaml.Unmarshal(raw, &game); err != nil {
		return fmt.Errorf("%s: %w", gamePath, err)
	}
	_, port, err := net.SplitHostPort(game.PlayerAccess.TCP.Addr)
	if err != nil || game.Sid <= 0 {
		return fmt.Errorf("%s: expected a positive sid and player_access.tcp.addr as host:port, got sid %d and %q", gamePath, game.Sid, game.PlayerAccess.TCP.Addr)
	}
	accountPath := filepath.Join(root, "configs", "service", "config.account.yaml")
	raw, err = os.ReadFile(accountPath)
	if err != nil {
		return err
	}
	body := string(raw)
	if body != "" && !strings.HasSuffix(body, "\n") {
		body += "\n"
	}
	if !strings.Contains("\n"+body, "\nlogin:\n") {
		body += "# Development login (the login module, services/account/login_dev.go): channel \"dev\" with this\n" +
			"# token as the credential. DEV ONLY: a production config that sets it stops the account process at start.\n" +
			"# The demo channel (login_channels.go, credential demo:<open_id>) does not read it.\n" +
			"login:\n  dev_token: " + demoDevLoginToken + "\n"
	}
	if !strings.Contains("\n"+body, "\nportal:\n") {
		body += "# The front door's server directory (the portal module): server id -> the address a client connects to.\n" +
			"# This machine's game server; a deployment lists its Gate addresses here or plugs in a dynamic directory\n" +
			"# (services/account/portal/portal_rules.go).\n" +
			fmt.Sprintf("portal:\n  servers:\n    \"%d\": 127.0.0.1:%s\n", game.Sid, port)
	}
	if body == string(raw) {
		return nil
	}
	return writeAtomic(accountPath, []byte(body), 0o644)
}

// scaffoldDemoTemplate builds the demo on top of the game template: Player
// gains a DAO and, through the profile and bag modules, a Profile component
// and a Bag component with its item table and coded errors, one Nest write
// transaction adds an item, a TCP endpoint carries it and turns failures into
// coded responses, and the account service gets collaborators that work.
// Everything it writes is application-owned and written once.
func scaffoldDemoTemplate(root string, m Manifest, gameService string) ([]string, error) {
	created, err := scaffoldGameTemplate(root, m, gameService)
	if err != nil {
		return created, err
	}
	vars := demoVars{module: m.Project.Module, gameService: gameService, project: m.Project.Name}
	for _, step := range demoScaffoldSteps(gameService) {
		if len(step.modules) > 0 {
			// deferDependencies：与配方一样，依赖解析留给 project new 的外层（解析或 --skip-deps）。安装器自己的
			// go mod tidy 按已发布的 core 解析，模块用到还没发布的包时（portal 的 wiring/modulehost）会让整个工程生成不出来。
			if _, moduleErr := AddModules(context.Background(), root, step.modules, ModuleOptions{Service: gameService, SkipSelfSync: true, deferDependencies: true}); moduleErr != nil {
				return created, fmt.Errorf("template %s: install modules %s (%s): %w", demoTemplateName, strings.Join(step.modules, ", "), step.why, moduleErr)
			}
			continue
		}
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

// applyInstalledModuleIDs 把模板里的 {{ID:<kind>:<name>}} 换成已装模块记录里的号。
func applyInstalledModuleIDs(root, rel, body string) (string, error) {
	manifest, err := LoadManifest(root)
	if err != nil {
		return "", err
	}
	var missing []string
	body = moduleIDPlaceholder.ReplaceAllStringFunc(body, func(placeholder string) string {
		match := moduleIDPlaceholder.FindStringSubmatch(placeholder)
		for _, installed := range manifest.Modules {
			if id, ok := installed.IDs[match[1]][match[2]]; ok {
				return fmt.Sprintf("%d", id)
			}
		}
		missing = append(missing, placeholder)
		return placeholder
	})
	if len(missing) > 0 {
		return "", fmt.Errorf("template %s: %s refers to %s, but no installed module was allocated that id", demoTemplateName, rel, strings.Join(missing, ", "))
	}
	return body, nil
}
