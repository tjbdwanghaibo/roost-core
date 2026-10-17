package roost

import (
	"context"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/spf13/viper"
	"gopkg.in/yaml.v3"
)

// The embed pattern in demo/embed.go lists top-level directories by name, so a
// new one that nobody adds to it would ship as a silently missing template.
// Compare the embedded set against the directory on disk.
func TestDemoEmbedCoversEveryFile(t *testing.T) {
	embedded, err := demoSourceFiles()
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(embedded)

	root := filepath.Join("..", "..", "..", "demo")
	var onDisk []string
	err = filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(path, ".tmpl") {
			return err
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil {
			return relErr
		}
		onDisk = append(onDisk, strings.TrimSuffix(filepath.ToSlash(rel), ".tmpl"))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(onDisk)
	if strings.Join(embedded, "\n") != strings.Join(onDisk, "\n") {
		t.Fatalf("demo/embed.go does not embed every template.\nembedded:\n%s\non disk:\n%s",
			strings.Join(embedded, "\n"), strings.Join(onDisk, "\n"))
	}
	if len(onDisk) == 0 {
		t.Fatal("no demo templates found; the walk or the layout is wrong")
	}
}

// Every shipped template must be written by a step, and every write step must
// name a shipped template: a template nothing writes is dead weight, and a
// step naming a missing file fails only at generation time.
func TestDemoTemplateStepsAndShippedFilesAgree(t *testing.T) {
	shipped, err := demoSourceFiles()
	if err != nil {
		t.Fatal(err)
	}
	written := map[string]bool{}
	for _, step := range demoScaffoldSteps("game") {
		if step.add != nil || step.run != nil {
			continue
		}
		if step.write == "" {
			t.Fatal("a step neither adds, runs nor writes")
		}
		if written[step.write] {
			t.Errorf("step writes %s twice", step.write)
		}
		written[step.write] = true
	}
	for _, file := range shipped {
		if !written[file] {
			t.Errorf("template %s is shipped but no step writes it", file)
		}
		delete(written, file)
	}
	for file := range written {
		t.Errorf("a step writes %s but no such template is shipped", file)
	}
}

// The demo builds on the game template and adds what its own code needs from
// the manifest, so a caller who narrowed -features still gets a project that
// compiles.
func TestDemoTemplateKeepsTheGameTemplateAndItsFeatures(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, []string{"configdata"}, []string{"config"})
	if err := applyDemoTemplate(&m, "game"); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"account", "activity", "mail", "match", "chat", "global", "platform", "rank", "session"} {
		if m.Services[name].Framework != name {
			t.Errorf("demo dropped hosted service %s: %+v", name, m.Services[name])
		}
	}
	for _, feature := range []string{"protocol", "entity", "nest", "dao", "config"} {
		if !contains(m.Features, feature) {
			t.Errorf("feature %q missing: %v", feature, m.Features)
		}
	}
	if err := m.Validate(); err != nil {
		t.Fatal(err)
	}
}

