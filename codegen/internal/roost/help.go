package roost

import (
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
)

type helpTopic struct {
	Name          string
	Aliases       []string
	Summary       string
	Usage         string
	Configuration string
	Example       string
}

var helpTopics = []helpTopic{
	{
		Name: "environment", Aliases: []string{"env", "install", "version"},
		Summary:       "检查本机工具、PATH 与当前 roost-codegen 版本",
		Usage:         "roost version\nroost env doctor",
		Configuration: "go 和 git 是必需项；make 和 docker 在使用生成 Makefile、开发依赖或容器部署时需要。检查失败会直接指出缺少的 PATH 工具。",
		Example:       "go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest\nroost version\nroost env doctor",
	},
	{
		Name: "beginner", Aliases: []string{"start", "quickstart", "newbie"},
		Summary: "从空目录到第一条玩家协议、Nest 与 Entity 业务链",
		Usage: `roost project new hello_roost -module example.com/hello_roost -mods configdata
cd hello_roost
roost project next
roost add access player --service game
roost add entity Player
roost add component Profile --entity Player
roost add dao Player --entity Player
roost add handler RenamePlayer --entity Player --component Profile
roost add protocol RenamePlayer --group game --handler player
# 编辑 Request 字段和 handler 参数后：
roost add endpoint RenamePlayer --handler player
roost add lifecycle Player --service game
roost generate
roost project doctor --workflow first-business
# 每完成一步都可重新运行：roost project next
# 需要框架自带 TCP 时，首条业务完成后：
roost add transport tcp
# 实现 auth.go 后：roost config enable player-tcp
make dev-up
make run SERVICE=game`,
		Configuration: `不需要先记住全部命令：每完成一步运行 roost project next，它只显示当前一步、原因、复查命令和对应文档。access player 生成传输无关的协议边界；Entity 是锁与生命周期边界；Component 写玩法方法；持久化字段放 DAO 并只通过生成 mutator 修改。完整逐步说明见 docs/BEGINNER_WORKBOOK.zh-CN.md。`,
		Example: `// profile_component.go
func (component *ProfileComponent) Rename(name string) error {
    component.Owner().Dao().SetName(name)
    return nil
}

// game/handler/profile.go
//roost:nest rollback=undo durability=strict
func handlerRenamePlayer(target player.IProfileEntity, name string) error {
    return target.ProfileComp().Rename(name)
}`,
	},
	{
		Name: "project", Aliases: []string{"project-new", "project-sync", "project-diff", "project-doctor", "project-next", "project-upgrade", "upgrade-project"},
		Summary: "创建、同步、检查和升级完整 Roost 工程",
		Usage: `roost project new <name> -module <go-module> [-out dir] [-services a,b] [-mods a,b] [-features a,b] [-template game|game-demo]
roost project sync|diff|doctor|next [--root dir]
roost project next [--workflow first-business|player-tcp]
roost project doctor --workflow first-business
roost project upgrade [--root dir] [--dry-run] [--skip-deps] [-core version]
make project-upgrade`,
		Configuration: `-module 必填，防止项目意外使用框架作者的仓库命名空间。默认 -out=<name>、-services=game、features=protocol,config,entity,nest,dao,errcode；不传 -mods 时启用除 saga 外的生产基础 Mod，新手建议显式使用 -mods configdata。-template game 是可选的起步形态：account、chat、mail、match、session、activity、global、platform、rank 九个框架服务各作为独立子命令托管（services.<name>.framework），第一个业务 Service 通过 services.<name>.uses 拿到它们的 ClientMod 与类型化访问器，并带 nest 运行时、Player 与 World 两个 Entity 及其 lifecycle；World 是进程内单例（game/lifecycle/world_singleton.go 的 EnsureWorld，Service.Init 里确保存在）；托管服务的协作者（身份校验、投递、频道策略……）在 internal/service/<name>/collaborators.go 里，默认全部拒绝，需要项目自己实现。-template game-demo 在 game 之上再生成一条可运行的写入链路：Player 带 Profile / Bag 两个组件与 PlayerDao 持久化字段，一个 //roost:nest rollback=undo durability=strict 的加道具事务，以及 player TCP 接入与对应协议 / 端点。组件方法、DAO 字段、handler、协议定义和 auth.go 都是业务文件：生成一次，之后 sync / upgrade 不再改写，直接在上面改。其中 game-demo 的 auth.go 只认 session:<id>:<token> 并通过 account 校验，普通接入骨架默认拒绝；上线前须实现身份协作者。demo 源码在本仓 demo/ 目录，按生成后的路径原样存放。project next 根据项目真实文件计算进度，只给一个当前动作，不会替业务决定字段或生成 preset；默认先完成 first-business，再进入可选 player-tcp。项目声明位于 roost.yaml。sync 在同级临时副本完成全部生成和所有权预检后提交；失败回滚已写文件，并拒绝覆盖提交期间发生的并发修改。upgrade 只覆盖 codegen 受控文件，以及应用配置里未手改、随 Mod 计划刷新的生成 shutdown: 块；project diff 与 upgrade --dry-run 会列出这些文件（生成器输出用 roost generate --check 检查）。`,
		Example: `roost project new planet -module example.com/planet -services game,gate
cd planet
roost project next

# 想先看一条跑通的写入链路，再照着改：
roost project new planet -module example.com/planet -mods configdata,mongo,nats,dataengine,nest -template game-demo

# 旧项目执行一次，升级后即可使用 make project-upgrade
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest project upgrade --root . --dry-run -core latest
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest project upgrade --root . -core latest`,
	},
	{
		Name: "versions", Aliases: []string{"deps", "dependency", "dependencies", "project-deps", "roost-up", "codegen-up"},
		Summary: "持续跟随 roost-core 最新发布版本（wiring、skill、生成器都在其中）",
		Usage: `roost project deps [--root dir]
make deps-update
make roost-up
make codegen-up
roost project upgrade -core latest`,
		Configuration: fmt.Sprintf(`roost.yaml 的 versions.* 默认是 latest。deps-update 在临时项目解析 roost-core，只提交最终 go.mod/go.sum；失败或并发变化不会覆盖原文件。roost-up 执行 GOWORK=off go get -u ./... 与 go mod tidy，更新所有被项目引用的依赖；codegen-up 执行 go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest，更新本机 CLI。go.mod 保存具体版本，roost.yaml 保存更新策略。生成器是 roost-core 的一部分，Makefile 按 versions.core 运行它（versions.codegen 已退役，写了会被拒绝）。兼容下限：core %s；versions.kit / skill / service / codegen 已退役，写了会被拒绝。明确版本表示 MVS 下限，不是上限。`, minimumVersions.Core),
		Example: `versions:
  core: latest

make deps-update
make roost-up
make codegen-up`,
	},
	{
		Name: "framework-release", Aliases: []string{"framework", "release-train", "framework-lock"},
		Summary: "校验 roost-core 发布版本并生成不可变框架锁",
		Usage: `roost framework verify --manifest codegen/ci/framework-release.yaml --expected-release v1.16.0 --lock framework-lock.json
roost framework verify --github-output "$GITHUB_OUTPUT"`,
		Configuration: `codegen/ci/framework-release.yaml 只能使用 vMAJOR.MINOR.PATCH，不能使用 latest 或伪版本。它只有一行 release：一个模块一个版本。verify 从 Go module proxy 获取那一个已发布模块，校验 module path、checksum、replace 和框架内部伪版本依赖，然后生成 framework-lock.json；scripts/pretag.sh 在打 tag 之前先比对清单与要发的版本。release workflow 只有在完整生成项目兼容矩阵通过后才进入受保护 framework-release Environment。`,
		Example: `schema: 3
release: v1.16.0
consumer_go: [1.27.x]

roost framework verify --expected-release v1.16.0`,
	},
	{
		Name: "generate", Aliases: []string{"gen"},
		Summary:       "按依赖顺序运行项目启用的全部生成器",
		Usage:         `roost generate [--changed] [--dry-run] [--check] [--force] [--root dir]`,
		Configuration: `执行顺序为 DAO → Event → Errcode → Protocol → Entity → Nest → Attribute → Config → WebRoute。普通生成在同级临时项目完成全部生成器和 GOWORK=off go mod tidy，全部成功后才以可回滚批次提交生成物与 go.mod/go.sum；不会主动升级框架，也不会因后置生成器失败留下前置生成物。--check 在临时副本生成并检查差异；--changed 根据原项目 git 变更缩小范围。`,
		Example: `roost generate --dry-run
roost generate
roost generate --check`,
	},
	{
		Name: "add", Aliases: []string{"scaffold"},
		Summary:       "生成业务骨架并分配需要的 ID",
		Usage:         `roost add <service|mod|access|transport|module|protocol|entity|component|handler|lifecycle|endpoint|skill|table|dao|webroute|errcode|saga|effect|rpc|timer|attribute> <name> [flags]`,
		Configuration: `通用参数：--root、--service、--mods、--steps、--native、--consumer、--group、--id；saga 用 --steps 按执行顺序列步骤、--native 选出业务是 Nest 事务的步骤（见 roost help saga）；effect 用 --consumer 选消费服务（见 roost help effect）；Component/DAO 可用 --entity 自动接入所属 Entity；protocol 用 --handler 选择 controller domain；endpoint 用 --handler、--protocol、--nest-handler 接线；handler 使用 --entity 和 --component 生成正确 Nest 锁目标。骨架文件归业务所有，不会被 project sync 覆盖；创建后运行 roost generate。`,
		Example: `roost add entity player
roost add component profile --entity player
roost add dao player --entity player
roost add mod nest -service game
roost add handler rename_player --entity player --component profile
roost add access player --service game
roost add protocol rename_player -group game --handler player
roost add endpoint rename_player --handler player
roost add lifecycle player
roost add skill fireball
roost add timer --entity player
roost add saga gift -service game -steps debit,credit -native debit,credit -entity player -component wallet
roost add effect player.level_up -consumer game`,
	},
	{
		Name: "access", Aliases: []string{"player-access", "gateway-access"},
		Summary: "生成已认证玩家协议边界并装配到指定 Service",
		Usage: `roost add access player --service <service>
roost generate`,
		Configuration: `当前 access 名仅支持 player。它写入 roost.yaml access.player.service，启用 protocol/nest，并给目标 Service 补 Nest 运行依赖；生成并发安全的 ProtocolRegistry、typed binding 聚合和 access.player Mod。用 add transport tcp 生成生产约束的应用接入适配器。玩家要收到自己 Player 的状态变化（金币、道具等 sync 字段）时，在 roost.yaml 打开 access.player.self_sync: true（需要 tcp 传输），再 roost project sync，给客户端要看的 DAO 字段写 dao:"persist,sync" 即可：Player 标记缺省按 sync=true，没写 subjectPacker= 时生成器写默认 packer game/entities/player/player_sync_packer_gen.go（视图 self 包含 DAO 全部 sync 字段，即标了 sync 的字段默认全部对玩家本人可见）；要裁剪字段或别的视图时自己写 packer 并在标记写 subjectPacker=。用法与自定义 packer 样例见 ` + selfSyncDocumentURL + `。`,
		Example: `roost add access player --service game
roost add protocol RenamePlayer --group game --handler player
roost add endpoint RenamePlayer --handler player`,
	},
	{
		Name: "transport", Aliases: []string{"tcp", "player-tcp", "gateway-tcp"},
		Summary: "为 player access 生成有界 TCP listener、帧协议和 fail-closed 鉴权边界",
		Usage: `roost add transport tcp [--service <service>]
roost project doctor --workflow player-tcp`,
		Configuration: `要求先 add access player。生成代码提供 16 字节有界帧、全局/单 IP 连接上限、独立 handshake 并发与字节上限、handshake/idle/write timeout、sequence 防重放、每连接有界出站队列（停读客户端积压超过上限或写超时即断开，不拖住其他连接）、TCP keepalive、优雅关停，以及 PushPlayer/PushSession 主动发布；auth.go 归应用所有且默认拒绝。完整 disabled 配置会自动写入，完成鉴权后用 config enable player-tcp 开启。ROOST_PLAYER_TOKEN 环境变量可供 cmd/playerprobe 做最小鉴权验收；生产鉴权仍需安全评审。Docker/Kubernetes 模板会发布 7000，Kubernetes 仅允许带 player-access 标签的来源 namespace。`,
		Example: `roost add access player --service game
roost add transport tcp
# 实现 internal/access/player/tcp/auth.go
roost config enable player-tcp
roost project doctor --workflow player-tcp
# 安全地设置 ROOST_PLAYER_TOKEN 后：go run ./cmd/playerprobe -addr 127.0.0.1:7000`,
	},
	{
		Name: "lifecycle", Aliases: []string{"entity-lifecycle"},
		Summary:       "生成 Entity 登录加载、首次创建和销毁边界",
		Usage:         `roost add lifecycle <entity> [--entity <entity>] [--service <service>]`,
		Configuration: `生成 Get/Create/GetOrCreate/Destroy 和 <Entity>FromRegistry（每个 Entity 一个，同包不重名）。它通过实例级 entity.runtime 与 Data Engine loader 工作；普通玩法读写仍走 Nest Sender。GetOrCreate 只把确切的未找到视为可创建，不吞数据库或解码错误。`,
		Example: `roost add lifecycle Player --service game
# 应用初始化：lifecycle.PlayerFromRegistry(registry)`,
	},
	{
		Name: "endpoint", Aliases: []string{"protocol-endpoint", "protocol-to-nest"},
		Summary:       "把 typed 玩家协议映射到生成的 Nest Sender",
		Usage:         `roost add endpoint <name> --handler <controller-domain> [--protocol <name>] [--nest-handler <name> | --lifecycle <entity>]`,
		Configuration: `要求 access.player、已有 protocol 和 Nest handler。第一个 Nest 参数是由 context.PlayerID 定位的 Entity；其余命名参数按名称映射到 Request 字段；业务时间由服务器决定、不从请求取：参数名 nowUnix / nowUnixMilli（int64）由 endpoint 用 app.BusinessClock(controller.Registry()).Now() 填入（含 time.logic_offset）。controller 从 Registry 注入 nest.Client，不直接访问 EntityManager。--lifecycle <entity> 生成登录边界：不走 Nest，改为对 context.PlayerID 调 lifecycle <Entity>FromRegistry(...).GetOrCreate，要求已有 roost add lifecycle 与 controller 的 Registry()；首登前的玩法请求会收到 errcode.CodeEntityNotFound（entity_not_found）。roost generate 为还没有 endpoint 的协议写的“未实现”占位（Handle<Protocol> 只返回 not implemented）在未改动时由 add endpoint 直接替换；改过的文件不覆盖，报错说明删掉或改名后重跑。`,
		Example: `roost add endpoint RenamePlayer --handler player
roost add endpoint EquipItem --handler inventory --protocol EquipItem --nest-handler Equip
roost add endpoint EnterGame --handler player --lifecycle Player`,
	},
	{
		Name: "timer", Aliases: []string{"timers", "entity-timer"},
		Summary:       "给实体加持久定时器：到点由驱动投递 Tick，在实体的 Nest 锁内触发",
		Usage:         `roost add timer --entity <Entity> [--component Timer] [--service <service>] [--id <component-id>]`,
		Configuration: `要求实体已有 DAO（roost add dao <Entity> --entity <Entity>）、工程有 nest 运行时；Remote 托管（remote=managed）与 noPersist 的实体拒绝。写入 roost.yaml timers.<entity>，在实体 DAO 定义里加节点 map 与种子两个字段、实体加组件字段（这两处之后归业务）；生成部分 <component>_component_gen.go、db/def/<entity>_<component>_node_gen.go、game/handler/tick_<entity>_<component>_gen.go 与 bootstrap 里的 wiring/entitytimer 驱动由 project sync 重写；定时器类型与回调写在 game/entities/<entity>/<component>_handlers.go（业务文件，只建一次）。驱动按已加载实体的最早期限只给到期的那一个实体投递 Tick，不按在线人数轮询，时间取 App 业务时钟。离线语义：框架不追帧，实体不在内存期间到期的节点加载后只触发一次，回调参数 entitytimer.Fire 带原定期限 Deadline、触发时刻 Now、加载时刻 LoadedAt、补触发标志 Overdue 与间隔 Interval，补算错过的周期由业务做；回调返回正值为下次间隔（从 Now 起算）。说明见 ` + entityTimerDocURL + `。`,
		Example: `roost add timer --entity Player
# game/entities/player/timer_handlers.go：定义 TimerTypeBuffExpire 与回调
# Nest handler 里：target.TimerComp().NewTimerAt(expireAt, player.TimerTypeBuffExpire, 0, 0, nil)
roost generate`,
	},
	{
		Name: "skill", Aliases: []string{"skills", "ability"},
		Summary:       "生成稳定 roost-core/skill JSON 骨架和启动期编译目录",
		Usage:         `roost add skill <name>`,
		Configuration: `创建 game/skills/<name>.json；首次创建同时生成 catalog.go 与 catalog_test.go（已有目录缺测试时补上，两者都归业务、只建一次）。空骨架只 finish，不预设伤害、目标或资源规则。CompileAll 使用稳定 /skill import，对所有嵌入定义严格 Parse/Compile，重复 ID 或 error diagnostic 返回错误。catalog_test.go 的 TestSkillCatalogCompiles 用同一路径编译全部 JSON，go test ./game/skills/ 与 roost project doctor 的 skills:catalog 项拦住写坏的定义；服务启动时也调用一次 CompileAll，失败拒绝启动（Init 归业务，需自己接上）。工程用自定义 CompileEnvironment 时把测试里的环境换成同一个。`,
		Example: `roost add skill Fireball
# 编辑 game/skills/fireball.json
go test ./game/skills/          # TestSkillCatalogCompiles 编译全部定义
# 服务 Init：skills.CompileAll(skill.DefaultCompileEnvironment())，失败返回错误`,
	},
	{
		Name: "mods", Aliases: []string{"mod", "kit-mods"},
		Summary: "配置并自动补齐 Wiring Mod 依赖",
		Usage: `roost add mod <name> -service <service>
roost project new <name> -module <go-module> -mods <comma-separated-mods>`,
		Configuration: `可用 Mod：lock、ops、statslog、configdata、etcd、redis、mongo、nats、sync、remote_entity、dataengine、nest、saga。Data Engine 是唯一持久化引擎；添加 nest 或 saga 时自动补齐 dataengine、mongo 和 nats。`,
		Example: `roost add mod saga -service game
# roost.yaml
services:
  game:
    mods: [configdata, etcd, redis, mongo, nats, dataengine, nest, saga]`,
	},
	{
		Name: "services", Aliases: []string{"framework-services", "hosted-services", "uses"},
		Summary: "把托管框架服务（排行、邮件、聊天……）接进工程：services.<name>.framework 与 uses",
		Usage: `# roost.yaml 里加一个托管服务进程，并让业务服务 uses 它，然后
roost project sync`,
		Configuration: `托管框架服务是 roost-core service/ 里现成的服务，共九个：account、chat、mail、match、session、activity、global、platform、rank。services.<name>.framework: <服务> 把一个进程声明为该托管服务（没有业务 Service，redis、nats 自动补齐，match、activity 另补 etcd）；它的协作者（身份校验、投递、频道策略……）在 internal/service/<name>/collaborators.go，只生成一次、默认全部拒绝，doctor 的 collaborators:<name> 提示还没实现的。业务服务的 services.<svc>.uses: [<name>] 装配对应 ClientMod，并在 internal/service/<svc>/framework_clients_gen.go 生成类型化访问器（例如 Rank(registry) 返回 rank.Rank），在 Init 里取一次保存。已有工程接入一个：按下面改 roost.yaml 后 roost project sync（新服务的配置、部署与协作者文件随之生成），再 roost project doctor。roost project new -template game 一次生成全部九个并接到第一个业务服务。说明见生成工程的 docs/ROOST_YAML.zh-CN.md 与 docs/SERVICES.zh-CN.md。`,
		Example: `# roost.yaml：把排行接进已有的 game 服务
services:
  game:
    mods: [configdata, nest]
    uses: [rank]
  rank:
    framework: rank
# 然后
roost project sync
# game 的 Init 里
ranks, err := Rank(registry) // internal/service/game/framework_clients_gen.go`,
	},
	{
		Name: "dao", Aliases: []string{"database"},
		Summary: "生成私有存储、getter/mutator、dirty、patch、undo 和持久化代码",
		Usage: `roost add dao <name> --entity <owner>
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/dao@latest -def ./db/def -out ./db -pkg db [-force]`,
		Configuration: `--entity 会自动把 DAO import、DaoManager、dao tag 和接口 getter 接入 Entity；省略时只创建独立 DAO。定义使用 //roost:dao coll=<collection> db=<database> [dbscope=sid|global]，add dao 骨架的 db= 默认是工程名（编译期常量，与 dataengine.database 无关；文档 _id 是完整 EntityID，用 roost id entity 换算）；只给 noPersist 实体用、全字段 nopersist 的内存 DAO 写 //roost:dao nocoll（不编造集合名，不生成持久化路径）。字段一旦写 dao tag 就必须声明 persist/sync 意图；支持 persist、sync、nopersist、nosync、map=fast、map=sharded 和 -。不要声明 ID/id/tracker 保留字段。字段与 tracker 均为私有，业务通过生成方法访问。`,
		Example: `//roost:dao coll=players db=game dbscope=sid
type PlayerDao struct {
    Name  string          ` + "`dao:\"persist,sync\"`" + `
    Items map[int64]int32 ` + "`dao:\"persist,sync,map=fast\"`" + `
}

dao.SetName("alice")
dao.SetItems(1001, 3)
name := dao.GetName()`,
	},
	{
		Name: "entity", Aliases: []string{"entities"},
		Summary: "生成 Entity 工厂、组件/DAO 装配、Snapshot 和生命周期 hook",
		Usage: `roost add entity <name>
roost add component <name> --entity <owner>
roost add dao <name> --entity <owner>
roost generate`,
		Configuration: `高层 add 命令自动分配 ID、注册 Component 工厂、生成窄 Entity 接口，并更新 Entity 的 ComponentManager/DaoManager 与字段 tag；Entity wire 生成 getter。只有一个 Entity 时 Component 可省略 --entity；多个时拒绝猜测。底层 Entity marker 使用 //roost:entity entityKind=<const>，还可配置 businessPool=short|long、remote=managed、sync=true、syncNamespace、subjectPacker。`,
		Example: `//roost:entity entityKind=EntityKindPlayer
type Player struct {
    *entity.EntityBase
    entity.ComponentManager
    entity.DaoManager
    bag *BagComponent ` + "`comp:\"CompTypeBag\"`" + `
    dao *PlayerDao    ` + "`dao:\"players\"`" + `
}`,
	},
	{
		Name: "nest", Aliases: []string{"handler", "sender"},
		Summary:       "生成 Entity 加锁调度、Sender、回滚和 durability 接入",
		Usage:         `go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/nest@latest -dir ./game [-sender=true] [-force]`,
		Configuration: `handler 使用 //roost:nest；rollback=state|undo，durability=memory|async|strict，可加 sync。Entity 接口参数是锁目标，多个目标按全局顺序加锁。Remote read 放在带 remote tag 的 RemoteViewRef 字段。`,
		Example: `//roost:nest rollback=undo durability=strict
func handlerTransfer(from IPlayerEntity, to IPlayerEntity, itemID int64) error {
    return nil
}`,
	},
	{
		Name: "protocol", Aliases: []string{"proto", "protobuf"},
		Summary:       "生成 Proto、PB Go、消息 ID、binding、handler、robot registry 和 manifest",
		Usage:         `go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/protocol@latest -def ./protocol/def -proto ./protocol/proto -pb ./protocol/pb -msgid ./protocol/msgid -bind ./protocol/player_bind -handlers ./game/protocol_handlers`,
		Configuration: `文件使用 //roost:proto package=<proto-package> go_package=<go-import;alias>；接口使用 //roost:protocol group=<group> handler=<name>；方法使用 //roost:msg id=<id>。支持 request/response、client push、server notify 和反向 proto 导入。`,
		Example: `//roost:protocol group=game handler=player
type GameProtocol interface {
    //roost:msg id=10001
    Ping(PingRequest) PingResponse
}`,
	},
	{
		Name: "cfggen", Aliases: []string{"configgen", "config-data"},
		Summary:       "从 YAML schema 生成强类型配置表、对象、bean、索引和引用校验",
		Usage:         `go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/cfggen@latest -meta ./configs/schema/cfg.yaml -out ./configs/cfg -pkg cfg`,
		Configuration: `schema 支持 package、beans、tables、globals；表字段支持 type、key、index、skipempty 与规则 required（键必须出现且不为 null）、unique、min、enum、ref。规则生成为 cfg 标签，由 roost-core/configdata 在每次加载 / reload 时检查（与 tablegen 同一个检查器），违反即整次拒绝并点名表 / 行 / 字段 / 规则。运行时通过 RegisterGeneratedConfigData 注册到 roost-core/configdata。`,
		Example: `package: cfg
tables:
  - name: monster
    key: id
    fields:
      - {name: id, type: int32}
      - {name: scene_id, type: int32, index: true}`,
	},
	{
		Name: "tablegen", Aliases: []string{"table", "csv"},
		Summary:       "从 Go metadata 生成配置类型、CSV 模板并转换/校验 JSON",
		Usage:         `go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/tablegen@latest -meta ./configs/schema [-out dir] [-csv-template dir] [-csv dir -json dir] [-check] [-force]`,
		Configuration: `类型使用 //roost:table name=<name> file=<csv> json=<json> key=<field> 或 //roost:object。字段通过 csv/json/title 与规则 tag required/unique/min/enum（enum:"a|b"）/ref 描述。规则只有一份：生成的 loader 把它们交给 roost-core/configdata，在每次加载 / reload 时对 JSON 检查（required 能分清缺列与零值；ref 的非零值必须是目标表的主键），违反即整次拒绝、旧快照保持，错误点名表 / 行 / 字段 / 规则；CSV 转 JSON 与 -json ./configs/data -check 用同一个检查器提前反馈。生成的代码需要包含 configdata 规则的 roost-core。`,
		Example: `//roost:table name=monster file=monster.csv json=monster.json key=ID
type Monster struct {
    ID   int32  ` + "`csv:\"id\" json:\"id\" required:\"true\" unique:\"true\"`" + `
    Name string ` + "`csv:\"name\" json:\"name\"`" + `
}`,
	},
	{
		Name: "attribute", Aliases: []string{"attr", "attributes"},
		Summary: "生成属性 ID、mask、setter、派生公式、snapshot 和容器访问器",
		Usage: `roost add attribute <Profile> [--root dir]
roost generate
# 单独运行生成器（roost generate 读的就是这个目录）：
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/attribute@latest -dir ./game/gameplay/attribute [-output file] [-force]`,
		Configuration: `roost add attribute 开启 attribute feature（project sync 写出 game/gameplay/attribute/doc.go 与受控的 runtime.go，生成代码引用的 AttrID、AttributeProfile 等在那里）、写带 dirtyMask 的 profile 骨架并立即生成。手改 roost.yaml 的 features 开启时要先 roost project sync，否则 roost generate 报错指出缺 runtime.go。profile 使用 //roost:attribute index=<start> max=<count>（写在 type 上一行），必须带未导出的 dirtyMask uint64 字段（生成的 setter 写它）；导出字段都是属性且必须是整数，可用 attr:"name" 或 attr:"-"。_Field 方法定义派生公式，参数名必须匹配输入字段；循环依赖、未知字段和类型不一致会失败。`,
		Example: `//roost:attribute index=1 max=64
type PlayerProfile struct {
    HP        int64
    Attack    int64
    Power     int64
    dirtyMask uint64
}

func (p *PlayerProfile) _Power(Attack int64, HP int64) int64 {
    return Attack*2 + HP/10
}`,
	},
	{
		Name: "webroute", Aliases: []string{"web", "http"},
		Summary: "生成类型化 HTTP route 注册、请求解码和响应映射",
		Usage: `roost add webroute <name> [-service <svc>] [--root dir]
roost generate
# 单独运行生成器：
go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/webroute@latest -dir ./service/web [-force]`,
		Configuration: `add webroute 写 service/web/<name>.go 路由骨架、开启 webroute feature（doc.go 定义路由收到的 Service）、给宿主服务装 web Mod（wiring/web，配置 web.enabled / web.addr 缺省 127.0.0.1:8080 / web.allow_public_addr，以及 web.read_header_timeout 5s / web.read_timeout 15s / web.write_timeout 15s / web.idle_timeout 60s / web.max_body_bytes 1048576，超限回 413）并立即生成。服务 Init 里加一行 kitweb.RegisterRoutes(registry, &web.Service{}) 后路由可达：web Mod 在 Start 时 bind，service.started 时冻结路由表并开始 Serve，service.stopping 时停止接新请求；doctor 的 webroute:serve 在没人登记时 WARN。web Mod 不做鉴权，生产环境绑非回环地址要显式 web.allow_public_addr: true。handler 使用 //roost:web method=<HTTP method> path=<path> body=json|raw。JSON 接受一个完整文档；raw 使用 webroute.RawRequest（含 Header）。返回的错误按框架错误码映射：errcode 定义的错误回 400 {"ok":false,"code":<码>,"reason":<message>}，其余回 500 {"ok":false,"code":1,"reason":"server error"}（原文只进服务端日志）；要别的状态码用 webroute.WriteResultWithMapper。handler panic 回 500 并记带路由模板的日志；指标 web.request.total / web.request.duration 按路由模板计。重复 method/path、非法模式和错误签名会失败。`,
		Example: `//roost:web method=POST path=/gm/player body=json
func queryPlayer(ctx context.Context, svc *Service, req QueryRequest) (QueryResponse, error) {
    return QueryResponse{}, nil
}`,
	},
	{
		Name: "errcode", Aliases: []string{"error", "errors"},
		Summary:       "扫描错误码定义、检查冲突并导出 CSV",
		Usage:         `go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/errcode@latest -root . -out docs/generated/errcode.csv`,
		Configuration: `扫描 errcode.Define(code, name, message) 常量调用；code/name 重复或非常量参数会失败。编号空间由 roost.yaml 的 ids.errcode 管理。`,
		Example: `var ErrItemNotFound = errcode.Define(100001, "item_not_found", "item not found")

roost id next errcode
roost generate`,
	},
	{
		Name: "saga", Aliases: []string{"transaction", "cross-service"},
		Summary: "生成跨服务 Saga 定义、步骤运行时（原生步骤 / Mongo 步骤）与原生步骤的 Nest handler 骨架",
		Usage:   `roost add saga <name> -service <service> -steps <step1,step2,...> [-native <step,...> -entity <Entity> -component <Component>]`,
		Configuration: `目标 Service 自动加上 saga Mod（补齐 dataengine、nats、mongo）。-steps 按执行顺序书写：生成的 Definition 原样保留这个顺序，Saga 按它推进、按它的倒序补偿；步骤名不能重复。-native 列出哪些步骤的业务是一笔 Nest 事务（必须是 -steps 的子集，不能重复），这些步骤走原生路径：改实体、回执与 completion 同一条 WAL 记录，至多生效一次，不需要自己的幂等；-entity / -component 与 add handler 相同（工程只有一个 Entity 时 -entity 可省），服务要有 nest Mod。其余步骤是 Mongo 步骤：在 Mongo 事务里运行，调用别的服务要按 Command.IdempotencyKey 幂等。
生成物都是业务文件、只建一次：saga/<name>/definition.go（定义、步骤主题常量、EmitStart / Start、按业务键查结果的 Lookup）；saga/<name>/steps.go（State 与 EncodeState / DecodeState、每步正向与补偿的步骤体、按主题常量登记的 RegisterSteps、StartSteps；durable 为 <service>-<name>-<步骤>）；每个原生步骤两个 Nest handler：game/handler/<name>_<step>.go 与 <name>_<step>_compensation.go（参数带 saga.NativeStep，事务里调用 step.Complete，业务拒绝也提交）。骨架默认拒绝（"... is not implemented"），写完业务前 saga 不会报告成功。
服务的 Init / Shutdown 归业务、不改写：Init 里 steps, err := <name>.StartSteps(context.Background(), registry, <name>.StepDeps{})，Shutdown 里 if err := steps.Drain(ctx); err != nil { steps.Stop() }；roost project doctor 的 saga-steps:<name> 在没人调用 StartSteps 时 WARN。Nest handler 里启动一笔 saga：state, _ := <name>.EncodeState(<name>.State{...})；<name>.EmitStart(businessKey, state, deadline)。一个 (Type, businessKey) 只有一笔 saga，ID 由协调器分配：业务键取发起方能交还给客户端的值（例如请求号），之后在跑协调器的进程里 <name>.Lookup(ctx, engine, businessKey) 查结果（engine 是 app.Lookup[*saga.Engine](registry, mods.ModSaga)；saga.ErrNotFound 表示还没开始，终态 Completed / Compensated / Failed 即结果）。超时、重试与回执参数在 saga 配置段（saga.step_defaults、saga.steps.<type>.<step>）。
容量（2026-10-18 实测，投影 checkpoint 缺省间隔 20 ms）：步骤消费者的回调只做预留与 Nest 请求、等投影回执在后台；DataEngine 投影器逐条处理带回执 / effect 的记录，约 20 条/s（正向每笔 2 条、全补偿每笔 3 条）。单进程两步原生步骤 saga 正向约 8/s、全补偿约 5/s 以内 P50 在 1 秒内（P99 0.8 / 1.2 s）；正向 10/s 仍 0 笔终态不对，但投影器跟不上、积压，延迟升到秒级（P50 2.2 s）；正向 12/s、全补偿 7/s 起步骤超时、重试放大而崩溃（仍恰好一次，但大量 saga 以 failed / compensated 结束）；上限档各单样本。Mongo 步骤正向 20/s 实测未饱和。量大时按 sid 分片多进程，并给原生步骤加 kitsaga.WithAdmit 准入（命令只在拥有该实体的进程执行，见 game-demo 送礼 saga）。实测：` + sagaCapacityDocURL + `；指南：` + sagaGuideURL + `。`,
		Example: `roost add saga gift -service game -steps debit,credit -native debit,credit -entity Player -component Wallet
roost add saga cross_server_trade -service game -steps reserve,deduct,deliver
roost generate`,
	},
	{
		Name: "effect", Aliases: []string{"outbox", "effects"},
		Summary: "生成事务性 effect（outbox）：载荷类型、topic 常量、Emit 辅助与消费服务的 SubscribeEffects 骨架",
		Usage:   `roost add effect <topic> -consumer <service>`,
		Configuration: `<topic> 是点分名字（例如 player.level_up，saga.* 保留给 saga）。生成 game/effects/<topic>.go（载荷 struct、Topic<Topic> 常量、Emit<Topic>(key, event)：在 Nest handler 里调用，effect 与实体修改进同一条 WAL 记录，事务回滚则不发）与 internal/service/<consumer>/<topic>_consumer.go（start<Topic>EffectConsumer：kitdataengine.SubscribeEffects，载荷解不开返回 kitdataengine.PoisonEffect，durable <consumer>-<topic>）。都是业务文件、只建一次；effect 文件已存在时只建新消费服务的消费者（同一 effect 多个消费者）。消费服务需要 dataengine Mod（效果流与回执库取 dataengine.effects.*、dataengine.database，不另配）。
语义：至少一次投递、按 EffectID 至多一次生效——handler 在 Mongo 事务里运行，经它的 ctx 写的 Mongo 与回执一起提交；调用别的服务在事务外，要按 envelope.EffectID 幂等。普通错误退避重投，PoisonEffect 丢弃并计 dataengine.effect_consumer.handled_total{result="poison"}。骨架默认返回错误（重投、不丢），写完业务前不会把 effect 当作已处理。
服务的 Init / Shutdown 归业务、不改写：Init 里 subscription, err := start<Topic>EffectConsumer(context.Background(), registry)，Shutdown 里 subscription.Drain()；roost project doctor 的 effect-consumer:<service>/<func> 在没人调用时 WARN。容量：一个副本在 durable 上逐条处理（约 1/handler 延迟），未单独测量；量大时加副本共享 durable。`,
		Example: `roost add effect player.level_up -consumer game
roost generate`,
	},
	{
		Name: "replication", Aliases: []string{"udp", "kcp", "quic", "lockstep"},
		Summary: "生成 UDP/KCP/QUIC 帧同步 Transport 装配入口",
		Usage: `roost project new <name> -module <go-module> -features nettransport-udp,nettransport-kcp,nettransport-quic
roost project sync`,
		Configuration: `feature 生成 internal/transport/generated.go：QUIC DATAGRAM、KCP reliable stream、UDP datagram 与 ReliableSender 组合。监听、TLS、票据、密钥和 Session 绑定由网关接入层提供。`,
		Example: `features:
  - nettransport-quic
  - nettransport-kcp
  - nettransport-udp`,
	},
	{
		Name: "config", Aliases: []string{"config-check"},
		Summary: "检查服务 YAML，并安全启停 player TCP",
		Usage: `roost config check --service <name> [--production] [--file path] [--root dir]
roost config check --all [--production] [--root dir]
roost config enable player-tcp [--service <name>] [--file path]
roost config disable player-tcp [--service <name>] [--file path]`,
		Configuration: `add transport tcp 会自动把禁用状态的完整配置补进开发和生产示例，不覆盖其他 YAML 内容。enable 会先检查 auth.go 不再是默认拒绝骨架，再只修改 enabled 标量；disable 始终可用。check 要求合法 YAML 和 sid，并按本服务全部 Mod 的声明检查值与键：框架段里没有声明的键（多半拼错）报错，可疑键先按编译出的进程里业务服务自己的声明复核；--file 按 config.<service>[.<变体>].yaml 认出服务，认不出时加 --service。--production 还拒绝危险默认值。键检查通过后，再交给编译出的进程跑 --check-config（跨键与跨 Mod 规则，例如 saga.start_effect_stream 必须等于 dataengine.effects.stream），与 doctor 的 config-check 同一处规则、同一个结论；工程编译不过时明确打印“cross-mod rules NOT checked”。业务独有段的拼写由 roost project doctor 检查。`,
		Example: `roost config check --all
roost config check --service game
roost config enable player-tcp --service game
roost project doctor --workflow player-tcp
roost config disable player-tcp --service game
roost config check --service game --production --file configs/service/config.game.prod.yaml`,
	},
	{
		Name: "id", Aliases: []string{"ids", "id-next", "id-check"},
		Summary: "分配并检查协议、Entity、Component 和错误码编号",
		Usage: `roost id next <kind> [--group group]
roost id check
roost id entity <entity-id>
roost id entity --unique <id> (--entity <Name> | --kind <n> --category <n> [--remote])`,
		Configuration: `编号范围位于 roost.yaml 的 ids。kind 可使用 protocol、entity、component、errcode；protocol 可按 group 划分范围。entity 子命令在完整 EntityID（DAO 文档的 _id、日志里的 entity=）与唯一号（玩家 ID）、kind、category 之间双向换算：布局 [remote(1)][unique(52)][kind(8)][category 低 2 位]；--entity 从工程的 //roost:entity 标记取 kind、category 与 remote=managed，解码时也据此注明 kind 名。`,
		Example: `roost id next -group game protocol
roost id next entity
roost id check
roost id entity 9217029
roost id entity --unique 9001 --entity Player`,
	},
	{
		Name: "format", Aliases: []string{"fmt", "format-check"},
		Summary:       "检查仓库 Go 文件是否经过 gofmt",
		Usage:         `roost format check`,
		Configuration: `该命令只检查，不修改文件；修复使用 gofmt 或 make fmt。CI 的 make ci 已包含 fmt-check。`,
		Example: `make fmt
roost format check`,
	},
	{
		Name: "deploy", Aliases: []string{"deployment", "docker", "kubernetes", "k8s", "shell"},
		Summary: "生成 Shell/systemd、Docker 和 Kubernetes 生产部署基线",
		Usage: `roost project sync
make cicd-check
make image-build VERSION=v1.0.0
make k8s-render ENV=staging`,
		Configuration: `生成项目包含可复现 CI、独立依赖升级、Release，以及 Shell、Docker、K8s 三套受保护环境部署 workflow。Shell 使用不可变 release、校验和、原子切换和 readiness 回滚；Docker 使用 digest、distroless nonroot 和 Compose 健康检查；K8s 使用 staging/production overlay、server-side dry-run、探针/PDB/NetworkPolicy，带 WAL 的服务使用 StatefulSet 与独占 RWO PVC。普通 CI 不解析 latest。`,
		Example: `make cicd-check
make deploy-shell SERVICE=game SID=1000 VERSION=v1.0.0 CONFIG=/etc/roost/game.yaml
make deploy-docker IMAGE=ghcr.io/example/planet@sha256:<digest> ENV_FILE=/etc/roost/planet.env
make deploy-k8s ENV=staging IMAGE=ghcr.io/example/planet@sha256:<digest>`,
	},
}

