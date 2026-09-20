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
		Example:       "go install github.com/tjbdwanghaibo/roost-codegen/cmd/roost@latest\nroost version\nroost env doctor",
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
roost project upgrade [--root dir] [--dry-run] [--consolidate] [-core version] [-kit version] [-codegen version]
make project-upgrade；跨过收敛边界（core v1.14.0 / kit v1.13.0）先执行 roost project upgrade --consolidate [--dry-run] 改写 import`,
		Configuration: `-module 必填，防止项目意外使用框架作者的仓库命名空间。默认 -out=<name>、-services=game、features=protocol,config,entity,nest,event,dao,errcode；不传 -mods 时启用除 saga 外的生产基础 Mod，新手建议显式使用 -mods configdata。-template game 是可选的起步形态：account、chat、mail、match、session 五个 roost-service 服务各作为独立子命令托管（services.<name>.framework），第一个业务 Service 通过 services.<name>.uses 拿到它们的 ClientMod 与类型化访问器，并带 nest 运行时、Player 与 World 两个 Entity 及其 lifecycle；World 是进程内单例（game/lifecycle/world_singleton.go 的 EnsureWorld，Service.Init 里确保存在）；托管服务的协作者（身份校验、投递、频道策略……）在 internal/service/<name>/collaborators.go 里，默认全部拒绝，需要项目自己实现。-template game-demo 在 game 之上再生成一条可运行的写入链路：Player 带 Profile / Bag 两个组件与 PlayerDao 持久化字段，一个 //roost:nest rollback=undo durability=strict 的加道具事务，以及 player TCP 接入与对应协议 / 端点。组件方法、DAO 字段、handler、协议定义和 auth.go 都是业务文件：生成一次，之后 sync / upgrade 不再改写，直接在上面改。其中 auth.go 是**演示用凭据**（认 player:<id>），上线前必须换成真实校验。demo 源码在 codegen 仓的 demo/ 目录，按生成后的路径原样存放。project next 根据项目真实文件计算进度，只给一个当前动作，不会替业务决定字段或生成 preset；默认先完成 first-business，再进入可选 player-tcp。项目声明位于 roost.yaml。sync 在同级临时副本完成全部生成和所有权预检后提交；失败回滚已写文件，并拒绝覆盖提交期间发生的并发修改。upgrade 只覆盖 codegen 受控文件。`,
		Example: `roost project new planet -module example.com/planet -services game,gate
cd planet
roost project next

# 想先看一条跑通的写入链路，再照着改：
roost project new planet -module example.com/planet -mods configdata,mongo,nats,dataengine,nest -template game-demo

# 旧项目执行一次，升级后即可使用 make project-upgrade
go run github.com/tjbdwanghaibo/roost-codegen/cmd/roost@latest project upgrade --root . --dry-run -core latest -kit latest -codegen latest
go run github.com/tjbdwanghaibo/roost-codegen/cmd/roost@latest project upgrade --root . -core latest -kit latest -codegen latest`,
	},
	{
		Name: "versions", Aliases: []string{"deps", "dependency", "dependencies", "project-deps", "roost-up", "codegen-up"},
		Summary: "持续跟随 core、kit、skill、codegen 最新发布版本",
		Usage: `roost project deps [--root dir]
make deps-update
make roost-up
make codegen-up
roost project upgrade -core latest -kit latest -skill latest -codegen latest`,
		Configuration: fmt.Sprintf(`roost.yaml 的 versions.* 默认是 latest。deps-update 在临时项目联合解析 core/kit，只提交最终 go.mod/go.sum；失败或并发变化不会覆盖原文件。roost-up 执行 GOWORK=off go get -u ./... 与 go mod tidy，更新所有被项目引用的依赖；codegen-up 执行 go install github.com/tjbdwanghaibo/roost-codegen/cmd/roost@latest，更新本机 CLI。go.mod 保存具体版本，roost.yaml 保存更新策略。兼容下限：core %s、kit %s、codegen %s。明确版本表示 MVS 下限，不是上限。`, minimumVersions.Core, minimumVersions.Kit, minimumVersions.Codegen),
		Example: `versions:
  core: latest
  kit: latest
  skill: latest
  codegen: latest

make deps-update
make roost-up
make codegen-up`,
	},
	{
		Name: "framework-release", Aliases: []string{"framework", "release-train", "framework-lock"},
		Summary: "校验 core、kit、skill 精确发布集合并生成不可变框架锁",
		Usage: `roost framework verify --manifest ci/framework-release.yaml --expected-codegen v1.10.0 --lock framework-lock.json
roost framework verify --github-output "$GITHUB_OUTPUT"`,
		Configuration: `ci/framework-release.yaml 只能使用 vMAJOR.MINOR.PATCH，不能使用 latest 或伪版本。verify 从 Go module proxy 获取三个已发布模块，校验 module path、checksum、replace 和框架内部伪版本依赖，然后生成 framework-lock.json。codegen release workflow 只有在完整生成项目兼容矩阵通过后才进入受保护 framework-release Environment。`,
		Example: `schema: 1
codegen: v1.10.0
framework: {core: v1.9.1, kit: v1.9.2, skill: v1.9.1}
consumer_go: [1.27.x]

roost framework verify --expected-codegen v1.10.0`,
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
		Usage:         `roost add <service|mod|access|transport|module|protocol|entity|component|handler|lifecycle|endpoint|skill|event|table|dao|webroute|errcode|saga|rpc> <name> [flags]`,
		Configuration: `通用参数：--root、--service、--mods、--steps、--group、--id；Component/DAO 可用 --entity 自动接入所属 Entity；protocol 用 --handler 选择 controller domain；endpoint 用 --handler、--protocol、--nest-handler 接线；handler 使用 --entity 和 --component 生成正确 Nest 锁目标。骨架文件归业务所有，不会被 project sync 覆盖；创建后运行 roost generate。`,
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
roost add saga cross_server_trade -service game -steps reserve,deduct,deliver`,
	},
	{
		Name: "access", Aliases: []string{"player-access", "gateway-access"},
		Summary: "生成已认证玩家协议边界并装配到指定 Service",
		Usage: `roost add access player --service <service>
roost generate`,
		Configuration: `当前 access 名仅支持 player。它写入 roost.yaml access.player.service，启用 protocol/nest，并给目标 Service 补 Nest 运行依赖；生成并发安全的 ProtocolRegistry、typed binding 聚合和 access.player Mod。用 add transport tcp 生成生产约束的应用接入适配器。`,
		Example: `roost add access player --service game
roost add protocol RenamePlayer --group game --handler player
roost add endpoint RenamePlayer --handler player`,
	},
	{
		Name: "transport", Aliases: []string{"tcp", "player-tcp", "gateway-tcp"},
		Summary: "为 player access 生成有界 TCP listener、帧协议和 fail-closed 鉴权边界",
		Usage: `roost add transport tcp [--service <service>]
roost project doctor --workflow player-tcp`,
		Configuration: `要求先 add access player。生成代码提供 16 字节有界帧、全局/单 IP 连接上限、独立 handshake 并发与字节上限、handshake/idle/write timeout、sequence 防重放、同步写背压、TCP keepalive、优雅关停，以及 PushPlayer/PushSession 主动发布；auth.go 归应用所有且默认拒绝。完整 disabled 配置会自动写入，完成鉴权后用 config enable player-tcp 开启。ROOST_PLAYER_TOKEN 环境变量可供 cmd/playerprobe 做最小鉴权验收；生产鉴权仍需安全评审。Docker/Kubernetes 模板会发布 7000，Kubernetes 仅允许带 player-access 标签的来源 namespace。`,
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
		Usage:         `roost add endpoint <name> --handler <controller-domain> [--protocol <name>] [--nest-handler <name>]`,
		Configuration: `要求 access.player、已有 protocol 和 Nest handler。第一个 Nest 参数是由 context.PlayerID 定位的 Entity；其余命名参数按名称映射到 Request 字段。controller 从 Registry 注入 nest.Client，不直接访问 EntityManager。`,
		Example: `roost add endpoint RenamePlayer --handler player
roost add endpoint EquipItem --handler inventory --protocol EquipItem --nest-handler Equip`,
	},
	{
		Name: "skill", Aliases: []string{"skills", "ability"},
		Summary:       "生成稳定 roost-core/skill JSON 骨架和启动期编译目录",
		Usage:         `roost add skill <name>`,
		Configuration: `创建 game/skills/<name>.json；首次创建同时生成 catalog.go。空骨架只 finish，不预设伤害、目标或资源规则。CompileAll 使用稳定 /skill import，对所有嵌入定义严格 Parse/Compile，重复 ID 或 error diagnostic 启动失败。`,
		Example: `roost add skill Fireball
# 编辑 game/skills/fireball.json
# 启动时：skills.CompileAll(skill.DefaultCompileEnvironment())`,
	},
	{
		Name: "mods", Aliases: []string{"mod", "kit-mods"},
		Summary: "配置并自动补齐 Kit Mod 依赖",
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
		Name: "dao", Aliases: []string{"database"},
		Summary: "生成私有存储、getter/mutator、dirty、patch、undo 和持久化代码",
		Usage: `roost add dao <name> --entity <owner>
go run github.com/tjbdwanghaibo/roost-codegen/cmd/dao@latest -def ./db/def -out ./db -pkg db [-force]`,
		Configuration: `--entity 会自动把 DAO import、DaoManager、dao tag 和接口 getter 接入 Entity；省略时只创建独立 DAO。定义使用 //roost:dao coll=<collection> db=<database> [dbscope=sid|global]。字段一旦写 dao tag 就必须声明 persist/sync 意图；支持 persist、sync、nopersist、nosync、map=fast、map=sharded 和 -。不要声明 ID/id/tracker 保留字段。字段与 tracker 均为私有，业务通过生成方法访问。`,
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
		Configuration: `高层 add 命令自动分配 ID、注册 Component 工厂、生成窄 Entity 接口，并更新 Entity 的 ComponentManager/DaoManager 与字段 tag；Entity wire 生成 getter。只有一个 Entity 时 Component 可省略 --entity；多个时拒绝猜测。底层 Entity marker 使用 //roost:entity entityKind=<const>，还可配置 remote=managed、sync=true、syncTopic、syncPacker、subjectPacker。`,
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
		Usage:         `go run github.com/tjbdwanghaibo/roost-codegen/cmd/nest@latest -dir ./game [-sender=true] [-force]`,
		Configuration: `handler 使用 //roost:nest；rollback=state|undo，durability=memory|async|strict，可加 sync。Entity 接口参数是锁目标，多个目标按全局顺序加锁。Remote read 放在带 remote tag 的 RemoteViewRef 字段。`,
		Example: `//roost:nest rollback=undo durability=strict
func handlerTransfer(from IPlayerEntity, to IPlayerEntity, itemID int64) error {
    return nil
}`,
	},
	{
		Name: "protocol", Aliases: []string{"proto", "protobuf"},
		Summary:       "生成 Proto、PB Go、消息 ID、binding、handler、robot registry 和 manifest",
		Usage:         `go run github.com/tjbdwanghaibo/roost-codegen/cmd/protocol@latest -def ./protocol/def -proto ./protocol/proto -pb ./protocol/pb -msgid ./protocol/msgid -bind ./protocol/player_bind -handlers ./game/protocol_handlers`,
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
		Usage:         `go run github.com/tjbdwanghaibo/roost-codegen/cmd/cfggen@latest -meta ./configs/schema/cfg.yaml -out ./configs/generated -pkg generated`,
		Configuration: `schema 支持 package、beans、tables、globals；字段支持 type、key、index、ref、required、skipempty。运行时通过 RegisterGeneratedConfigData 注册到 roost-core/configdata。`,
		Example: `package: generated
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
		Usage:         `go run github.com/tjbdwanghaibo/roost-codegen/cmd/tablegen@latest -meta ./configs/schema [-out dir] [-csv-template dir] [-csv dir -json dir] [-check] [-force]`,
		Configuration: `类型使用 //roost:table name=<name> file=<csv> json=<json> key=<field> 或 //roost:object。字段通过 csv/json/title/required/unique/ref tag 描述。`,
		Example: `//roost:table name=monster file=monster.csv json=monster.json key=ID
type Monster struct {
    ID   int32  ` + "`csv:\"id\" json:\"id\" required:\"true\" unique:\"true\"`" + `
    Name string ` + "`csv:\"name\" json:\"name\"`" + `
}`,
	},
	{
		Name: "eventgen", Aliases: []string{"event", "events"},
		Summary:       "生成事件类型、Type 方法以及接收者订阅/分发代码",
		Usage:         `go run github.com/tjbdwanghaibo/roost-codegen/cmd/eventgen@latest -def ./event/def -out ./event -pkg event -game ./game -eventpkg <module>/event [-force]`,
		Configuration: `定义目录中 Event 前缀 struct 会进入生成。业务接收者实现 DealEventXxx(*event.EventXxx)；签名不匹配在生成期失败。`,
		Example: `type EventPlayerLevelUp struct { PlayerID int64; Level int32 }

func (p *Player) DealEventPlayerLevelUp(e *event.EventPlayerLevelUp) {}`,
	},
	{
		Name: "attribute", Aliases: []string{"attr", "attributes"},
		Summary:       "生成属性 ID、mask、setter、派生公式、snapshot 和容器访问器",
		Usage:         `go run github.com/tjbdwanghaibo/roost-codegen/cmd/attribute@latest -dir ./game/attribute [-output file] [-force]`,
		Configuration: `profile 使用 //roost:attribute index=<start> max=<count>；字段可用 attr:"name" 或 attr:"-"。_Field 方法定义派生公式，参数名必须匹配输入字段；循环依赖、未知字段和类型不一致会失败。`,
		Example: `//roost:attribute index=1 max=64
type PlayerProfile struct { HP int64; Attack int64; Power int64 }

func (p *PlayerProfile) _Power(Attack int64, HP int64) int64 {
    return Attack*2 + HP/10
}`,
	},
	{
		Name: "webroute", Aliases: []string{"web", "http"},
		Summary:       "生成类型化 HTTP route 注册、请求解码和响应映射",
		Usage:         `go run github.com/tjbdwanghaibo/roost-codegen/cmd/webroute@latest -dir ./service/web [-force]`,
		Configuration: `handler 使用 //roost:web method=<HTTP method> path=<path> body=json|raw。JSON 接受一个完整文档；raw 使用 webroute.RawRequest。重复 method/path、非法模式和错误签名会失败。`,
		Example: `//roost:web method=POST path=/gm/player body=json
func queryPlayer(ctx context.Context, svc *Service, req QueryRequest) (QueryResponse, error) {
    return QueryResponse{}, nil
}`,
	},
	{
		Name: "errcode", Aliases: []string{"error", "errors"},
		Summary:       "扫描错误码定义、检查冲突并导出 CSV",
		Usage:         `go run github.com/tjbdwanghaibo/roost-codegen/cmd/errcode@latest -root . -out docs/generated/errcode.csv`,
		Configuration: `扫描 errcode.Define(code, name, message) 常量调用；code/name 重复或非常量参数会失败。编号空间由 roost.yaml 的 ids.errcode 管理。`,
		Example: `var ErrItemNotFound = errcode.Define(100001, "item_not_found", "item not found")

roost id next errcode
roost generate`,
	},
	{
		Name: "saga", Aliases: []string{"transaction", "cross-service"},
		Summary:       "生成跨服务 Saga 定义、步骤、补偿和注册骨架",
		Usage:         `roost add saga <name> -service <service> -steps <step1,step2,...>`,
		Configuration: `目标 Service 必须启用 saga Mod；它会补齐 dataengine、nats 和 mongo。每个步骤需要幂等执行与补偿，消息 durable、超时、重试和 receipt 参数位于 saga 配置段。`,
		Example: `roost add mod saga -service game
roost add saga cross_server_trade -service game -steps reserve,deduct,deliver
roost generate`,
	},
	{
		Name: "replication", Aliases: []string{"udp", "kcp", "quic", "lockstep"},
		Summary: "生成 UDP/KCP/QUIC 帧同步 Transport 装配入口",
		Usage: `roost project new <name> -module <go-module> -features replication-udp,replication-kcp,replication-quic
roost project sync`,
		Configuration: `feature 生成 internal/transport/generated.go：QUIC DATAGRAM、KCP reliable stream、UDP datagram 与 ReliableSender 组合。监听、TLS、票据、密钥和 Session 绑定由网关接入层提供。`,
		Example: `features:
  - replication-quic
  - replication-kcp
  - replication-udp`,
	},
	{
		Name: "config", Aliases: []string{"config-check"},
		Summary: "检查服务 YAML，并安全启停 player TCP",
		Usage: `roost config check --service <name> [--production] [--file path] [--root dir]
roost config check --all [--production] [--root dir]
roost config enable player-tcp [--service <name>] [--file path]
roost config disable player-tcp [--service <name>] [--file path]`,
		Configuration: `add transport tcp 会自动把禁用状态的完整配置补进开发和生产示例，不覆盖其他 YAML 内容。enable 会先检查 auth.go 不再是默认拒绝骨架，再只修改 enabled 标量；disable 始终可用。check 要求合法 YAML 和 sid，--production 还拒绝危险默认值。`,
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
roost id check`,
		Configuration: `编号范围位于 roost.yaml 的 ids。kind 可使用 protocol、entity、component、errcode；protocol 可按 group 划分范围。`,
		Example: `roost id next -group game protocol
roost id next entity
roost id check`,
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