// The whole template, generated into a real directory: the demo's own files
// land, they are valid Go, and the pieces the generators read out of them —
// the handler's parameters and the request fields that must match — are
// present. Compilation against roost-core is verified by CI, which generates a
// project from this template and builds it.
func TestDemoTemplateGeneratesABuildableWritePath(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target,
		Mods: []string{"configdata", "mongo", "nats", "dataengine", "nest"}, Template: demoTemplateName,
	}); err != nil {
		t.Fatal(err)
	}

	shipped, err := demoSourceFiles()
	if err != nil {
		t.Fatal(err)
	}
	for _, rel := range shipped {
		path := filepath.Join(target, filepath.FromSlash(rel))
		raw, readErr := os.ReadFile(path)
		if readErr != nil {
			t.Fatalf("demo file %s missing: %v", rel, readErr)
		}
		if strings.Contains(string(raw), demoModulePlaceholder) {
			t.Errorf("%s still contains the module placeholder", rel)
		}
		if !strings.HasSuffix(rel, ".go") {
			continue
		}
		if _, parseErr := parser.ParseFile(token.NewFileSet(), path, raw, parser.AllErrors); parseErr != nil {
			t.Errorf("%s does not parse: %v", rel, parseErr)
		}
	}

	read := func(rel string) string {
		t.Helper()
		raw, readErr := os.ReadFile(filepath.Join(target, filepath.FromSlash(rel)))
		if readErr != nil {
			t.Fatal(readErr)
		}
		return string(raw)
	}
	// The endpoint generator matches handler parameters against request
	// fields, so these two must stay in step; they are the demo's contract.
	if handler := read("game/handler/add_item.go"); !strings.Contains(handler, "itemID int64, count int32") {
		t.Errorf("handler lost its parameters:\n%s", handler)
	}
	if request := read("protocol/def/add_item.go"); !strings.Contains(request, "ItemID int64") || !strings.Contains(request, "Count  int32") {
		t.Errorf("request fields do not match the handler parameters:\n%s", request)
	}
	// The generated endpoint is what proves the two were wired together.
	if endpoint := read("game/controllers/player/add_item.go"); !strings.Contains(endpoint, "request.ItemID") || !strings.Contains(endpoint, "request.Count") {
		t.Errorf("endpoint does not pass the request through:\n%s", endpoint)
	}
	// The demo replaces the fail-closed skeleton with a session-ticket
	// authenticator and ships no debug credential: a token the server does
	// not verify is not a credential.
	if auth := read("internal/access/player/tcp/auth.go"); !strings.Contains(auth, sessionTokenPrefixName) || strings.Contains(auth, `"player:"`) {
		t.Errorf("auth.go is not the session-ticket authenticator, or still carries the debug shortcut:\n%s", auth)
	}
	// Persistent state must go through generated mutators, not raw fields,
	// and validation reads the item table through the generated accessor.
	if bag := read("game/entities/player/bag_component.go"); !strings.Contains(bag, "dao.SetItems(") || !strings.Contains(bag, "generated.ItemByID(") {
		t.Errorf("bag component does not use the generated mutator and table accessor:\n%s", bag)
	}
	// The demo endpoint replaces the generated scaffold with the error
	// boundary; a handler error that reached the access layer would close the
	// connection.
	if endpoint := read("game/controllers/player/add_item.go"); !strings.Contains(endpoint, "errcode.ClientError(err)") {
		t.Errorf("endpoint does not translate errors at the boundary:\n%s", endpoint)
	}
	// The table exists in all three forms the generator maintains: schema,
	// rows, and the JSON the runtime loads — the last proves the final
	// Generate ran the CSV conversion on the demo's rows.
	if schema := read("configs/schema/item.go"); !strings.Contains(schema, "//roost:table name=item key=ID") {
		t.Errorf("item schema lost its marker:\n%s", schema)
	}
	if data := read("configs/data/item.json"); !strings.Contains(data, "\"id\": 1001") {
		t.Errorf("item.json was not converted from the demo rows:\n%s", data)
	}
	// Coded errors sit in the manifest's errcode space, so `roost id check`
	// owns their uniqueness; an id outside it would fail at add time.
	for _, file := range []string{"internal/errors/item_unknown.go", "internal/errors/item_count.go", "internal/errors/bag_full.go"} {
		if body := read(file); !strings.Contains(body, "errcode.Define(1000") || strings.Contains(body, "TODO") {
			t.Errorf("%s is not a finished coded error:\n%s", file, body)
		}
	}
	// The account collaborators must be the working ones, bound to Redis
	// through the kit hook; the game template's defaults refuse every login.
	// Every hosted service's collaborators are implemented: doctor judges by
	// the stub marker, so the demo's own texts must not contain it either
	// (account's verifier once said "channel %q is not configured" and read as
	// a stub).
	for _, service := range []string{"account", "activity", "chat", "global", "mail", "match", "platform", "rank", "session"} {
		if collaborators := read("internal/service/" + service + "/collaborators.go"); strings.Contains(collaborators, collaboratorUnconfiguredMarker) {
			t.Errorf("%s collaborators still read as unconfigured to doctor:\n%s", service, collaborators)
		}
	}
	if chatCollaborators := read("internal/service/chat/collaborators.go"); !strings.Contains(chatCollaborators, "chat.GrantSystem()") || !strings.Contains(chatCollaborators, "chatroom.TypeText") {
		t.Errorf("chat collaborators do not grant the system path and register the text type:\n%s", chatCollaborators)
	}
	for _, rel := range []string{"game/chatroom/chatroom.go", "protocol/def/send_chat.go", "protocol/def/chat_history.go", "protocol/def/chat_message.go",
		"game/controllers/player/send_chat.go", "game/controllers/player/chat_history.go", "game/controllers/player/chat_push.go", "deploy/dev/run.sh",
		"game/rewards/rewards.go", "protocol/def/list_mail.go", "protocol/def/claim_mail.go",
		"game/controllers/player/list_mail.go", "game/controllers/player/claim_mail.go",
		"internal/service/session/collaborators.go", "protocol/def/enter_dungeon.go", "protocol/def/finish_dungeon.go", "game/handler/player_level.go",
		"game/controllers/player/enter_dungeon.go", "game/controllers/player/finish_dungeon.go",
		"game/skills/fireball.json", "game/skills/catalog.go", "protocol/def/skill_catalog.go", "game/controllers/player/skill_catalog.go",
		"internal/service/game/gm.go",
		"saga/gift_item/definition.go", "game/gift/gift.go", "game/handler/start_gift.go", "game/handler/gift_debit.go",
		"protocol/def/send_gift.go", "protocol/def/gift_status.go", "game/controllers/player/send_gift.go", "game/controllers/player/gift_status.go",
		"internal/service/game/gift_saga.go", "internal/errors/item_short.go",
		"game/battle/battle.go", "protocol/def/battle_input.go", "protocol/def/battle_frame.go",
		"game/controllers/player/battle_input.go", "internal/service/game/battle.go",
		"game/dungeon/dungeon.go", "game/dungeon/dungeon_test.go", "game/handler/claim_dungeon.go", "internal/errors/dungeon_run.go",
		"internal/service/game/battle_test.go", "game/handler/claim_mail_reward.go",
		"game/handler/claim_mail_reward_test.go", "game/handler/claim_mail_expiry_test.go", "internal/errors/mail_claim.go"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(rel))); err != nil {
			t.Errorf("demo did not write %s: %v", rel, err)
		}
	}
	if mailer := read("internal/service/game/level_up_mail.go"); !strings.Contains(mailer, "rewards.Encode(rewards.LevelUpReward(") {
		t.Errorf("the level-up mail carries no reward attachment, so ClaimMail has nothing to claim")
	}
	// The grant in the middle carries its own identity: the mail id is
	// recorded on the Player in the same transaction as the items, so a
	// retry after a lost CommitClaim cannot grant a second stack.
	if claim := read("game/controllers/player/claim_mail.go"); !strings.Contains(claim, ".ReserveClaim(") || !strings.Contains(claim, ".Sync_ClaimMailReward(") || !strings.Contains(claim, ".CommitClaim(") {
		t.Errorf("claim_mail does not run the reserve → ledgered grant → commit sequence")
	}
	if claim := read("game/handler/claim_mail_reward.go"); !strings.Contains(claim, "ClaimMailReward(mailID, expiresAtUnix, nowUnix)") || !strings.Contains(claim, "rollback=undo durability=strict") {
		t.Errorf("the mail claim handler does not record the mail id in the granting transaction")
	}
	// The ledger's memory is bounded by the MAIL's own expiry, which the mail
	// service hands over with the reservation — not by a retention this game
	// invented against another service's configuration (RR-20260918-05).
	if claim := read("game/controllers/player/claim_mail.go"); !strings.Contains(claim, "claim.ExpiresAtUnix") {
		t.Errorf("claim_mail does not pass the mail's authoritative expiry into the granting transaction")
	}
	// Runtime entity ids carry the process's sid, so two game processes
	// cannot mint the same one (RR-20260918-09).
	if spawn := read("internal/service/game/spawner.go"); !strings.Contains(spawn, "runtimeid.New(") {
		t.Errorf("the spawner mints monster ids without the process's sid; two instances would collide")
	}
	// A connection that closes says so, instead of being noticed by a push
	// that fails (RR-20260918-06).
	if scene := read("internal/service/game/scene.go"); !strings.Contains(scene, "OnSessionClosed(") {
		t.Errorf("the scene does not subscribe to session closes, so an idle world keeps offline members")
	}
	if dao := read("db/def/player.go"); !strings.Contains(dao, "MailClaims map[string]int64") {
		t.Errorf("the Player DAO has no ledger for mail attachment claims")
	}
	if conn := read("loadtest/playertcp/conn.go"); !strings.Contains(conn, "packet.Flags&wire.FlagPush == 0 && packet.Seq != 0") {
		t.Errorf("the robot transport does not classify frames by the server-push flag; a push carrying a pending wire sequence would be taken for the response")
	}
	// Ten processes on one machine: each config has its own ops port, in
	// the order run.sh and prometheus.yml assume.
	for service, port := range map[string]string{"game": "9100", "account": "9101", "activity": "9102", "chat": "9103", "global": "9104", "mail": "9105", "match": "9106", "platform": "9107", "rank": "9108", "session": "9109"} {
		if cfg := read("configs/service/config." + service + ".yaml"); !strings.Contains(cfg, "addr: 127.0.0.1:"+port) {
			t.Errorf("config.%s.yaml does not listen ops on %s", service, port)
		}
	}
	// The GM surface: admin on in the dev config with a dev token, off in the
	// production example (which config check --production would refuse anyway).
	if cfg := read("configs/service/config.game.yaml"); !strings.Contains(cfg, "admin_enabled: true") || !strings.Contains(cfg, "admin_token: dev-gm-token") || !strings.Contains(cfg, "allow_dev_token: true") {
		t.Errorf("dev config does not enable the ops admin endpoint for the GM commands")
	}
	if cfg := read("configs/service/config.game.prod.example.yaml"); !strings.Contains(cfg, "admin_enabled: false") {
		t.Errorf("production example config enables admin")
	}
	// The dungeon clear is paid on the authoritative run state, once per run:
	// judging on the request's own Success flag paid for failed runs, expired
	// runs and every replay (RR-20260917-08).
	if endpoint := read("game/controllers/player/finish_dungeon.go"); !strings.Contains(endpoint, "run.State != svcsession.StateSucceeded") || !strings.Contains(endpoint, "MultiSync_ClaimDungeon(") || strings.Contains(endpoint, "if !request.Success {") {
		t.Errorf("finish_dungeon still decides the reward from the request instead of the run the service returned")
	}
	if claim := read("game/handler/claim_dungeon.go"); !strings.Contains(claim, "ClaimDungeonRun(runID, resolvedAtUnix, nowUnix)") || !strings.Contains(claim, "rollback=undo durability=strict") {
		t.Errorf("the claim handler does not record the run id in the rewarding transaction")
	}
	// The ledger is bounded, so forgetting a record and refusing a claim have
	// to be the same condition — otherwise a pruned run is still payable and
	// is paid twice (RR-20260918-04). The check is in the transaction that
	// pays, against a time the session service minted, not time.Now().
	if claim := read("game/handler/claim_dungeon.go"); !strings.Contains(claim, "dungeon.ClaimWindowClosed(resolvedAtUnix, nowUnix)") {
		t.Errorf("the claim handler pays a run without checking whether its reward window is still open")
	}
	if endpoint := read("game/controllers/player/finish_dungeon.go"); !strings.Contains(endpoint, "resolvedAt := run.FinishedAtUnix") {
		t.Errorf("finish_dungeon anchors the reward window on something other than the run's own resolution time")
	}
	if dao := read("db/def/player.go"); !strings.Contains(dao, "DungeonClaims map[string]int64") {
		t.Errorf("the Player DAO has no claim ledger for dungeon rewards")
	}
	if scenario := read("loadtest/scenarios/demo.yaml"); !strings.Contains(scenario, "finish_dungeon_replay") {
		t.Errorf("the robot scenario never replays a finished dungeon, so a double reward would pass unnoticed")
	}
	// The one nested DAO field: a struct inside the DAO holding a map of
	// pointers to another struct. Two P1 defects lived in exactly that path
	// and no generated project could see them, because nothing in the demo
	// had one (U-0236, U-0238, U-0245).
	if def := read("db/def/player.go"); !strings.Contains(def, "Equipment Equipment") || !strings.Contains(def, "Slots map[int32]*GearPiece") {
		t.Errorf("the Player DAO has no nested struct field, so the demo never exercises two-level dirty propagation")
	}
	// 持久 schema 继续声明，用于加载前的严格版本校验。
	if def := read("db/def/player.go"); !strings.Contains(def, "schema=2") {
		t.Errorf("the Player DAO does not declare a schema version, so stored data cannot be checked")
	}
	// Entity sync: the Player is a replicated subject, the scene is the room
	// that schedules and fans out its deltas, and the client half decodes
	// them. A demo without this never exercises server-authoritative
	// replication at all — which is how a generated sync=true entity could
	// stop compiling against Core without anything noticing (RR-20260918-01).
	if marker := read("game/entities/player/entity.go"); !strings.Contains(marker, "sync=true") || !strings.Contains(marker, "subjectPacker=NewPlayerSyncPacker") {
		t.Errorf("the demo Player is not a replicated subject, so sync=true has no consumer in the template")
	}
	if scene := read("internal/service/game/scene.go"); !strings.Contains(scene, "entitysync.NewManager(") || !strings.Contains(scene, "DurableWatermark") || !strings.Contains(scene, "manager.Register(") {
		t.Errorf("the scene does not assemble the sync manager, its subjects and the durability gate")
	}
	if packer := read("game/entities/player/sync_packer.go"); !strings.Contains(packer, "PackSubjectDelta") || !strings.Contains(packer, "MarshalSync(mask)") {
		t.Errorf("the player packer does not turn the DAO's dirty mask into a delta")
	}
	if scenario := read("loadtest/scenarios/demo.yaml"); !strings.Contains(scenario, "scene_watch") || !strings.Contains(scenario, "scene_expect") {
		t.Errorf("the robot scenario never observes replicated state, so a broken sync path would pass unnoticed")
	}
	// The lockstep battle: the room is opened by the matchmaker, owned by one
	// goroutine, and its broadcast lane is the player TCP push. The robot
	// plays it with roost-core/robot's LockstepBot, so the scenario must
	// reach the battle after a match and the load test must register the
	// per-frame message's codec entries (it is sent by an action, not by a
	// registered call).
	if manager := read("internal/service/game/battle.go"); !strings.Contains(manager, "lockstep.NewMatch(") || !strings.Contains(manager, "room.Tick(ctx)") || !strings.Contains(manager, "battleDrainWindow") {
		t.Errorf("battle.go does not open a lockstep room, drive it or keep a drain window for the last keyframe's hash reports")
	}
	// The start grace is its own timer: waiting for a first command instead
	// meant a room nobody typed in never cut a frame (RR-20260917-09).
	if manager := read("internal/service/game/battle.go"); !strings.Contains(manager, "grace := time.NewTimer(battleStartGrace)") || !strings.Contains(manager, "case <-grace.C:") {
		t.Errorf("the battle room has no independent start-grace timer, so its declared start-anyway semantics do not hold")
	}
	if matchmaker := read("internal/service/game/matchmaker.go"); !strings.Contains(matchmaker, "battles.Open(match.ID, members)") {
		t.Errorf("the matchmaker does not open a battle for a formed match")
	}
	if loadtest := read("cmd/loadtest/main.go"); !strings.Contains(loadtest, "robot.NewLockstepBot(") || !strings.Contains(loadtest, "MustRegisterEncoder(msgid.MsgBattleInput") {
		t.Errorf("the load test does not run a lockstep bot with the battle message's codec entries")
	}
	if scenario := read("loadtest/scenarios/demo.yaml"); !strings.Contains(scenario, "action: battle") {
		t.Errorf("the robot scenario never plays a battle")
	}
	// The gift saga: the saga mod on the game service with its config section
	// (added after the configs were rendered — the generic add-mod gap the
	// live run found), the definition registered in the manifest, both robot
	// paths in the scenario, and the step consumers using the non-deprecated
	// Mongo step subscription.
	if manifest := read("roost.yaml"); !strings.Contains(manifest, "- gift_item") || !strings.Contains(manifest, "- saga") {
		t.Errorf("manifest does not carry the gift_item saga and the saga mod:\n%s", manifest)
	}
	if cfg := read("configs/service/config.game.yaml"); !strings.Contains(cfg, "\nsaga:\n") {
		t.Errorf("game config has no saga section although the demo put the saga mod on it")
	}
	if scenario := read("loadtest/scenarios/demo.yaml"); !strings.Contains(scenario, "send_gift_self") || !strings.Contains(scenario, "status: compensated") || !strings.Contains(scenario, "to_player_id: 1") {
		t.Errorf("the robot scenario does not exercise both gift saga paths")
	}
	if definition := read("saga/gift_item/definition.go"); !strings.Contains(definition, "saga.SubscribeMongoStep(") || strings.Contains(definition, "saga.SubscribeStep(") {
		t.Errorf("generated saga definition must subscribe through SubscribeMongoStep, not the removed SubscribeStep")
	}
	// Two step shapes, one per kind of work: the Nest-transaction steps bind
	// their receipt and emit their completion inside the transaction (the
	// native path), the mail step keeps the Mongo inbox and the mail
	// service's own RequestID dedupe.
	if steps := read("internal/service/game/gift_saga.go"); !strings.Contains(steps, "saga.SubscribeDataEngineStep(") || !strings.Contains(steps, "saga.SubscribeMongoStep(") || !strings.Contains(steps, "RequestID: \"gift:\" + command.IdempotencyKey") {
		t.Errorf("gift saga does not run the Nest steps natively while keeping the mail step on the Mongo inbox")
	}
	if steps := read("internal/service/game/gift_saga.go"); !strings.Contains(steps, "giftitem.TopicDebit") || !strings.Contains(steps, "saga.NewDataEngineStepInbox(") {
		t.Errorf("gift saga does not address the generated topics or build the native inbox")
	}
	if debit := read("game/handler/gift_debit.go"); !strings.Contains(debit, "step.Complete(true, \"\")") || !strings.Contains(debit, "step.Complete(false, reason)") {
		t.Errorf("the debit handler does not commit its receipt and outcome in the transaction")
	}
	if contract := read("game/gift/gift.go"); !strings.Contains(contract, "func (step NativeStep) Complete(") || !strings.Contains(contract, "saga.EmitCompletion(") {
		t.Errorf("the gift contract has no native step carrier")
	}
	// A step's topics come from the generated definition, so a durable and
	// its filter cannot drift apart.
	if definition := read("saga/gift_item/definition.go"); !strings.Contains(definition, "TopicDebit = ") || !strings.Contains(definition, "TopicDebitCompensation = ") {
		t.Errorf("the generated saga definition exposes no topic constants, so the native path has to repeat the strings")
	}
	// player_id takes either id an operator has at hand: the unique id the
	// client sees or the full entity id Mongo stores as _id. Re-wrapping a
	// full id produced an id no Player has (live run, 2026-09-17).
	if gm := read("internal/service/game/gm.go"); !strings.Contains(gm, "entity.MatchEntityID(raw, player.EntityKindPlayer)") || !strings.Contains(gm, "entity.GetUniqueIDFromEntityID(raw)") {
		t.Errorf("gm.go does not accept the full Player entity id as player_id")
	}
	if scrape := read("deploy/dev/observability/prometheus.yml"); !strings.Contains(scrape, "job_name: chat") || !strings.Contains(scrape, ":9102") {
		t.Errorf("prometheus.yml does not scrape the chat process on its assigned port")
	}
	if collaborators := read("internal/service/account/collaborators.go"); !strings.Contains(collaborators, "account.RegistryBound") || strings.Contains(collaborators, "is not configured; implement") {
		t.Errorf("account collaborators are still the refusing defaults:\n%s", collaborators)
	}
	// The authenticator validates session tickets through the account client
	// it is bound to in Provide; the generated transport must expose the hook.
	if auth := read("internal/access/player/tcp/auth.go"); !strings.Contains(auth, "ValidateSession(") || !strings.Contains(auth, "BindRegistry(") {
		t.Errorf("auth.go does not validate sessions through a bound account client:\n%s", auth)
	}
	if server := read("internal/access/player/tcp/server_gen.go"); !strings.Contains(server, "type RegistryBound interface") {
		t.Errorf("the generated transport lost the RegistryBound hook the demo authenticator relies on")
	}
	// The event chain: the component emits on the transaction, the service
	// consumes with an inbox and sends mail keyed by the effect id.
	if profile := read("game/entities/player/profile_component.go"); !strings.Contains(profile, "effects.EmitPlayerLevelUp(") {
		t.Errorf("profile component does not emit the level-up effect:\n%s", profile)
	}
	if service := read("internal/service/game/service.go"); !strings.Contains(service, "startLevelUpMailer(") {
		t.Errorf("game service does not start the effect consumer:\n%s", service)
	}
	if mailer := read("internal/service/game/level_up_mail.go"); !strings.Contains(mailer, "nestwal.SubscribeJetStreamEffects(") || !strings.Contains(mailer, "RequestID:        envelope.EffectID") {
		t.Errorf("level-up mailer is not the inbox-backed, idempotent consumer:\n%s", mailer)
	}
	if endpoint := read("game/controllers/player/add_exp.go"); !strings.Contains(endpoint, "errcode.ClientError(err)") {
		t.Errorf("add_exp endpoint does not translate errors at the boundary:\n%s", endpoint)
	}
	// The load test: the controller can create Players, the EnterGame message
	// is bound, the transport adapter and the scenario ship, and the command
	// checks every response code so a coded failure fails the run.
	if controller := read("game/controllers/player/controller.go"); !strings.Contains(controller, "lifecycle.PlayerFromRegistry(") {
		t.Errorf("controller does not hold the Player lifecycle:\n%s", controller)
	}
	if guild := read("game/controllers/player/guild.go"); !strings.Contains(guild, "controller.nextGuildID(") || strings.Contains(guild, "controller.RuntimeIDs(") {
		t.Fatal("persistent guild creation must use the durable allocator")
	}
	read("game/controllers/player/guild_ids.go")
	read("game/controllers/player/guild_ids_test.go")
	if bind := read("game/protocol_handlers/player/protocol_gen.go"); !strings.Contains(bind, "HandleEnterGame") {
		t.Errorf("EnterGame is not bound to the controller:\n%s", bind)
	}
	if conn := read("loadtest/playertcp/conn.go"); !strings.Contains(conn, "transport.RegisterDialer(") {
		t.Errorf("playertcp does not register a robot dialer:\n%s", conn)
	}
	if spec := read("loadtest/scenarios/demo.yaml"); !strings.Contains(spec, "action: enter_game") || !strings.Contains(spec, "action: add_exp") {
		t.Errorf("demo scenario lost a step:\n%s", spec)
	}
	if command := read("cmd/loadtest/main.go"); !strings.Contains(command, "loadtest.Threshold{") || !strings.Contains(command, "coded(resp.Code, resp.Reason)") {
		t.Errorf("loadtest command lacks thresholds or response-code checks:\n%s", command)
	}
	// Cross-service matchmaking and the World's job: the matchmaker calls the
	// typed match client and applies a Grouping; the World's counters move
	// through Nest handlers; the match process sweeps the demo queue.
	if matchmaker := read("internal/service/game/matchmaker.go"); !strings.Contains(matchmaker, "matcher.Commit(") || !strings.Contains(matchmaker, "grouping.Group(") || !strings.Contains(matchmaker, "Sync_RecordMatch(") {
		t.Errorf("matchmaker does not drive the match service and record into the World:\n%s", matchmaker)
	}
	if stats := read("game/handler/world_stats.go"); !strings.Contains(stats, "(world.Stats, error)") {
		t.Errorf("world_stats handler does not return the counters as a value:\n%s", stats)
	}
	if config := read("configs/service/config.match.yaml"); !strings.Contains(config, "- duel:2:default") {
		t.Errorf("match config does not sweep the duel queue:\n%s", config)
	}
	for _, name := range []string{"HandleJoinQueue", "HandlePollMatch", "HandleWorldStats"} {
		if bind := read("game/protocol_handlers/player/protocol_gen.go"); !strings.Contains(bind, name) {
			t.Errorf("%s is not bound to the controller", name)
		}
	}
	if spec := read("loadtest/scenarios/demo.yaml"); !strings.Contains(spec, "action: join_queue") || !strings.Contains(spec, "action: poll_match") || !strings.Contains(spec, "action: world_stats") {
		t.Errorf("demo scenario lacks the matchmaking steps:\n%s", spec)
	}
	// Push, real login, server registration: the matchmaker pushes MatchFound
	// through the transport Runtime and the bind registers the push encoder;
	// the load test can log in through the account service; the game
	// announces its server id to account.
	if matchmaker := read("internal/service/game/matchmaker.go"); !strings.Contains(matchmaker, "transport.PushPlayer(") || !strings.Contains(matchmaker, "msgid.MsgMatchFound") {
		t.Errorf("matchmaker does not push MatchFound:\n%s", matchmaker)
	}
	if bootstrap := read("game/protocol_bootstrap/protocol_gen.go"); !strings.Contains(bootstrap, "RegisterMatchFoundEncoder(") {
		t.Errorf("the push encoder is not registered by the generated bootstrap:\n%s", bootstrap)
	}
	if spec := read("loadtest/scenarios/demo.yaml"); !strings.Contains(spec, "action: wait_push") || !strings.Contains(spec, "msg: 10100") {
		t.Errorf("scenario does not wait for the MatchFound push:\n%s", spec)
	}
	if command := read("cmd/loadtest/main.go"); !strings.Contains(command, "svcaccount.NewBusClient(") || !strings.Contains(command, `"session:%d:%s"`) || !strings.Contains(command, ".SelectRole(") || strings.Contains(command, `"player:"`) {
		t.Errorf("loadtest does not log in through the account service only:\n%s", command)
	}
	// Lock ranks and the two-entity transaction: both kinds carry their
	// category on the marker, and the generated Sender for AddExp takes one
	// id per entity parameter.
	if entityFile := read("game/entities/player/entity.go"); !strings.Contains(entityFile, "category=entity.EntityCategoryPlayer") {
		t.Errorf("Player is not in EntityCategoryPlayer:\n%s", entityFile)
	}
	if entityFile := read("game/entities/world/entity.go"); !strings.Contains(entityFile, "category=entity.EntityCategoryWorld") {
		t.Errorf("World is not in EntityCategoryWorld:\n%s", entityFile)
	}
	if sender := read("game/handler/syncsender/add_exp_nest_gen.go"); !strings.Contains(sender, "MultiSync_AddExp(ctx context.Context, target int64, stats int64, amount int64)") {
		t.Errorf("AddExp Sender is not the two-entity form:\n%s", sender)
	}
	if operator := read("cmd/accountctl/main.go"); !strings.Contains(operator, ".UpsertServer(") || !strings.Contains(operator, `"roost:planet:account"`) {
		t.Errorf("accountctl does not register a server on the project's account store:\n%s", operator)
	}
	// Observability: the dashboard is valid JSON with panels that all query
	// the provisioned datasource, and the scrape config covers the three
	// processes and the load test.
	var dashboard struct {
		UID    string `json:"uid"`
		Panels []struct {
			Type    string `json:"type"`
			Targets []struct {
				Expr string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal([]byte(read("deploy/dev/observability/grafana/dashboards/roost-demo.json")), &dashboard); err != nil {
		t.Fatalf("dashboard is not valid JSON: %v", err)
	}
	queries := 0
	for _, panel := range dashboard.Panels {
		if panel.Type == "row" {
			continue
		}
		if len(panel.Targets) == 0 {
			t.Errorf("dashboard panel of type %s has no query", panel.Type)
		}
		for _, target := range panel.Targets {
			if strings.TrimSpace(target.Expr) == "" {
				t.Errorf("dashboard has an empty query")
			}
			if strings.Contains(target.Expr, demoGameServicePlaceholder) {
				t.Errorf("dashboard query kept the service placeholder: %s", target.Expr)
			}
			queries++
		}
	}
	if dashboard.UID != "roost-demo" || queries < 20 {
		t.Errorf("dashboard uid=%q queries=%d", dashboard.UID, queries)
	}
	// A17: the scene's session-open-failure counter (历史：RR-20260927-19 的重开失败计数，
	// 0a5c9f53 起只计 Join 打开会话的失败) is listed in the observability README
	// and must have a panel; the name must be the one scene.go counts on, which
	// /metrics exports as is (already _total).
	openPanel := false
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			openPanel = openPanel || strings.Contains(target.Expr, "scene_session_open_failed_total{")
		}
	}
	if !openPanel {
		t.Errorf("the dashboard has no panel querying scene_session_open_failed_total")
	}
	if scene := read("internal/service/game/scene.go"); !strings.Contains(scene, `sceneOpenFailedMetric = "scene_session_open_failed_total"`) {
		t.Errorf("scene.go no longer counts on scene_session_open_failed_total; update the dashboard panel with it")
	}
	// Round 12 (A17 follow-up): the observability README lists
	// configdata_rollback_total{trigger}; the dashboard must chart it by
	// trigger, and kit/configdata must still count under that name and label
	// (configdata.rollback.total exports as configdata_rollback_total).
	rollbackPanel := false
	for _, panel := range dashboard.Panels {
		for _, target := range panel.Targets {
			rollbackPanel = rollbackPanel || (strings.Contains(target.Expr, "configdata_rollback_total{") && strings.Contains(target.Expr, "by (trigger)"))
		}
	}
	if !rollbackPanel {
		t.Errorf("the dashboard has no panel querying configdata_rollback_total by trigger")
	}
	if readme := read("deploy/dev/observability/README.md"); !strings.Contains(readme, "configdata_rollback_total{trigger") {
		t.Errorf("the observability README no longer lists configdata_rollback_total{trigger}; keep the panel and the README together")
	}
	if source, err := os.ReadFile(filepath.Join("..", "..", "..", "wiring", "configdata", "configdata.go")); err != nil {
		t.Errorf("read kit/configdata: %v", err)
	} else if !strings.Contains(string(source), `IncCounter("configdata.rollback.total", metrics.Labels{"trigger": `) {
		t.Errorf("wiring/configdata no longer counts configdata.rollback.total{trigger}; update the dashboard panel with it")
	}
	if scrape := read("deploy/dev/observability/prometheus.yml"); !strings.Contains(scrape, "job_name: game") || !strings.Contains(scrape, ":9300") {
		t.Errorf("prometheus.yml does not scrape the game process and the load test:\n%s", scrape)
	}
	if command := read("cmd/loadtest/main.go"); !strings.Contains(command, "metrics.PrometheusText(metrics.Snapshot())") {
		t.Errorf("loadtest does not expose its metrics")
	}
	// The whole player-tcp workflow, as doctor judges it: access declared,
	// transport generated, authenticator real, listener enabled.
	manifest, err := LoadManifest(target)
	if err != nil {
		t.Fatal(err)
	}
	items, err := checkPlayerTCPWorkflow(target, manifest)
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range items {
		if item.Status != StatusOK {
			t.Errorf("doctor %s: %s", item.Name, item.Detail)
		}
	}
}