func isHelpToken(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "help", "-h", "--help":
		return true
	default:
		return false
	}
}

func runHelp(args []string, stdout io.Writer) error {
	if err := validateHelpTopics(); err != nil {
		return fmt.Errorf("invalid help catalog: %w", err)
	}
	if len(args) == 0 || (len(args) == 1 && (strings.EqualFold(args[0], "list") || isHelpToken(args[0]))) {
		printHelpOverview(stdout)
		return nil
	}
	if len(args) != 1 {
		return errors.New("usage: roost help [list|all|<capability>]")
	}
	if strings.EqualFold(args[0], "all") {
		for index, topic := range helpTopics {
			if index > 0 {
				fmt.Fprintln(stdout, "\n------------------------------------------------------------")
			}
			printHelpTopic(stdout, topic)
		}
		return nil
	}
	topic, ok := findHelpTopic(args[0])
	if !ok {
		return fmt.Errorf("unknown help capability %q; run roost help to list capabilities", args[0])
	}
	printHelpTopic(stdout, topic)
	return nil
}

func runContextHelp(command []string, stdout io.Writer) error {
	if err := validateHelpTopics(); err != nil {
		return fmt.Errorf("invalid help catalog: %w", err)
	}
	if len(command) == 0 {
		printHelpOverview(stdout)
		return nil
	}
	// Config subcommands share one safety contract. Prefer it over the
	// player-tcp alias so `config enable player-tcp --help` explains the
	// fail-closed enable/disable behavior rather than only the transport shape.
	if command[0] == "config" {
		if topic, ok := findHelpTopic("config"); ok {
			printHelpTopic(stdout, topic)
			return nil
		}
	}
	joined := strings.Join(command, "-")
	for _, candidate := range []string{joined, command[len(command)-1], command[0]} {
		if topic, ok := findHelpTopic(candidate); ok {
			printHelpTopic(stdout, topic)
			return nil
		}
	}
	return fmt.Errorf("no help topic for %q; run roost help to list capabilities", strings.Join(command, " "))
}

