package roost

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"go/parser"
	"go/token"
	"io"
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
	"github.com/tjbdwanghaibo/roost-core/codegen/internal/protocol"
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
		if len(step.modules) > 0 {
			// 模块步骤装的是 modules/ 里的模块，名字必须在目录里。
			catalog, err := loadModuleCatalog()
			if err != nil {
				t.Fatal(err)
			}
			for _, name := range step.modules {
				if _, ok := catalog[name]; !ok {
					t.Errorf("a step installs module %q, which is not in modules/", name)
				}
			}
			continue
		}
		if step.write == "" {
			t.Fatal("a step neither adds, runs, installs modules nor writes")
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

// 模块带来的组件（bag 的 Bag、profile 的 Profile、enter 的 Enter、mailbox 的 Mailbox）在安装模块的那一步才落盘。demo 的步骤因此守四条：
//   - 全部模块在同一步里装，顺序固定：号按这个顺序、每次取最小的空号分配，新换用的模块只能加在末尾；
//   - 不再自己建这些组件，道具表也不再自己加（它随 bag 模块来）；
//   - 建在这些组件上的 Nest handler 不跑 roost add handler（它要求组件文件已经在盘上，而且只写一个随即被覆盖的骨架）：
//     handler 文件直接由 write 步骤写出、留在原处，模块那一步跑生成器时全部 handler 都在盘上；
//   - 覆盖模块文件的 write 排在安装之后，否则安装器会因为落点被占用而拒绝。
func TestDemoStepsLeaveModuleComponentsToTheModules(t *testing.T) {
	steps := demoScaffoldSteps("game")
	installed := -1
	var modules []string
	for index, step := range steps {
		if len(step.modules) == 0 {
			continue
		}
		if installed >= 0 {
			t.Fatalf("modules are installed in more than one step (%v, then %v): the first step's generators and go mod tidy would run on a project that refers to modules not installed yet", modules, step.modules)
		}
		installed, modules = index, step.modules
	}
	if installed < 0 {
		t.Fatal("no step installs the modules")
	}
	if got, want := strings.Join(modules, " "), "enter chat leaderboard bag profile mailbox skillbook rehome attribute equipment gm purchase team alliance login portal servertime"; got != want {
		t.Errorf("the modules are installed as %q, want %q: the order decides every allocated id, so a newly adopted module goes at the end", got, want)
	}
	catalog, err := loadModuleCatalog()
	if err != nil {
		t.Fatal(err)
	}
	components := map[string]string{}  // 组件名 -> 带来它的模块
	moduleFiles := map[string]string{} // 模块文件 -> 模块
	for _, name := range modules {
		spec := catalog[name]
		if spec.Component != "" {
			components[spec.Component] = name
		}
		for _, rel := range spec.Files {
			moduleFiles[rel] = name
		}
	}
	for _, component := range []string{"Bag", "Profile", "Mailbox"} {
		if components[component] == "" {
			t.Errorf("no installed module brings the %s component", component)
		}
	}
	written := map[string]bool{}
	for index, step := range steps {
		if step.add != nil {
			if module := components[toPascal(step.add.Name)]; step.add.Kind == "component" && module != "" {
				t.Errorf("the demo still adds its own %s component: it comes with the %s module", step.add.Name, module)
			}
			if module := components[toPascal(step.add.Component)]; step.add.Kind == "handler" && module != "" {
				t.Errorf("add handler %s --component %s cannot run: the component arrives with the %s module in the last step; write the handler file instead", step.add.Name, step.add.Component, module)
			}
			if step.add.Kind == "table" && step.add.Name == "Item" {
				t.Error("the demo still adds its own item table: it comes with the bag module")
			}
		}
		if step.write == "" {
			continue
		}
		written[step.write] = true
		if module := moduleFiles[step.write]; module != "" && index < installed {
			t.Errorf("%s is the %s module's file and is written before the module is installed", step.write, module)
		}
	}
	// demo 只写自己的跨模块玩法。联盟、邮件和付费发货 handler 由各自模块提供。
	for _, rel := range []string{
		"game/handler/add_item.go", "game/handler/start_gift.go", "game/handler/gift_debit.go", "game/handler/gift_refund.go",
		"game/handler/purchase_balance.go",
		"game/handler/add_exp.go", "game/handler/claim_dungeon.go",
	} {
		if !written[rel] {
			t.Errorf("no step writes %s", rel)
		}
	}
}

// 邮件由 mailbox 模块带来（连同它依赖的 wallet；模块在同一步里装、顺序由上一条用例钉住）。demo 不再有自己的邮件协议、
// handler、组件与错误码步骤，也不覆盖 mailbox / wallet 的任何文件：发件方只调模块的 SendTo。
func TestDemoLeavesMailToTheMailboxModule(t *testing.T) {
	steps := demoScaffoldSteps("game")
	installs := false
	for _, step := range steps {
		installs = installs || contains(step.modules, "mailbox")
		if contains(step.modules, "wallet") {
			t.Errorf("wallet is listed in %v: it comes with mailbox as its dependency, and listing it would move every later id", step.modules)
		}
	}
	if !installs {
		t.Fatal("no step installs the mailbox module")
	}
	catalog, err := loadModuleCatalog()
	if err != nil {
		t.Fatal(err)
	}
	moduleFiles := map[string]string{}
	for _, name := range []string{"mailbox", "wallet"} {
		for _, rel := range catalog[name].Files {
			moduleFiles[rel] = name
		}
	}
	for _, step := range steps {
		if step.add != nil {
			switch {
			case step.add.Kind == "protocol" && (step.add.Name == "ListMail" || step.add.Name == "ClaimMail"),
				step.add.Kind == "handler" && step.add.Name == "ClaimMailReward",
				step.add.Kind == "errcode" && step.add.Name == "MailClaim",
				step.add.Kind == "component" && (step.add.Name == "Mailbox" || step.add.Name == "Wallet"):
				t.Errorf("the demo still adds its own %s %s: mail comes with the mailbox module", step.add.Kind, step.add.Name)
			}
		}
		if owner, owned := moduleFiles[step.write]; owned {
			t.Errorf("the demo overwrites %s, a file of the %s module it is meant to use as shipped", step.write, owner)
		}
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
//
// 工程取 game-demo 夹具的副本：夹具的 NewOptions 与这里原先直接传给 NewProject 的逐字段相同，副本与一次新鲜
// NewProject 逐字节相同由 TestProjectFixtureCopiesMatchAFreshNewProject 证明（它对 game-demo 真实生成一次再比对），
// 所以断言对象不变；省掉的是同一组参数的又一次完整生成（本包最慢的两条之一，负载高时 150s 以上）。
func TestDemoTemplateGeneratesABuildableWritePath(t *testing.T) {
	t.Parallel()
	target := copyOfNewProject(t, "game-demo")

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
	// 握手的认证器是 login 模块的（demoModuleSteps），demo 不带自己的一份：生成的“一律拒绝”的桩被模块替换，
	// 票据规则在 game/login。没有“直接报玩家号”的调试旁路。
	if auth := read("internal/access/player/tcp/auth.go"); !strings.Contains(auth, "gamelogin.Authenticator{Accounts: accounts}") || strings.Contains(auth, `"player:"`) {
		t.Errorf("auth.go is not the login module's authenticator, or carries a debug shortcut:\n%s", auth)
	}
	if rules := read("game/login/login.go"); strings.Contains(rules, `"player:"`) || !strings.Contains(rules, `enterTicketPrefix = "enter:"`) {
		t.Errorf("game/login/login.go is not the login module's ticket rules:\n%s", rules)
	}
	// demo 只覆盖登录模块留给工程的渠道入口：demo 渠道，凭据 demo:<open_id>。开发登录、玩家号计数器是模块的。
	if channels := read("internal/service/account/login_channels.go"); !strings.Contains(channels, `DemoChannel svcaccount.Channel = "demo"`) || !strings.Contains(channels, `demoCredentialPrefix = "demo:"`) {
		t.Errorf("login_channels.go is not the demo channel:\n%s", channels)
	}
	for _, rel := range []string{"internal/service/account/login_dev.go", "internal/service/account/login_player_id.go", "game/portal/portal.go", "game/servertime/servertime.go", "game/login/login_enter.go"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(rel))); err != nil {
			t.Errorf("%s is missing: the login, portal and servertime modules are not installed: %v", rel, err)
		}
	}
	// 开发配置开箱可用：开发令牌与本机这台 game 的地址只在 account 的开发配置里；生产示例与 k8s Secret 示例都没有。
	if dev := read("configs/service/config.account.yaml"); !strings.Contains(dev, "\nlogin:\n  dev_token: dev-local\n") || !strings.Contains(dev, "\nportal:\n  servers:\n    \"1000\": 127.0.0.1:7000\n") {
		t.Errorf("the account dev config lacks the development login token or the server directory:\n%s", dev)
	}
	for _, rel := range []string{"configs/service/config.account.prod.example.yaml", "deploy/k8s/base/secret.account.example.yaml"} {
		if production := read(rel); strings.Contains(production, "dev_token: dev-") || strings.Contains(production, "dev-local") || strings.Contains(production, "portal:") {
			t.Errorf("%s carries the development login or the dev server directory:\n%s", rel, production)
		}
	}
	// 背包是 bag 模块装进来的（demoModuleSteps），demo 自己不带一份：组件、PlayerDao.Bag、错误码与道具表的
	// 落点都记在 roost.yaml 的 modules.bag 下。组件只经生成的 mutator 改状态；道具号与单种上限的规则在
	// 模块的 bag_items.go 里，读生成的道具表访问器——demo 只换了道具表的列与行，没有覆盖这个规则文件。
	if bag := read("game/entities/player/bag_component.go"); !strings.Contains(bag, "bag.SetItemsKey(") || !strings.Contains(bag, "bagItemRule(itemID)") || strings.Contains(bag, "PublishSyncDirty") {
		t.Errorf("the bag component is not the bag module's:\n%s", bag)
	}
	if rule := read("game/entities/player/bag_items.go"); !strings.Contains(rule, "generated.ItemByID(itemID)") {
		t.Errorf("the bag's item rule does not read the item table:\n%s", rule)
	}
	manifestText := read("roost.yaml")
	for _, owned := range []string{"    bag:\n", "- PlayerDao.Bag", "- game/entities/player/bag_component.go", "- game/entities/player/bag_items.go",
		"- configs/schema/item.go", "- configs/table/item.csv", "- internal/errors/bag.go", "bag_stack_full:", "BagDiscard:"} {
		if !strings.Contains(manifestText, owned) {
			t.Errorf("roost.yaml does not record %q under the bag module", owned)
		}
	}
	if dao := read("db/def/player.go"); !strings.Contains(dao, "Bag BagState `store:\"bag\" dao:\"persist,sync\"`") || strings.Contains(dao, "store:\"items\"") {
		t.Errorf("PlayerDao does not carry the bag as the module's nested state (or still has the demo's flat Items):\n%s", dao)
	}
	if aggregate := read("game/entities/player/entity.go"); !strings.Contains(aggregate, "`comp:\"CompTypeBag\"`") {
		t.Errorf("the Player aggregate does not hold the module's Bag component:\n%s", aggregate)
	}
	// demo 的业务调用模块组件；充值与邮件的去重状态分别由 purchase / mailbox 模块维护。
	for rel, call := range map[string]string{
		"game/handler/add_item.go":                    "target.BagComp().Add(itemID, int64(count))",
		"game/handler/gift_debit.go":                  "target.BagComp().Remove(itemID, int64(count))",
		"game/handler/gift_refund.go":                 "target.BagComp().Add(itemID, int64(count))",
		"game/handler/alliance_debit.go":              "target.BagComp().Remove(itemID, int64(count))",
		"game/handler/alliance_credit.go":             "target.BagComp().Add(itemID, int64(count))",
		"game/entities/player/equipment_component.go": "c.owner.BagComp().Remove(itemID, 1)",
		"game/entities/player/purchase_item.go":       "owner.BagComp().Add",
	} {
		if body := read(rel); !strings.Contains(body, call) {
			t.Errorf("%s does not go through the bag module's component (%s):\n%s", rel, call, body)
		}
	}
	if state := read("db/def/purchase.go"); !strings.Contains(state, "Applied uint64") {
		t.Errorf("purchase module cursor missing: %s", state)
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
	// The rows are the demo's (the Gold Coin and the two attribute columns),
	// written over the bag module's sample table after the module is installed.
	if data := read("configs/data/item.json"); !strings.Contains(data, "\"id\": 1001") || !strings.Contains(data, "\"id\": 9001") || !strings.Contains(data, "\"attack\": 15") {
		t.Errorf("item.json was not converted from the demo rows:\n%s", data)
	}
	// Coded errors sit in the manifest's errcode space, so `roost id check`
	// owns their uniqueness; an id outside it would fail at add time.
	// The bag's codes are the module's (internal/errors/bag.go), allocated
	// from the same space when the module is installed.
	for _, file := range []string{"internal/errors/exp_amount.go", "internal/errors/profile.go", "internal/errors/bag.go"} {
		if body := read(file); !strings.Contains(body, "errcode.Define(1000") || strings.Contains(body, "TODO") {
			t.Errorf("%s is not a finished coded error:\n%s", file, body)
		}
	}
	for _, gone := range []string{"internal/errors/item_unknown.go", "internal/errors/item_count.go", "internal/errors/bag_full.go", "internal/errors/item_short.go"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(gone))); err == nil {
			t.Errorf("%s is still written: the bag's coded errors are the bag module's", gone)
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
	if chatCollaborators := read("internal/service/chat/collaborators.go"); !strings.Contains(chatCollaborators, "chat.GrantSystem()") || !strings.Contains(chatCollaborators, "gamechat.TypeText") {
		t.Errorf("chat collaborators do not grant the system path and register the text type:\n%s", chatCollaborators)
	}
	// 聊天是 chat 模块装进来的（demoModuleSteps），demo 自己不带一份。
	for _, rel := range []string{"game/chat/chat.go", "protocol/def/chat_send.go", "protocol/def/chat_history.go", "protocol/def/chat_message.go",
		"game/controllers/chat/chat_send.go", "game/controllers/chat/chat_history.go", "game/controllers/chat/chat_push.go", "deploy/dev/run.sh",
		"game/rewards/rewards.go", "game/rewards/rewards_test.go",
		"internal/service/session/collaborators.go", "protocol/def/enter_dungeon.go", "protocol/def/finish_dungeon.go", "game/handler/profile_card.go",
		"game/controllers/player/enter_dungeon.go", "game/controllers/player/finish_dungeon.go",
		"game/skills/fireball.json", "game/skills/catalog.go", "protocol/def/skillbook_catalog.go", "game/controllers/skillbook/skillbook_catalog.go",
		"internal/service/game/gm.go",
		"saga/gift_item/definition.go", "game/gift/gift.go", "game/handler/start_gift.go", "game/handler/gift_debit.go",
		"protocol/def/send_gift.go", "protocol/def/gift_status.go", "game/controllers/player/send_gift.go", "game/controllers/player/gift_status.go",
		"internal/service/game/gift_saga.go", "internal/errors/bag.go", "internal/service/game/bag_sync_test.go",
		"game/battle/battle.go", "protocol/def/battle_input.go", "protocol/def/battle_frame.go",
		"game/controllers/player/battle_input.go", "internal/service/game/battle.go",
		"game/dungeon/dungeon.go", "game/dungeon/dungeon_test.go", "game/handler/claim_dungeon.go", "internal/errors/dungeon_run.go",
		"internal/service/game/battle_test.go", "game/handler/claim_harness_test.go",
		"game/gift/gift_test.go", "game/handler/add_exp_test.go", "game/controllers/alliance/refresh_test.go"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(rel))); err != nil {
			t.Errorf("demo did not write %s: %v", rel, err)
		}
	}
	// 邮件是 mailbox 模块装进来的（demoModuleSteps，连同它依赖的 wallet）：demo 不带自己的 ListMail / ClaimMail、
	// ClaimMailReward handler、领取账本与附件编码，也不覆盖模块的任何文件。
	for _, gone := range []string{"protocol/def/list_mail.go", "protocol/def/claim_mail.go",
		"game/controllers/player/list_mail.go", "game/controllers/player/claim_mail.go",
		"game/handler/claim_mail_reward.go", "game/handler/claim_mail_reward_test.go", "game/handler/claim_mail_expiry_test.go",
		"internal/errors/mail_claim.go"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("the demo still ships its own mail file %s (err=%v)", gone, err)
		}
	}
	for _, owned := range []string{"    mailbox:\n", "    wallet:\n", "- PlayerDao.MailboxLedger", "- PlayerDao.Wallet", "MailboxList:", "MailboxClaim:", "WalletSpend:",
		"mailbox_attachment:", "- game/mailbox/mailbox_send.go", "- game/controllers/mailbox/mailbox_enter.go", "- game/mailbox/mailbox_item.go", "- game/entities/player/mailbox_item.go"} {
		if !strings.Contains(manifestText, owned) {
			t.Errorf("roost.yaml does not record %q under the mailbox / wallet modules", owned)
		}
	}
	if bind := read("game/protocol_handlers/mailbox/protocol_gen.go"); !strings.Contains(bind, "HandleMailboxList") || !strings.Contains(bind, "HandleMailboxClaim") {
		t.Errorf("the mailbox protocols are not bound to the mailbox controller:\n%s", bind)
	}
	// The grant in the middle carries its own identity: the mail id is
	// recorded on the Player in the same transaction as the reward, so a
	// retry after a lost CommitClaim cannot grant a second stack. The
	// ledger's memory is bounded by the MAIL's own expiry, which the mail
	// service hands over with the reservation — not by a retention this game
	// invented against another service's configuration (RR-20260918-05).
	// All three are the module's files, as the module ships them.
	if claim := read("game/controllers/mailbox/mailbox_claim.go"); !strings.Contains(claim, ".ReserveClaim(") || !strings.Contains(claim, ".Sync_MailboxClaimReward(") || !strings.Contains(claim, ".CommitClaim(") || !strings.Contains(claim, "claim.ExpiresAtUnix") {
		t.Errorf("mailbox_claim does not run the reserve → ledgered grant → commit sequence with the mail's own expiry:\n%s", claim)
	}
	if claim := read("game/handler/mailbox_claim_reward.go"); !strings.Contains(claim, "MailboxComp().Claim(mailID, attachment, expiresAtUnix,") || !strings.Contains(claim, "rollback=undo durability=strict") {
		t.Errorf("the mail claim handler does not record the mail id in the granting transaction:\n%s", claim)
	}
	if component := read("game/entities/player/mailbox_component.go"); !strings.Contains(component, "ledger.SetClaimsKey(mailID, expiresAtUnix)") || !strings.Contains(component, "gamemailbox.LedgerMayForget(recordedExpiry, nowUnix)") {
		t.Errorf("the Mailbox component does not keep the claim ledger by the mail's own expiry:\n%s", component)
	}
	if dao := read("db/def/player.go"); !strings.Contains(dao, "MailboxLedger MailboxLedger `store:\"mailbox_ledger\" dao:\"persist\"`") || strings.Contains(dao, "MailClaims") ||
		!strings.Contains(dao, "Wallet WalletState `store:\"wallet\" dao:\"persist,sync\"`") {
		t.Errorf("PlayerDao does not carry the mailbox module's ledger and the wallet module's state (or still has the demo's MailClaims):\n%s", dao)
	}
	// 道具附件靠 mailbox 与 bag 的联动进背包：demo 的奖励邮件带的都是道具。
	if glue := read("game/entities/player/mailbox_item.go"); !strings.Contains(glue, "RegisterMailboxGrant(gamemailbox.TypeItem") || !strings.Contains(glue, "owner.BagComp().Add(attachment.ID, attachment.Amount)") {
		t.Errorf("the mailbox+bag glue was not written into the demo:\n%s", glue)
	}
	// 发件方都走模块的 SendTo，附件是模块的编码；升级奖励的幂等键是 EffectID（见下面事件链那一段）。
	for rel, call := range map[string]string{
		"game/gm/mailbox.go":                 "Attachment: payload.Attachment",
		"internal/service/game/activity.go":  "gamemailbox.Item(reward.ItemID, int64(reward.Count))",
		"internal/service/game/gift_saga.go": "gamemailbox.Item(state.ItemID, int64(state.Count))",
	} {
		if body := read(rel); !strings.Contains(body, "gamemailbox.SendTo(") || !strings.Contains(body, call) || strings.Contains(body, "svcmail.SendRequest{") {
			t.Errorf("%s does not send through the mailbox module's SendTo with the module's attachment (%s)", rel, call)
		}
	}
	if reward := read("game/rewards/rewards.go"); !strings.Contains(reward, "func LevelUpReward(level int32) gamemailbox.Attachment") || strings.Contains(reward, "func Decode(") || strings.Contains(reward, "LedgerMayForget") {
		t.Errorf("game/rewards still carries its own attachment encoding or ledger rules:\n%s", reward)
	}
	// 进入游戏时拉全服邮件是 mailbox 与 enter 的联动（进入钩子），画像读登录端点带过去的会话 claims；
	// demo 的进入流程里不再手写 PullBroadcasts，player controller 也不再持有 mail 客户端。
	if glue := read("game/controllers/mailbox/mailbox_enter.go"); !strings.Contains(glue, `gameenter.OnEntered("mailbox"`) || !strings.Contains(glue, "svcaccount.RoleCreatedAtClaim") || !strings.Contains(glue, "svcaccount.OriginServerIDClaim") {
		t.Errorf("the mailbox+enter glue was not written into the demo:\n%s", glue)
	}
	if login := read("game/controllers/player/enter_game.go"); strings.Contains(login, "PullBroadcasts(") || strings.Contains(login, "func BroadcastProfile(") || !strings.Contains(login, "claims := context.Session.Principal().Claims") || !strings.Contains(login, "InitResult: entered, Claims: claims,") {
		t.Errorf("the demo's enter flow still pulls server-wide mail by hand, or does not hand the session claims to the enter hooks:\n%s", login)
	}
	if controller := read("game/controllers/player/controller.go"); strings.Contains(controller, "svcmail") {
		t.Errorf("the player controller still holds the mail client:\n%s", controller)
	}
	if auth := read("internal/access/player/tcp/login_claims_test.go"); !strings.Contains(auth, "controllermailbox.BroadcastProfileOnEnter(") {
		t.Errorf("the authenticator's test does not read its claims back through the mailbox enter hook's profile:\n%s", auth)
	}
	if robot := read("cmd/loadtest/main.go"); !strings.Contains(robot, "msgid.MsgMailboxList") || !strings.Contains(robot, "msgid.MsgMailboxClaim") || strings.Contains(robot, "ClaimMail") || strings.Contains(robot, "ListMail") {
		t.Errorf("the load-test robot does not read and claim mail through the module's MailboxList / MailboxClaim")
	}
	if spec := read("loadtest/scenarios/demo.yaml"); !strings.Contains(spec, "action: mailbox_list") || !strings.Contains(spec, "action: mailbox_claim_replay") || strings.Contains(spec, "action: claim_mail") {
		t.Errorf("the demo scenario does not walk level-up → reward mail → list → claim through the mailbox module:\n%s", spec)
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
	if conn := read("loadtest/playertcp/conn.go"); !strings.Contains(conn, "transport.DialPlayerTCP(ctx, cfg)") {
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
	if def := read("db/def/player.go"); !strings.Contains(def, "Equipment EquipmentState") || !strings.Contains(read("db/def/equipment.go"), "Slots map[int32]*GearPiece") {
		t.Errorf("the Player DAO has no nested struct field, so the demo never exercises two-level dirty propagation")
	}
	// 持久 schema 继续声明，用于加载前的严格版本校验。
	if def := read("db/def/player.go"); !strings.Contains(def, "schema=4") {
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
	// 离线入会再登录（联盟方案 §2.5B 决定 D）：机器人场景、它用的运维命令与 dev-smoke 的第二轮都在。
	if scenario := read("loadtest/scenarios/demo.yaml"); !strings.Contains(scenario, "name: alliance_offline_join") || !strings.Contains(scenario, "action: join_alliance_offline") || !strings.Contains(scenario, "action: my_alliance") {
		t.Errorf("the robot scenarios lack alliance_offline_join (a alliance joined while offline, seen at the next login)")
	}
	if gm := read("internal/service/game/gm.go"); !strings.Contains(gm, `"gm.player.join_alliance"`) {
		t.Errorf("the game GM lacks gm.player.join_alliance, the operator step alliance_offline_join runs")
	}
	if makefile := read("Makefile"); !strings.Contains(makefile, "-scenario alliance_offline_join") {
		t.Errorf("make dev-smoke does not run the alliance_offline_join scenario")
	}
	// 步骤消费者只有 wiring/saga Steps 一种写法（D12 方向 B 去掉了 definition.go 的 Subscribe* 辅助）；demo 的登记
	// 在 gift_saga.go，生成的单进程 steps.go 被换成指向它的说明（送礼的扣款 / 退款要按 sid 准入与转交）。
	if definition := read("saga/gift_item/definition.go"); strings.Contains(definition, "func Subscribe") || !strings.Contains(definition, "TopicDebitCompensation") {
		t.Errorf("generated saga definition must carry the step topics and no Subscribe* helpers:\n%s", definition)
	}
	if steps := read("saga/gift_item/steps.go"); strings.Contains(steps, "func StartSteps(") || !strings.Contains(steps, "gift_saga.go") {
		t.Errorf("the demo's saga/gift_item/steps.go must point at gift_saga.go instead of a second registration:\n%s", steps)
	}
	// Two step shapes, one per kind of work: the Nest-transaction steps bind
	// their receipt and emit their completion inside the transaction (the
	// native path), the mail step keeps the Mongo inbox and the mail
	// service's own RequestID dedupe.
	// The glue (inboxes, publisher, durables, drain) is the framework's step
	// runtime (D12 方向 A); the demo registers step bodies by name.
	if steps := read("internal/service/game/gift_saga.go"); !strings.Contains(steps, `runtime.Native("debit", giftitem.TopicDebit, steps.debit, kitsaga.WithSidRoute()`) || !strings.Contains(steps, "runtime.Mongo(\"deliver\", giftitem.TopicDeliver") || !strings.Contains(steps, "RequestID: \"gift:\" + command.IdempotencyKey") {
		t.Errorf("gift saga does not run the Nest steps natively while keeping the mail step on the Mongo inbox")
	}
	if steps := read("internal/service/game/gift_saga.go"); !strings.Contains(steps, "kitsaga.NewSteps(registry, giftSagaSteps)") || !strings.Contains(steps, "saga.BounceStep(saga.SidRoute(admission.SID)") || strings.Contains(steps, "saga.NewDataEngineStepInbox(") {
		t.Errorf("gift saga does not assemble its consumers through the framework's step runtime, or does not bounce a step for a sender bound elsewhere")
	}
	// 准入按玩家当前的绑定（rehome 模块）：换服中的退回不带路由，绑定别处的退回并给出当前 sid；
	// 载荷里的 sid 不再决定，旧的 playerMoving 钩子与它的 TODO 已经删掉。公会的玩家步骤走同一个函数。
	if steps := read("internal/service/game/gift_saga.go"); !strings.Contains(steps, "owners.AdmitBound(ctx, playerID)") || !strings.Contains(steps, `saga.BounceStep("", `) || strings.Contains(steps, "playerMoving") || strings.Contains(steps, "notMoving") {
		t.Errorf("gift saga does not admit its Player steps by the sender's current binding")
	}
	if steps := read("game/alliance/runtime/runtime.go"); !strings.Contains(steps, "alliance.Binding(ctx, registry, playerID, sid)") || strings.Contains(steps, "playerMoving") {
		t.Errorf("the alliance sagas do not admit their Player steps by the player's current binding")
	}
	// 驻留表实现并发布 rehome 模块的 Residency；GM 有换服命令；demo 自己的换服前检查在盘上。
	if owners := read("internal/service/game/playerowner.go"); !strings.Contains(owners, "var _ gamerehome.Residency = (*PlayerOwners)(nil)") || !strings.Contains(owners, "gamerehome.Binding(ctx, registry, playerID)") {
		t.Errorf("PlayerOwners is not the rehome module's Residency, or does not ask the current binding through the module")
	}
	if service := read("internal/service/game/service.go"); !strings.Contains(service, "registry.Register(gamerehome.ResidencyCapability, gamerehome.Residency(owners))") {
		t.Errorf("the game service does not publish its ownership table as the rehome module's Residency")
	}
	if gm := read("game/gm/rehome.go"); !strings.Contains(gm, `"gm.player.rehome"`) || !strings.Contains(gm, "rehome.Move(ctx, unique, payload.To,") {
		t.Errorf("the game GM lacks gm.player.rehome, or it does not go through the rehome module's server-side entry")
	}
	if checks := read("internal/service/game/rehome_checks.go"); !strings.Contains(checks, `gamerehome.OnApplying("game"`) {
		t.Errorf("the demo's own checks before a server change are not registered:\n%s", checks)
	}
	// The debit is routed to the sender's sid from the start (plan ③, 2026-11-11).
	if definition := read("saga/gift_item/definition.go"); !strings.Contains(definition, "CompensateTopic: TopicDebitCompensation, Routed: true}") || strings.Contains(definition, "TopicDeliverCompensation, Routed: true") {
		t.Errorf("the gift definition does not route exactly the debit by sid")
	}
	if start := read("game/handler/start_gift.go"); !strings.Contains(start, "Route: saga.SidRoute(fromSID)") {
		t.Errorf("StartGift does not route the saga to the sender's sid")
	}
	if debit := read("game/handler/gift_debit.go"); !strings.Contains(debit, "step.Complete(true, \"\")") || !strings.Contains(debit, "step.Complete(false, reason)") {
		t.Errorf("the debit handler does not commit its receipt and outcome in the transaction")
	}
	if debit := read("game/handler/gift_debit.go"); !strings.Contains(debit, "step saga.NativeStep)") {
		t.Errorf("the debit handler does not take the framework's native step")
	}
	if contract := read("game/gift/gift.go"); strings.Contains(contract, "NativeStep struct") {
		t.Errorf("the gift contract still carries its own native step; it is saga.NativeStep now")
	}
	// A step's topics come from the generated definition, so a durable and
	// its filter cannot drift apart.
	if definition := read("saga/gift_item/definition.go"); !strings.Contains(definition, "TopicDebit = ") || !strings.Contains(definition, "TopicDebitCompensation = ") {
		t.Errorf("the generated saga definition exposes no topic constants, so the native path has to repeat the strings")
	}
	// player_id takes either id an operator has at hand: the unique id the
	// client sees or the full entity id Mongo stores as _id. Re-wrapping a
	// full id produced an id no Player has (live run, 2026-09-17).
	if gm := read("internal/service/game/gm.go"); !strings.Contains(gm, "gamemodgm.PlayerIDs(payload.PlayerID, player.EntityKindPlayer)") {
		t.Errorf("gm.go does not use the module PlayerIDs normalizer")
	}
	if scrape := read("deploy/dev/observability/prometheus.yml"); !strings.Contains(scrape, "job_name: chat") || !strings.Contains(scrape, ":9102") {
		t.Errorf("prometheus.yml does not scrape the chat process on its assigned port")
	}
	// account 的协作者是 login 模块的：玩家号计数器经 RegistryBound 拿 account 进程的 Redis。
	if collaborators := read("internal/service/account/collaborators.go"); !strings.Contains(collaborators, "&playerIDCounter{}") || strings.Contains(collaborators, "is not configured; implement") {
		t.Errorf("account collaborators are still the refusing defaults:\n%s", collaborators)
	}
	if counter := read("internal/service/account/login_player_id.go"); !strings.Contains(counter, "wiringaccount.RegistryBound") {
		t.Errorf("the player id counter does not bind to the account process's registry:\n%s", counter)
	}
	// The authenticator validates tickets through the account client it is
	// bound to in Provide; the generated transport must expose the hook.
	if auth := read("internal/access/player/tcp/auth.go"); !strings.Contains(auth, "BindRegistry(") {
		t.Errorf("auth.go does not bind an account client:\n%s", auth)
	}
	if rules := read("game/login/login.go"); !strings.Contains(rules, "ValidateSession(") || !strings.Contains(rules, "EnterServer(") {
		t.Errorf("game/login/login.go does not validate both kinds of ticket through account:\n%s", rules)
	}
	if server := read("internal/access/player/tcp/server_gen.go"); !strings.Contains(server, "type RegistryBound interface") {
		t.Errorf("the generated transport lost the RegistryBound hook the demo authenticator relies on")
	}
	// 升级邮件与属性由各自模块注册，消费者属于 mailbox 的模块生命周期。
	if hook := read("game/entities/player/mailbox_profile.go"); !strings.Contains(hook, "OnProfileLevelUp(") || !strings.Contains(hook, "mailbox.EmitProfileReward(") || !strings.Contains(read("game/entities/player/attribute_component.go"), "OnProfileLevelUp(") {
		t.Errorf("module profile hooks do not register both attribute and mailbox")
	}
	if mailer := read("game/mailbox/mailbox_profile.go"); !strings.Contains(mailer, "kitdataengine.SubscribeEffects(") || !strings.Contains(mailer, "RequestID: envelope.EffectID") || !strings.Contains(mailer, "errors.Is(err, ErrLetterInvalid)") {
		t.Errorf("module reward mail consumer lost durable/idempotent handling: %s", mailer)
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
	// Alliances live in the alliance service (FEATURE-2026-11-04-alliance-service
	// §2.6): the game reads alliances through the mirrors and changes them only
	// through the alliance RPC. It keeps remote_entity for the partitioned
	// scene's Team, the one Remote kind it owns (FEATURE-2026-11-03-seamless-world
	// §14); the alliance kinds are foreign to its Remote runtime.
	demoManifest, err := LoadManifest(target)
	if err != nil {
		t.Fatal(err)
	}
	gameSpec := demoManifest.Services["game"]
	if !contains(gameSpec.Mods, "remote_entity") || contains(gameSpec.Mods, "remote_mirror") || !contains(gameSpec.UsesRpcs, "alliance") {
		t.Errorf("game mods %v, uses_rpcs %v: want remote_entity (for its Team) without remote_mirror, and the alliance RPC", gameSpec.Mods, gameSpec.UsesRpcs)
	}
	if ownership := read("internal/registry/ownership_gen.go"); !strings.Contains(ownership, "alliance.EntityKindAlliance") || !strings.Contains(ownership, "team.EntityKindTeam") {
		t.Errorf("ownership does not split the alliance kinds and the team between the two services:\n%s", ownership)
	}
	if alliance := read("game/controllers/alliance/alliance.go"); !strings.Contains(alliance, "controller.alliances.Join(") || !strings.Contains(alliance, "entity.RemoteReadMonotonic") {
		t.Errorf("alliance endpoints do not go through the alliance RPC and the mirrors:\n%s", alliance)
	}
	for _, gone := range []string{"game/entities/guild", "db/def/guild.go", "game/controllers/player/guild_ids.go", "game/handler/join_guild.go"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("%s still generated (err=%v): the game-local alliance is gone", gone, err)
		}
	}
	// EnterGame 的协议与初始化标记来自 enter 模块：端点绑在 enter 域上，demo 用自己的进入流程覆盖模块的 controller。
	if bind := read("game/protocol_handlers/enter/protocol_gen.go"); !strings.Contains(bind, "HandleEnterGame") {
		t.Errorf("EnterGame is not bound to the enter controller:\n%s", bind)
	}
	if enter := read("game/controllers/enter/enter_game.go"); !strings.Contains(enter, "player.EnterGame(context, request)") {
		t.Errorf("the enter endpoint does not hand over to the demo's own enter flow:\n%s", enter)
	}
	// 新建请求（EnterCreate）同样交给 player controller：建档之前先核对归属、接入驻留表；
	// 会话激活后的告知（enter_status.go）是模块的，demo 不覆盖。
	if create := read("game/controllers/enter/enter_create.go"); !strings.Contains(create, "player.EnterCreate(context, request)") {
		t.Errorf("the create endpoint does not hand over to the demo's own flow:\n%s", create)
	}
	if create := read("game/controllers/player/enter_create.go"); !strings.Contains(create, "controller.boundHere(context, \"enter_create\")") ||
		!strings.Contains(create, "gameenter.Create(") || !strings.Contains(create, "owners.Serve(loginCtx, context.PlayerID, boundSID)") {
		t.Errorf("the demo's EnterCreate does not check the binding, take the player into service and run the module's Create:\n%s", create)
	}
	if status := read("game/controllers/enter/enter_status.go"); !strings.Contains(status, "func Announce(") || !strings.Contains(status, "gameenter.Assess(") {
		t.Errorf("enter_status.go is not the enter module's notice:\n%s", status)
	}
	if gate := read("internal/service/game/playerowner.go"); !strings.Contains(gate, "messageID == msgid.MsgEnterGame || messageID == msgid.MsgEnterCreate") {
		t.Errorf("the write gate does not exempt the create request beside the login:\n%s", gate)
	}
	flow := read("game/controllers/player/enter_game.go")
	if !strings.Contains(flow, "Sync_EnterInit(") {
		t.Errorf("the demo's enter flow does not run the enter module's first-time init:\n%s", flow)
	}
	// rehome 与 enter 都装着：联动登记了进入前检查与到达。demo 的进入流程在把玩家接入服务（Serve）、装载实体之前
	// 跑检查，并把拒绝里的目标服带进应答。
	check, serve := strings.Index(flow, "gameenter.RunEntering("), strings.Index(flow, "owners.Serve(")
	if check < 0 || serve < 0 || check > serve || !strings.Contains(flow, "gamerehome.OwnerSID(err)") {
		t.Errorf("the demo's enter flow does not run the checks before entering ahead of taking the player into service:\n%s", flow)
	}
	if glue := read("game/controllers/rehome/rehome_enter.go"); !strings.Contains(glue, `gameenter.OnEntering("rehome"`) || !strings.Contains(glue, `gameenter.OnEntered("rehome"`) {
		t.Errorf("the rehome+enter glue was not written into the demo:\n%s", glue)
	}
	// enter 与 chat 都装着：chat 的联动文件登记了进入钩子，demo 的进入流程执行钩子而不是自己加在线名单。
	if !strings.Contains(flow, "gameenter.RunEntered(") || strings.Contains(flow, "Presence().Add(") {
		t.Errorf("the demo's enter flow does not go through the enter hooks:\n%s", flow)
	}
	if glue := read("game/chat/chat_enter.go"); !strings.Contains(glue, `gameenter.OnEntered("chat"`) {
		t.Errorf("the chat+enter glue was not written into the demo:\n%s", glue)
	}
	// 排行榜是 leaderboard 模块装进来的：demo 不带自己的 game/ranking 与 RankTop；模块的逻辑文件原样，
	// demo 只覆盖业务文件（通关榜）；副本结算在事务之外调模块的提交，压测机器人读模块的协议。
	for _, gone := range []string{"game/ranking/ranking.go", "protocol/def/rank_top.go", "game/controllers/player/rank_top.go"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("the demo still ships its own leaderboard file %s (err=%v)", gone, err)
		}
	}
	if board := read("game/leaderboard/leaderboard_board.go"); !strings.Contains(board, `BoardID = "clears"`) || !strings.Contains(board, "func CurrentSeason() int64") {
		t.Errorf("the leaderboard module's board file is not the demo's clears board:\n%s", board)
	}
	if logic := read("game/leaderboard/leaderboard.go"); !strings.Contains(logic, "func SubmitSeason(") || strings.Contains(logic, `"clears"`) {
		t.Errorf("game/leaderboard/leaderboard.go is not the module's own logic file:\n%s", logic)
	}
	if bind := read("game/protocol_handlers/leaderboard/protocol_gen.go"); !strings.Contains(bind, "HandleLeaderboardTop") || !strings.Contains(bind, "HandleLeaderboardMe") {
		t.Errorf("the leaderboard protocols are not bound to the leaderboard controller:\n%s", bind)
	}
	if finish := read("game/controllers/player/finish_dungeon.go"); !strings.Contains(finish, "gameleaderboard.SubmitSeason(") || strings.Contains(finish, "svcrank") {
		t.Errorf("the dungeon settlement does not submit through the leaderboard module:\n%s", finish)
	}
	// 档案是 profile 模块装进来的：demo 不带自己的档案组件、PlayerLevel 读 handler 与 name_empty 错误码；模块的组件文件原样
	// （不发 effect、不碰属性），demo 只覆盖升级钩子；名字、等级、经验不再是 PlayerDao 的平铺字段，而是模块追加的两个
	// 顶层字段（Profile 别人可见、ProfilePrivate 只给本人）；公会副本与副本领奖账本在 demo 自己的文件里。
	for _, gone := range []string{"game/handler/player_level.go", "internal/errors/name_empty.go"} {
		if _, err := os.Stat(filepath.Join(target, filepath.FromSlash(gone))); !os.IsNotExist(err) {
			t.Errorf("the demo still ships its own profile file %s (err=%v)", gone, err)
		}
	}
	if component := read("game/entities/player/profile_component.go"); !strings.Contains(component, "func (component *ProfileComponent) CheckAddExp(") ||
		strings.Contains(component, "effects.") || strings.Contains(component, "AttributeComp()") || strings.Contains(component, "AllianceID") || strings.Contains(component, "Dungeon") {
		t.Errorf("game/entities/player/profile_component.go is not the module's own component file:\n%s", component)
	}
	if dao := read("db/def/player.go"); !strings.Contains(dao, "Profile ProfileState `store:\"profile\" dao:\"persist,sync\"`") ||
		!strings.Contains(dao, "ProfilePrivate ProfilePrivateState `store:\"profile_private\" dao:\"persist,sync\"`") ||
		strings.Contains(dao, `store:"name"`) || strings.Contains(dao, `store:"exp"`) || strings.Contains(dao, `store:"gold"`) {
		t.Errorf("PlayerDao does not take its name, level and exp from the profile module's two fields:\n%s", dao)
	}
	if packer := read("game/entities/player/sync_packer.go"); !strings.Contains(packer, `playerPublicFields = []string{"Profile", `) || strings.Contains(packer, `playerPublicFields = []string{"Profile", "ProfilePrivate"`) {
		t.Errorf("the view other players get does not list Profile without ProfilePrivate:\n%s", packer)
	}
	if alliance := read("game/entities/player/alliance_component.go"); !strings.Contains(alliance, "func (c *AllianceComponent) SetAlliance(") {
		t.Errorf("the alliance copy is not provided by its module:\n%s", alliance)
	}
	if ledger := read("game/entities/player/dungeon_ledger.go"); !strings.Contains(ledger, "func (component *ProfileComponent) ClaimDungeonRun(") {
		t.Errorf("the dungeon claim ledger is not in the demo's own file:\n%s", ledger)
	}
	if queue := read("game/controllers/player/join_queue.go"); !strings.Contains(queue, "Sync_ProfileCard(") {
		t.Errorf("the ranked queue does not read the level through the profile module's ProfileCard:\n%s", queue)
	}
	if robot := read("cmd/loadtest/main.go"); !strings.Contains(robot, "msgid.MsgLeaderboardTop") || strings.Contains(robot, "RankTop") {
		t.Errorf("the load-test robot does not read the board through the module's LeaderboardTop")
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
	if operator := read("cmd/accountctl/main.go"); !strings.Contains(operator, ".UpsertServer(") || !strings.Contains(operator, `"roost:planet:account"`) || !strings.Contains(operator, "func registerCommand(") {
		t.Errorf("accountctl is not the login module's command on the project's account store:\n%s", operator)
	}
	// demo 多出来的四个命令经模块的 registerCommand 登记在另一个文件里，模块的 main.go 没有被覆盖。
	if extra := read("cmd/accountctl/operator.go"); !strings.Contains(extra, `registerCommand("merge-server"`) || !strings.Contains(extra, `registerCommand("unmerge-server"`) ||
		!strings.Contains(extra, `registerCommand("show-role"`) || !strings.Contains(extra, `registerCommand("transfer-role"`) {
		t.Errorf("the demo's operator commands are not registered beside the module's:\n%s", extra)
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
	raw, err := os.ReadFile(filepath.Join(target, "internal", "service", "arena", "gift_saga.go"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"package " + defaultServicePackage("arena"), `"arena-gift"`} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("gift_saga.go lacks %q:\n%s", want, raw)
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
				// 只属于开发配置的两段（demoLoginDevConfig）：开发登录令牌在生产配置里写了 account 进程不启动；
				// 区服地址是部署的事。两段都可以不写，缺了它们进程照常启动。
				if service == "account" && (key == "login.dev_token" || strings.HasPrefix(key, "portal.servers.")) {
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
//
//go:embed testdata/gate/real_test.go.txt
var gateRealSupportTest string

//go:embed testdata/gate/consumer_test.go.txt
var gateConsumerTest string

//go:embed testdata/gate/load_test.go.txt
var gateLoadTest string

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
	if err = os.WriteFile(filepath.Join(root, "internal/service/game/gate_real_test.go"), []byte(gateRealSupportTest), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "internal/service/game/gate_load_test.go"), []byte(gateLoadTest), 0644); err != nil {
		t.Fatal(err)
	}
	bootstrapTest := `package bootstrap
import (
	"testing"

	kitetcd "github.com/tjbdwanghaibo/roost-core/wiring/etcd"
	gatewiring "github.com/tjbdwanghaibo/roost-core/wiring/gate"
	playertcp "example.com/planet/internal/access/player/tcp"
)
func TestGeneratedGateTransportSelection(t *testing.T) {
 local := playertcp.NewMod()
 if playerTransport(Options{},local) != local { t.Fatal("default changed from local TCP") }
 options := gatewiring.DefaultOptions()
 if _,ok := playerTransport(Options{GameIngress:&options},local).(*playertcp.IngressMod); !ok { t.Fatal("Gate option did not select Game Ingress") }
 discovery := kitetcd.NewEtcdMod()
 if playerDiscovery(Options{},discovery) != discovery || playerDiscovery(Options{GameIngress:&options},discovery) != discovery { t.Fatal("the etcd Mod changed although Game Ingress does not register discovery") }
 options.RegisterDiscovery = true
 if playerDiscovery(Options{GameIngress:&options},discovery) == discovery { t.Fatal("Game Ingress registers discovery but the etcd Mod still registers the service") }
}
`
	if err := os.WriteFile(filepath.Join(root, "internal/bootstrap/gate_transport_test.go"), []byte(bootstrapTest), 0644); err != nil {
		t.Fatal(err)
	}
	var clientDLL string
	if os.Getenv("ROOST_GATE_DOTNET") == "1" {
		args := []string{"-def", filepath.Join(root, "protocol/def"), "-proto", filepath.Join(root, "protocol/proto"), "-pb", filepath.Join(root, "protocol/pb"), "-msgid", filepath.Join(root, "protocol/msgid"), "-manifest", filepath.Join(root, "protocol/protocol_manifest.json"), "-bind", "", "-handlers", "", "-robot-protocol", "", "-csharp", filepath.Join(root, "client/MessageIds.cs")}
		if err := protocol.Run(args, io.Discard); err != nil {
			t.Fatal(err)
		}
		outputDir := t.TempDir()
		build := exec.Command("dotnet", "build", filepath.Join(repo, "client/dotnet/Roost.Client.Demo"), "-p:DemoRoot="+root, "-o", outputDir)
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("build generated C# client: %v\n%s", err, output)
		}
		clientDLL = filepath.Join(outputDir, "Roost.Client.Demo.dll")
	}
	var nativeExecutable string
	if os.Getenv("ROOST_GATE_CPP") == "1" {
		buildDir := t.TempDir()
		configure := exec.Command("cmake", "-S", filepath.Join(repo, "client/cpp"), "-B", buildDir, "-DROOST_DEMO_ROOT="+root, "-DCMAKE_PREFIX_PATH="+os.Getenv("ROOST_PROTOBUF_PREFIX"))
		if output, err := configure.CombinedOutput(); err != nil {
			t.Fatalf("native client configure: %v\n%s", err, output)
		}
		build := exec.Command("cmake", "--build", buildDir, "--target", "roost_gate_client", "--parallel", "4")
		if output, err := build.CombinedOutput(); err != nil {
			t.Fatalf("native client build: %v\n%s", err, output)
		}
		nativeExecutable = filepath.Join(buildDir, "roost_gate_client")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	args := []string{"test", "-mod=mod", "-count=1", "-v", "-run", "^TestGeneratedGate(NestDAOAndWAL|TransportSelection|ProjectionCapabilities)$", "./internal/service/game", "./internal/bootstrap"}
	if os.Getenv("ROOST_GATE_LOAD_SECONDS") == "" {
		args = append(args, "-race")
	}
	cmd := exec.CommandContext(ctx, "go", args...)
	cmd.Dir = root
	cmd.Env = append(os.Environ(), "GOWORK=off", "ROOST_GATE_DOTNET_DLL="+clientDLL, "ROOST_GATE_NATIVE_EXE="+nativeExecutable)
	output, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("generated Gate business: %v\n%s", err, output)
	}
	t.Log(string(output))
}