// The demo's service files follow the game service's name: a project whose
// first service is not called "game" gets them under its own directory, in
// its own package, naming itself correctly.
func TestDemoTemplateFollowsTheGameServiceName(t *testing.T) {
	t.Parallel()
	target := filepath.Join(t.TempDir(), "planet")
	if _, _, err := NewProject(NewOptions{
		Name: "planet", Module: "example.com/planet", Out: target, Services: []string{"arena"},
		Mods: []string{"configdata", "mongo", "nats", "dataengine", "nest"}, Template: demoTemplateName,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(target, "internal", "service", "game")); !os.IsNotExist(err) {
		t.Fatalf("internal/service/game exists in a project whose game service is arena (err=%v)", err)
	}
	raw, err := os.ReadFile(filepath.Join(target, "internal", "service", "arena", "level_up_mail.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"package " + defaultServicePackage("arena"), `"arena-level-up-mail"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("level_up_mail.go lacks %q:\n%s", want, raw)
		}
	}
	if strings.Contains(string(raw), demoGameServicePlaceholder) || strings.Contains(string(raw), demoGameServicePackagePlaceholder) {
		t.Errorf("a placeholder survived:\n%s", raw)
	}
	service, err := os.ReadFile(filepath.Join(target, "internal", "service", "arena", "service.go"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(service), `app.ServiceName("arena")`) {
		t.Errorf("service.go does not name the arena service:\n%s", service)
	}
}

// sessionTokenPrefixName is the identifier the demo authenticator declares.
// The test asserts on the name rather than the literal so renaming the
// credential format does not silently pass.
const sessionTokenPrefixName = "sessionTokenPrefix"

// Every generated Makefile carries a loadtest target. The target guards on
// cmd/loadtest existing, so the game template — which ships no load test —
// keeps the same Makefile and tells the developer what to generate instead.
func TestGeneratedMakefileHasLoadtestTarget(t *testing.T) {
	m := DefaultManifest("planet", "example.com/planet", []string{"game"}, nil, nil)
	plan, err := renderProject(m)
	if err != nil {
		t.Fatal(err)
	}
	body := string(plan["Makefile"].Body)
	for _, want := range []string{"\nloadtest:\n", "LOADTEST_ENDPOINT ?= ", "LOADTEST_COUNT ?= ", "LOADTEST_ACCOUNT_NATS ?= ", "test -d cmd/loadtest", "go run ./cmd/loadtest -endpoint $(LOADTEST_ENDPOINT) -count $(LOADTEST_COUNT)", "-account-nats $(LOADTEST_ACCOUNT_NATS)"} {
		if !strings.Contains(body, want) {
			t.Errorf("Makefile lacks %q", want)
		}
	}
	if !strings.Contains(body, " loadtest ") && !strings.Contains(body, " loadtest\n") {
		t.Errorf("loadtest is not declared .PHONY")
	}
}

// C4（维护者决定，docs/feature/C4-ACTIVITY-GROUPS-FILE-2026-10-06.md）：托管 activity 协调器的工程生成
// 一份活动组文件 configs/activity_groups.yaml，协调器与 game-demo 的 game 都经 activity.groups_file 读它。
// 承诺：
//   - 文件存在，一个以工程名为 id 的组，成员覆盖生成的各部署方式给第一个业务服务的 sid（本机 / k8s /
//     shell 1000、second-game.sh 1001、生产 compose 的 1000+N），game 按任何一种生成的方式启动都在组里；
//   - 协调器的开发 / 生产示例配置写 groups_file，不再写 sweep_groups: []（组 id 只写在组文件里）；
//   - game-demo 的 game 三份配置写 groups_file，不再有 activity.game_sids；
//   - 文件随发布走：Dockerfile 把它拷到 /app 下同一相对路径，shell install 把它装进 release；
//   - second-game.sh 在 game 读组文件时先检查自己的 sid 在文件里。
// 文件的校验规则本身（≤ 64、重复、int32……）由 kit/service/global/activity 的 groups 测试守；生成器不得
// import kit（dependency_boundary_test），这里按文件格式直接解析。

func TestGameDemoActivityGroupsFileIsTheOneDefinition(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}

	var file struct {
		Groups []struct {
			ID       string  `yaml:"id"`
			GameSIDs []int32 `yaml:"game_sids"`
		} `yaml:"groups"`
	}
	if err := yaml.Unmarshal([]byte(read(activityGroupsFile)), &file); err != nil {
		t.Fatalf("decode %s: %v", activityGroupsFile, err)
	}
	if len(file.Groups) != 1 || file.Groups[0].ID != m.Project.Name {
		t.Fatalf("%s groups = %+v, want one group named %q", activityGroupsFile, file.Groups, m.Project.Name)
	}
	members := file.Groups[0].GameSIDs

	// Every sid a generated deployment starts the game with is a member.
	gameService := m.Access["player"].Service
	composeSID := regexp.MustCompile(`"` + regexp.QuoteMeta(gameService) + `", "--sid=(\d+)"`).FindStringSubmatch(read("deploy/docker/docker-compose.prod.yaml"))
	if composeSID == nil {
		t.Fatal("the production compose does not start the game with a --sid")
	}
	k8sSID := regexp.MustCompile(`name: ROOST_SID\s+value: "(\d+)"`).FindStringSubmatch(read("deploy/k8s/base/" + gameService + ".yaml"))
	if k8sSID == nil {
		t.Fatal("the k8s workload sets no ROOST_SID")
	}
	secondSID := regexp.MustCompile(`SECOND_SID:-(\d+)`).FindStringSubmatch(read("deploy/dev/second-game.sh"))
	devSID := regexp.MustCompile(`SID:-(\d+)`).FindStringSubmatch(read("deploy/dev/run.sh"))
	if secondSID == nil || devSID == nil {
		t.Fatal("the dev scripts set no default sid")
	}
	for _, started := range []struct{ how, sid string }{
		{"production compose", composeSID[1]}, {"k8s", k8sSID[1]}, {"second-game.sh", secondSID[1]}, {"run.sh", devSID[1]},
	} {
		sid, _ := strconv.Atoi(started.sid)
		if !slices.Contains(members, int32(sid)) {
			t.Fatalf("%s starts the game as sid %d, which is in no group of %s %v: that game would refuse to start", started.how, sid, activityGroupsFile, members)
		}
	}

	for _, rel := range []string{"configs/service/config.activity.yaml", "configs/service/config.activity.prod.example.yaml"} {
		leaves := configLeaves(t, rel, read(rel))
		if leaves["activity.groups_file"] != activityGroupsFile {
			t.Fatalf("%s activity.groups_file = %q, want %q", rel, leaves["activity.groups_file"], activityGroupsFile)
		}
		if _, ok := leaves["activity.sweep_groups"]; ok {
			t.Fatalf("%s still writes activity.sweep_groups; the group ids are written once, in %s", rel, activityGroupsFile)
		}
	}
	for _, rel := range []string{
		"configs/service/config." + gameService + ".yaml",
		"configs/service/config." + gameService + ".prod.example.yaml",
	} {
		leaves := configLeaves(t, rel, read(rel))
		if leaves["activity.groups_file"] != activityGroupsFile {
			t.Fatalf("%s activity.groups_file = %q, want %q", rel, leaves["activity.groups_file"], activityGroupsFile)
		}
		if _, ok := leaves["activity.game_sids"]; ok {
			t.Fatalf("%s still has the removed activity.game_sids", rel)
		}
	}

	if want := "COPY --from=build /src/" + activityGroupsFile + " /app/" + activityGroupsFile; !strings.Contains(read("Dockerfile"), want) {
		t.Fatalf("the Dockerfile does not ship the groups file (%q)", want)
	}
	if want := `install -m 0644 "$ACTIVITY_GROUPS" "$RELEASE_ROOT/` + activityGroupsFile + `"`; !strings.Contains(read("deploy/shell/install.sh"), want) {
		t.Fatalf("install.sh does not install the groups file into the release (%q)", want)
	}
	if !strings.Contains(read("deploy/dev/second-game.sh"), "check_activity_group\n") {
		t.Fatal("second-game.sh does not check its sid against the groups file before starting")
	}
}

// A project without the activity coordinator gets no groups file and no
// Dockerfile line for one.
func TestNoActivityGroupsFileWithoutTheCoordinator(t *testing.T) {
	t.Parallel()
	m := Manifest{Project: ProjectSpec{Name: "plain"}, Services: map[string]ServiceSpec{"game": {}}}
	if hostsActivityCoordinator(m) {
		t.Fatal("a project with no activity service hosts the coordinator")
	}
	if strings.Contains(renderDockerfile(m), activityGroupsFile) {
		t.Fatal("the Dockerfile copies a groups file the project does not have")
	}
	if guard, install := renderShellActivityGroups(m); guard != "" || install != "" {
		t.Fatal("install.sh requires a groups file the project does not have")
	}
}

// game-demo 的生产示例配置与 k8s Secret 示例必须能让 game 通过 Init 的配置校验：platform.payment_secret 非空、
// App 单实例锁打开、activity.key_prefix 非空、activity.groups_file 指向活动组文件、platform.key_prefix 按
// kit/mods.KeyPrefix 的规则非空且无空白。
// 同时守“与开发配置同源”：每个服务的生产示例与开发配置的键集合一致；Secret 示例里 demo 追加的段与生产示例一致；
// 前缀取值与开发配置、以及所属服务自己的配置相同；密钥不沿用开发值（生产里是 CHANGE_ME，由运维填）。
// Secret 与生产示例整体同源由 k8s_contract_test.go 守。
// App 单实例锁：game 带 dataengine，三份配置都要 singleton.enabled: true、key_prefix 为 roost:<project>:singleton，
// 取值满足 App 启动校验的三条关系。按玩家的 Redis 表已删除，game_route 段在三份配置里都不能再有。

// configLeaves flattens a YAML document into dotted key -> scalar text.
func configLeaves(t *testing.T, name, body string) map[string]string {
	t.Helper()
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	out := map[string]string{}
	var walk func(prefix string, value any)
	walk = func(prefix string, value any) {
		if m, ok := value.(map[string]any); ok && len(m) > 0 {
			for key, child := range m {
				next := key
				if prefix != "" {
					next = prefix + "." + key
				}
				walk(next, child)
			}
			return
		}
		out[prefix] = fmt.Sprint(value)
	}
	walk("", doc)
	return out
}

// secretExampleConfig returns the config.yaml a k8s Secret example embeds.
func secretExampleConfig(t *testing.T, name, body string) string {
	t.Helper()
	var secret struct {
		StringData map[string]string `yaml:"stringData"`
	}
	if err := yaml.Unmarshal([]byte(body), &secret); err != nil {
		t.Fatalf("decode %s: %v", name, err)
	}
	config, ok := secret.StringData["config.yaml"]
	if !ok {
		t.Fatalf("%s has no stringData.config.yaml", name)
	}
	return config
}

func sortedKeys(set map[string]string) []string {
	keys := make([]string, 0, len(set))
	for key := range set {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// checkDemoGameInitConfig applies the checks the demo's game code makes on its
// configuration at Init, in the order it makes them, and returns the first
// refusal with the game's own wording.
func checkDemoGameInitConfig(body string) error {
	cfg := viper.New()
	cfg.SetConfigType("yaml")
	if err := cfg.ReadConfig(strings.NewReader(body)); err != nil {
		return err
	}
	// game/controllers/player/controller.go
	if cfg.GetString("platform.payment_secret") == "" {
		return fmt.Errorf("player controller: platform.payment_secret is empty")
	}
	// internal/service/<game>/activity.go: app.ModSingleton is published only
	// when the App singleton lock is on.
	if !cfg.GetBool("singleton.enabled") {
		return fmt.Errorf("activity: capability %q not found; needs singleton.enabled=true", "singleton")
	}
	if cfg.GetString("activity.key_prefix") == "" {
		return fmt.Errorf("activity: activity.key_prefix is empty")
	}
	if cfg.GetString("activity.groups_file") == "" {
		return fmt.Errorf("activity: activity.groups_file is empty")
	}
	if cfg.IsSet("activity.game_sids") {
		return fmt.Errorf("activity: activity.game_sids is the removed per-service list; the group comes from activity.groups_file (C4)")
	}
	// internal/service/<game>/purchase_drain.go: mods.KeyPrefix(cfg, "platform")
	if prefix := strings.TrimSpace(cfg.GetString("platform.key_prefix")); prefix == "" || strings.ContainsAny(prefix, " \t\n") {
		return fmt.Errorf("purchase drain: platform.key_prefix %q", prefix)
	}
	return nil
}

// demoGameSection reports whether a dotted key belongs to one of the sections
// the demo appends to the game service's configs.
func demoGameSection(key string) bool {
	for _, section := range []string{"activity", "platform", "singleton"} {
		if key == section || strings.HasPrefix(key, section+".") {
			return true
		}
	}
	return false
}

func TestGameDemoProductionConfigsPassTheGameInitChecks(t *testing.T) {
	t.Parallel()
	root := newGameDemo(t)
	m, err := LoadManifest(root)
	if err != nil {
		t.Fatal(err)
	}
	read := func(rel string) string {
		t.Helper()
		raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
		if err != nil {
			t.Fatal(err)
		}
		return string(raw)
	}
	gameService := m.Access["player"].Service
	if gameService == "" {
		t.Fatal("game-demo declares no player access service")
	}
	for _, service := range sortedServiceNames(m) {
		devRel := "configs/service/config." + service + ".yaml"
		prodRel := "configs/service/config." + service + ".prod.example.yaml"
		secretRel := "deploy/k8s/base/secret." + service + ".example.yaml"
		configs := map[string]string{
			prodRel:   read(prodRel),
			secretRel: secretExampleConfig(t, secretRel, read(secretRel)),
		}
		dev := configLeaves(t, devRel, read(devRel))
		for rel, body := range configs {
			leaves := configLeaves(t, rel, body)
			var missing []string
			for _, key := range sortedKeys(dev) {
				if rel == secretRel && !demoGameSection(key) {
					continue
				}
				if _, ok := leaves[key]; !ok {
					missing = append(missing, key)
				}
			}
			if len(missing) > 0 {
				t.Errorf("%s lacks keys the dev config %s has: %v", rel, devRel, missing)
			}
			for _, key := range sortedKeys(leaves) {
				if strings.HasSuffix(key, "key_prefix") && dev[key] != "" && leaves[key] != dev[key] {
					t.Errorf("%s: %s = %q, dev config has %q; the two come from one source", rel, key, leaves[key], dev[key])
				}
				if strings.Contains(strings.ToLower(leaves[key]), "dev-") {
					t.Errorf("%s: %s carries the dev value %q", rel, key, leaves[key])
				}
			}
			if service != gameService {
				continue
			}
			if err := checkDemoGameInitConfig(body); err != nil {
				t.Errorf("%s: the game refuses it at Init: %v", rel, err)
			}
			assertSingletonOn(t, rel, leaves, "roost:planet:singleton")
			for key := range leaves {
				if key == "game_route" || strings.HasPrefix(key, "game_route.") {
					t.Errorf("%s still has %s; the per-player route table and its keyspace were removed with static binding", rel, key)
				}
			}
			// The prefixes the game borrows are the owning services' own.
			for owner, key := range map[string]string{"activity": "activity.key_prefix", "platform": "platform.key_prefix"} {
				ownerRel := "configs/service/config." + owner + ".prod.example.yaml"
				want := configLeaves(t, ownerRel, read(ownerRel))[key]
				if got := leaves[key]; got != want {
					t.Errorf("%s: %s = %q, but the %s service keeps its keys under %q", rel, key, got, owner, want)
				}
			}
		}
	}
}

// 实际生成 Entity/DAO/PB/Sender，经过真实 TCP、NATS、Nest 和文件 WAL。
// 投影记录器是测试存储，不把它记成真实 Mongo 或独立进程集群验收。
const gateConsumerTest = `package game
import (
 "context"
 "fmt"
 "net"
 "os"
 "sync"
 "testing"
 "time"
 "github.com/spf13/viper"
 "github.com/tjbdwanghaibo/roost-core/framework/app"
 "github.com/tjbdwanghaibo/roost-core/wiring/mods"
 accessplayer "example.com/planet/internal/access/player"
 playertcp "example.com/planet/internal/access/player/tcp"
 "github.com/tjbdwanghaibo/roost-core/client/wire"
 coredata "github.com/tjbdwanghaibo/roost-core/framework/dataengine"
 dataengine "github.com/tjbdwanghaibo/roost-core/framework/dataengine/engine"
 "github.com/tjbdwanghaibo/roost-core/framework/nest"
 "github.com/tjbdwanghaibo/roost-core/framework/nestwal"
 "github.com/tjbdwanghaibo/roost-core/framework/entity"
 "github.com/tjbdwanghaibo/roost-core/framework/sync/entitysync"
 "github.com/tjbdwanghaibo/roost-core/framework/sync/frame"
 "github.com/tjbdwanghaibo/roost-core/infra/network/gateway"
 fnats "github.com/tjbdwanghaibo/roost-core/infra/network/nats"
 "github.com/tjbdwanghaibo/roost-core/infra/network/nats/driver"
 gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
 "example.com/planet/game/handler"
 syncsender "example.com/planet/game/handler/syncsender"
 "example.com/planet/game/playeraccess"
 "example.com/planet/protocol/pb"
)
type gateChecker struct{matches func(context.Context,string,gateway.ProcessIdentity)(bool,error)}
func(checker gateChecker)Matches(ctx context.Context,role string,sid int32,incarnation string)(bool,error){return checker.matches(ctx,role,gateway.ProcessIdentity{ServerID:sid,Incarnation:incarnation})}
type gateProjection struct{mu sync.Mutex;records map[coredata.TransactionID]coredata.CommitRecord}
func (store *gateProjection) Project(_ context.Context,record coredata.CommitRecord)error{store.mu.Lock();defer store.mu.Unlock();store.records[record.ID]=coredata.CloneCommitRecord(record);return nil}
func TestGeneratedGateNestDAOAndWAL(t *testing.T){
 url:=os.Getenv("ROOST_DATAENGINE_IT_NATS_URL");if url==""{t.Skip("private NATS URL not configured")}
 player,world:=newScenePlayer(t,7001),newSceneWorld(t)
 handler.RegisterAddExpNestHandlers();t.Cleanup(nest.ResetHandlersForTest)
 wal,err:=nestwal.Open(nestwal.DefaultOptions(t.TempDir()));if err!=nil{t.Fatal(err)}
 store:=&gateProjection{records:make(map[coredata.TransactionID]coredata.CommitRecord)}
 projector,err:=dataengine.NewProjector(wal,store,dataengine.DefaultProjectorOptions());if err!=nil{t.Fatal(err)}
 t.Cleanup(func(){ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();if err:=projector.Close(ctx);err!=nil{t.Error(err)}})
 syncTransport:=&gatewiring.Transport{SyncMessageID:501,LockstepMessageID:502}
 syncManager,err:=entitysync.NewManager(entitysync.ManagerConfig{Mode:entitysync.ModeOnChange,Interval:time.Hour,Transport:syncTransport});if err!=nil{t.Fatal(err)}
 if err=syncManager.Register(player.Sync());err!=nil{t.Fatal(err)}
 t.Cleanup(func(){_ = syncManager.Close(context.Background())})
 business:=nest.NewEngine(nest.NestOptionWithEntitySync(syncManager),nest.NestOptionWithGetter(sceneGetter{player.ID():player,world.ID():world}),nest.NestOptionWithTransactionCommitter(projector),nest.NestOptionWithWorkerPools(nest.WorkerPoolConfig{Workers:4,QueueCap:32}, nest.WorkerPoolConfig{},nest.WorkerPoolConfig{}))
 if err:=business.Start();err!=nil{t.Fatal(err)};t.Cleanup(func(){_ = business.Shutdown(context.Background())})
 registry:=playeraccess.NewProtocolRegistry()
 if err:=playeraccess.RegisterProtocol[*pb.AddExpRequest,*pb.AddExpResponse](registry,42,43,pb.UnmarshalAddExpRequest,pb.MarshalAddExpResponse,func(ctx *playeraccess.Context,request *pb.AddExpRequest)(*pb.AddExpResponse,error){
  if ctx.PlayerID!=7001{return nil,gateway.ErrUnauthenticated}
  gained,err:=syncsender.NewAddExpSender(business).MultiSync_AddExp(ctx.Context(),player.ID(),world.ID(),request.Amount)
  return &pb.AddExpResponse{LevelsGained:gained},err
 });err!=nil{t.Fatal(err)};if err:=registry.Seal();err!=nil{t.Fatal(err)}
 config:=gateway.DefaultConfig();config.Namespace=fmt.Sprintf("roost.generated.gate.n%d",time.Now().UnixNano())
 gateID,gameID:=gateway.ProcessIdentity{ServerID:11,Incarnation:"gate000000000001"},gateway.ProcessIdentity{ServerID:22,Incarnation:"game000000000001"}
 raw:=func(role string,identity gateway.ProcessIdentity)fnats.RawClient{
  client,err:=driver.NewClient(fnats.DefaultConfig(url),driver.ClientOptions{ReconnectBufferBytes:-1});if err!=nil{t.Fatal(err)};t.Cleanup(client.Close)
  prefix,_:=gateway.InboxPrefix(config.Namespace,role,identity);scoped,err:=client.ForInbox(prefix);if err!=nil{t.Fatal(err)};return scoped
 }
 gameRaw,gateRaw:=raw("game",gameID),raw("gate",gateID)
 auth:=gateway.AuthenticatorFunc(func(context.Context,string,net.Addr)(gateway.Principal,error){return gateway.Principal{PlayerID:7001,ServerID:22,SessionID:"generated"},nil})
 matches:=func(_ context.Context,role string,id gateway.ProcessIdentity)(bool,error){return (role=="game"&&id==gameID)||(role=="gate"&&id==gateID),nil}
 cfg:=viper.New();cfg.Set("player.tcp.enabled",false)
 capabilities:=app.NewRegistry(cfg)
 for _,capability:=range []app.Capability{{Name:app.ModSingletonIncarnation,Value:app.SingletonIncarnation{Sid:gameID.ServerID,Token:gameID.Incarnation}},{Name:app.ModSingletonIdentityChecker,Value:gateChecker{matches}},{Name:mods.ModNatsRaw,Value:gameRaw},{Name:accessplayer.Name,Value:&accessplayer.Runtime{Protocols:registry}}}{if err:=capabilities.Register(capability.Name,capability.Value);err!=nil{t.Fatal(err)}}
 options:=gatewiring.DefaultOptions();options.Config=config;options.Authenticator=auth;options.Ready=func()bool{return true}
 options.OnActive=func(binding gateway.Binding,id uint64)error{if err:=syncManager.OpenSession(entitysync.SessionID(id));err!=nil{return err};return syncManager.Subscribe(entitysync.SessionID(id),player.ID(),entity.SyncProfile{})}
 options.OnClosed=func(_ gateway.Binding,id uint64){syncManager.CloseSession(entitysync.SessionID(id))}
 ingressMod:=playertcp.NewIngressMod(options)
 if err:=ingressMod.Init(cfg);err!=nil{t.Fatal(err)};if err:=ingressMod.Provide(capabilities);err!=nil{t.Fatal(err)}
 ingress,ok:=app.Lookup[*gateway.GameIngress](capabilities,gatewiring.GameIngressName);if !ok{t.Fatal("generated Game ingress not provided")}
 if _,ok:=app.Lookup[*playertcp.Runtime](capabilities,playertcp.Name);!ok{t.Fatal("generated business runtime missing")}

 syncTransport.Game=ingress
 if err=syncManager.Start(context.Background());err!=nil{t.Fatal(err)}
 if err=ingressMod.Start();err!=nil{t.Fatal(err)}
 gate,err:=gateway.NewGate(config,gateID,gateway.GateDependencies{Client:gateRaw,QueueFactory:gatewiring.AsyncQueueFactory,Ready:func()bool{return true},Matches:matches,ResolveGame:func(context.Context,int32)(gateway.ProcessIdentity,error){return gameID,nil}});if err!=nil{t.Fatal(err)}
 if err=gate.Start();err!=nil{t.Fatal(err)}
 tcp:=gateway.DefaultTCPConfig();tcp.Addr="127.0.0.1:0";tcp.MaxPayloadBytes=uint32(config.MaxPayloadBytes)
 server,err:=gateway.NewTCPServer(tcp,gate.Forward,auth);if err!=nil{t.Fatal(err)};if err=server.ConnectHandshake(gate);err!=nil{t.Fatal(err)}
 ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel()
 for _,client:=range []fnats.RawClient{gameRaw,gateRaw}{if err:=client.FlushContext(ctx);err!=nil{t.Fatal(err)}}
 if err=server.Start();err!=nil{t.Fatal(err)}
 t.Cleanup(func(){ctx,cancel:=context.WithTimeout(context.Background(),5*time.Second);defer cancel();if err:=gate.Stop(ctx);err!=nil{t.Error(err)};if err:=server.Stop(ctx);err!=nil{t.Error(err)};if err:=ingressMod.StopWithContext(ctx);err!=nil{t.Error(err)}})
 conn,err:=net.DialTimeout("tcp",server.Addr().String(),time.Second);if err!=nil{t.Fatal(err)};defer conn.Close();_ = conn.SetDeadline(time.Now().Add(5*time.Second))
 if err=wire.Write(conn,[]*wire.Packet{{Seq:1,Payload:[]byte("ticket")}},8192);err!=nil{t.Fatal(err)};if _,err=wire.Read(conn,8192);err!=nil{t.Fatal(err)}
 // Interval 为一小时，下面的增量必须由 handler 完成后的 on_change 唤醒。
 snapshot,err:=wire.Read(conn,config.MaxPayloadBytes);if err!=nil{t.Fatal(err)}
 if (wire.Header{Flags:snapshot.Flags}).Kind()!=wire.PayloadSync||snapshot.MsgID!=501{t.Fatalf("initial Sync packet=%+v",snapshot)}
 full,err:=entitysync.DecodeFrame(snapshot.Payload,frame.DefaultLimits());if err!=nil{t.Fatal(err)}
 if len(full.Objects)!=1||len(full.Objects[0].Components)!=1{t.Fatalf("snapshot=%+v",full)}
 initial,err:=entitysync.DecodeSubjectUpdate(full.Objects[0].Components[0].Data,0);if err!=nil||!initial.Full||initial.SubjectID!=player.ID(){t.Fatalf("initial update=%+v error=%v",initial,err)}
 data,err:=pb.MarshalAddExpRequest(&pb.AddExpRequest{Amount:250});if err!=nil{t.Fatal(err)}
 if err=wire.Write(conn,[]*wire.Packet{{MsgID:42,Seq:2,Payload:data}},config.MaxPayloadBytes);err!=nil{t.Fatal(err)}
 var gotResponse,gotDelta bool
 for range 2{
  response,err:=wire.Read(conn,config.MaxPayloadBytes);if err!=nil{t.Fatal(err)}
  if (wire.Header{Flags:response.Flags}).Kind()==wire.PayloadSync{
   delta,err:=entitysync.DecodeFrame(response.Payload,frame.DefaultLimits());if err!=nil{t.Fatal(err)}
   if delta.Epoch!=full.Epoch||delta.Tick<=full.Tick||len(delta.Objects)!=1||len(delta.Objects[0].Components)!=1{t.Fatalf("delta frame=%+v",delta)}
   update,err:=entitysync.DecodeSubjectUpdate(delta.Objects[0].Components[0].Data,0);if err!=nil||update.Full||update.SubjectID!=player.ID()||update.BaseVersion!=initial.Version||update.Version<=initial.Version||update.Payload.Len()==0{t.Fatalf("delta=%+v err=%v",update,err)}
   gotDelta=true
  }else{decoded,err:=pb.UnmarshalAddExpResponse(response.Payload);if err!=nil||decoded.Code!=0||decoded.LevelsGained==0||response.MsgID!=43||response.Seq!=2{t.Fatalf("response=%+v packet=%+v err=%v",decoded,response,err)};gotResponse=true}
 }
 if !gotResponse||!gotDelta{t.Fatalf("response=%v delta=%v",gotResponse,gotDelta)}
 if err=projector.Flush(ctx);err!=nil{t.Fatal(err)}
 store.mu.Lock();defer store.mu.Unlock()
 if len(store.records)!=1{t.Fatalf("unique transactions=%d",len(store.records))}
 for _,record:=range store.records{ids:=make(map[int64]bool);for _,mutation:=range record.Mutations{ids[mutation.Key.ID]=true};if len(ids)!=2{t.Fatalf("two-entity transaction=%+v",record)}}
 if stats:=wal.Stats();stats.Appended!=1{t.Fatalf("WAL=%+v",stats)}
}
`

func TestGateGeneratedBusinessConsumer(t *testing.T) {
	if testing.Short() {
		t.Skip("compiles and executes formal generated business consumer")
	}
	if os.Getenv("ROOST_DATAENGINE_IT_NATS_URL") == "" {
		t.Skip("private NATS URL not configured")
	}
	repo, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatal(err)
	}
	root := copyOfNewProject(t, "game-demo")
	modPath := filepath.Join(root, "go.mod")
	mod, err := os.ReadFile(modPath)
	if err != nil {
		t.Fatal(err)
	}
	mod = regexp.MustCompile(`(?m)^replace github\.com/tjbdwanghaibo/roost-core\b.*$\n?`).ReplaceAll(mod, nil)
	mod = append(mod, []byte("\nreplace github.com/tjbdwanghaibo/roost-core => "+strconv.Quote(filepath.ToSlash(repo))+"\n")...)
	if err = os.WriteFile(modPath, mod, 0644); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(root, "internal/service/game/gate_consumer_test.go"), []byte(gateConsumerTest), 0644); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, "go", "test", "-mod=mod", "-race", "-count=1", "-v", "-run", "^TestGeneratedGateNestDAOAndWAL$", "./internal/service/game")
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off")
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated Gate business: %v\n%s", err, output)
	}
	t.Log(string(output))
}