func findHelpTopic(name string) (helpTopic, bool) {
	name = strings.ToLower(strings.TrimSpace(name))
	for _, topic := range helpTopics {
		if topic.Name == name {
			return topic, true
		}
		for _, alias := range topic.Aliases {
			if alias == name {
				return topic, true
			}
		}
	}
	return helpTopic{}, false
}

func printHelpOverview(w io.Writer) {
	fmt.Fprintln(w, "roost-codegen - Roost 游戏服务器工程与代码生成工具链")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "用法:")
	fmt.Fprintln(w, "  roost help                         显示能力目录")
	fmt.Fprintln(w, "  roost help <能力>                  显示配置、命令和示例")
	fmt.Fprintln(w, "  roost help all                     显示全部专题")
	fmt.Fprintln(w, "  roost <顶级命令> --help            显示对应专题")
	fmt.Fprintln(w, "  roost version                      显示已安装版本")
	fmt.Fprintln(w, "  roost env doctor                   检查本机工具和 PATH")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "第一次使用: roost help beginner")
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "能力:")
	width := 0
	for _, topic := range helpTopics {
		if len(topic.Name) > width {
			width = len(topic.Name)
		}
	}
	for _, topic := range helpTopics {
		fmt.Fprintf(w, "  %-*s  %s\n", width, topic.Name, topic.Summary)
	}
	fmt.Fprintln(w, "")
	fmt.Fprintln(w, "示例: roost help beginner | roost help entity | roost help versions")
	fmt.Fprintln(w, "完整文档: README.md、docs/CODEGEN_REFERENCE.zh-CN.md、docs/PROJECT_GENERATOR.zh-CN.md")
}

func printHelpTopic(w io.Writer, topic helpTopic) {
	fmt.Fprintf(w, "%s - %s\n\n", topic.Name, topic.Summary)
	fmt.Fprintln(w, "用途:")
	fmt.Fprintln(w, indentHelp(topic.Summary))
	fmt.Fprintln(w, "\n命令:")
	fmt.Fprintln(w, indentHelp(topic.Usage))
	fmt.Fprintln(w, "\n配置/标记:")
	fmt.Fprintln(w, indentHelp(topic.Configuration))
	fmt.Fprintln(w, "\n示例:")
	fmt.Fprintln(w, indentHelp(topic.Example))
}

func indentHelp(value string) string {
	lines := strings.Split(strings.TrimSpace(value), "\n")
	for index := range lines {
		lines[index] = "  " + lines[index]
	}
	return strings.Join(lines, "\n")
}

func validateHelpTopics() error {
	seen := make(map[string]string)
	for _, topic := range helpTopics {
		if strings.TrimSpace(topic.Name) == "" || strings.TrimSpace(topic.Summary) == "" || strings.TrimSpace(topic.Usage) == "" || strings.TrimSpace(topic.Configuration) == "" || strings.TrimSpace(topic.Example) == "" {
			return fmt.Errorf("help topic %q is incomplete", topic.Name)
		}
		names := append([]string{topic.Name}, topic.Aliases...)
		for _, name := range names {
			name = strings.ToLower(strings.TrimSpace(name))
			if owner, exists := seen[name]; exists {
				return fmt.Errorf("help alias %q is shared by %s and %s", name, owner, topic.Name)
			}
			seen[name] = topic.Name
		}
	}
	return nil
}

func helpTopicNames() []string {
	names := make([]string, 0, len(helpTopics))
	for _, topic := range helpTopics {
		names = append(names, topic.Name)
	}
	sort.Strings(names)
	return names
}
