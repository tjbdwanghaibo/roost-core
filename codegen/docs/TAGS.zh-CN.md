# Roost 标记与字段 tag 手册

本文回答“这个标记写在哪里、影响什么、哪些要一起写”。命令参数与部署流程见[命令参考](CODEGEN_REFERENCE.zh-CN.md)，不在这里重复。以当前检出的 core / codegen 为准；`remoteview` 与独立 Remote 请求生成是当前 main 的未发布能力，版本状态见 [CHANGELOG](../../CHANGELOG.md)。

## 1. 先区分三种写法

| 形式 | 写在哪里 | 谁读取 | 示例 |
| --- | --- | --- | --- |
| 注释标记 | 对应类型、函数或接口方法的声明上方 | 生成器；部分由 ID 检查器读取 | `//roost:nest rollback=undo durability=strict` |
| Go struct tag | 字段类型后的反引号内 | 对应生成器或运行时编码器 | `dao:"persist" store:"coins"` |
| 项目配置 | `roost.yaml`、服务 YAML | 生成流水线、装配与运行时 | feature、扫描目录、Mod、同步模式 |

注释参数用空格分隔，字段 tag 的内部选项用逗号分隔。标记放在目标声明的文档注释中，最稳妥的是紧贴声明；不要隔一个其他声明，也不要给整个 `type (...)` 共用一个类型标记。只改手写输入，再运行 `roost generate`；不能手改 `*_gen.go`。

特别容易混淆的名字：

- DAO 定义字段上的 `dao:"persist,sync"` 控制字段能力；Entity 成员上的 `dao:"players"` 是 DAO 装配名称。
- `remoteview:"member"` 决定发布哪些字段；请求中的 `remote:"db.AllianceMemberSnapshot,required"` 决定预取哪种视图。
- `//roost:view group=game` 属于业务 PB 协议分组，与 Remote 视图不是同一功能。
- `//roost:nest sync` 生成同步调用 Sender，不是 EntitySync 开关；`//roost:entity sync=true` 才是实体同步接线。
- `//roost:rpc` 是服务 RPC；`//roost:protocol` / `//roost:msg` 是客户端业务 PB。

## 2. 注释标记速查

下表列推荐拼写；值的限制见后文及对应手册。不是每个标记都需要业务手写，`roost add` 和生成流水线会维护很多声明。

| 标记 | 位置与作用 | 参数 |
| --- | --- | --- |
| `//roost:dao` | DAO 定义 struct；生成事务化读写、持久与同步代码 | `coll`、`db`、`dbscope`、`schema`；或独立 flag `nocoll` |
| `//roost:redisdao` | Redis DAO 定义 struct；生成 Redis 访问包装 | 必需 `key`、`prefix`；可选 `mode`、`key_type`、`version`、`ttl`、`name` |
| `//roost:entity` | Entity struct；生成工厂、装配、生命周期 | `id`、`entityKind`、`category`、`businessPool`、`remote`、`noPersist`、`lifetime`、`sync`、`syncNamespace`、`subjectPacker`、`service` |
| `//roost:component type=N` | 组件声明；供 ID 空间检查 | `type` 为组件类型号；不代替组件实现、工厂和 Entity 的 `comp` 接线 |
| `//roost:mirror` | 手写只读 DTO；生成读取器 | `entityKind`、`coll`，对应拥有者 kind 与 DAO 集合 |
| `//roost:endpoint` | 手写 `Controller.Handle<Protocol>` 方法；持续生成同包 dispatch 转交 | 必需 `nest=<Nest handler>`；协议名由方法名推导 |
| `//roost:nest` | 业务 handler 函数；生成分发包装、Sender | `target` 或 `targets`、`rollback`、`durability`、`sync` |
| `//roost:proto` | 协议定义文件；指定 PB 包 | `package`、`go_package` |
| `//roost:protocol` | 协议接口 | `group`、`handler`；`controller` / `domain` 是 handler 的替代拼写，推荐只写 `handler` |
| `//roost:msg` | 协议接口方法 | `id`、`name`、`tags`、`handler`（也识别 `controller` / `domain`） |
| `//roost:view` | 协议 struct；覆盖协议分组 | `group` |
| `//roost:table` | 配置表 schema struct | `name`、`file`、`json`、`key` |
| `//roost:object` | 单例配置 schema struct | `name`、`file`、`json` |
| `//roost:attribute` | 属性 profile struct | `index`、`max` |
| `//roost:web` | HTTP handler | 必需 `method`、`path`、`body`；可选 `headers`、`compress`、`errors`、`status` |
| `//roost:rpc` | 服务接口 / 接口方法 | 接口：`service_type`、`capability`；方法：`affinity`、`route` / `route=<param>`（发往实体的 owner，键是 `int64` 完整 Entity ID，缺省参数名 `target`；见 [CODEGEN_REFERENCE](CODEGEN_REFERENCE.zh-CN.md)）。`shard=` 已移除 |
| `//roost:register` | 导出的包级注册函数 | `phase`、`order` |

`//roost:cache` 是 glsvet 的组件字段检查说明，不是生成器：表示明确允许不参与事务回滚的缓存字段，不能用它把事务状态绕开 DAO。`//go:generate` 与 `//go:build protocoldef` 属于 Go 工具链，也不是 Roost tag。

## 3. DAO：持久、客户端同步与服务端视图

```go
//roost:dao coll=alliances db=main schema=1
type AllianceDao struct {
    Name     string `store:"name" remoteview:"public,member"`
    Bonus    int64  `store:"bonus" dao:"persist" remoteview:"member"`
    Treasury int64  `store:"treasury" dao:"persist"`
    Online   int32  `dao:"nopersist,sync" store:"online"`
    Scratch  int64  `dao:"nopersist,nosync" store:"scratch"`
}
```

此处 Name 默认持久并允许客户端同步；Bonus 不进客户端同步，但会进入服务端 member 视图；Treasury 不进入任何声明的 Remote 字段视图。字段资格不等于网络授权：接收方、订阅与权限仍要业务接线。

### DAO 类型参数

| 参数 | 含义与缺省 |
| --- | --- |
| `coll` | 持久集合名，与 Entity 的 DAO 成员 tag 对应 |
| `db` | 逻辑数据库名，例如 `main`；不是 Mongo 物理库名 |
| `dbscope` | `global`（默认）或 `sid`（按服分库） |
| `schema` | 正整数，默认 1；持久文档须匹配当前版本，不自动迁移 |
| `nocoll` | 无集合 DAO；不能同时写上述存储参数，字段必须明确非持久 |

`redisdao` 的 `mode` 默认 `ref-hmap`，也支持 `raw`；`key_type` 可由字段路径推断，`ttl` 用 Go duration。它不是给普通 Mongo DAO 换权威存储的开关，也不支持下面的 `remoteview`。

### DAO 字段 tag

| 写法 | 持久 | 客户端 Sync | 说明 |
| --- | --- | --- | --- |
| 没有 `dao` tag | 是 | 是 | 必填的 `store` 不改变此缺省 |
| `dao:"persist,sync"` | 是 | 是 | 明确两项都开 |
| `dao:"persist"` | 是 | 否 | 只持久 |
| `dao:"sync"` / `dao:"nopersist,sync"` | 否 | 是 | 只同步 |
| `dao:"nopersist,nosync"` | 否 | 否 | 仍生成事务化 mutator、参与回滚 |
| `dao:"-"` | 否 | 否 | 完全排除，不是“只参与回滚” |
| `dao:"persist,sync,map=fast"` | 是 | 是 | map 实现选项；`small` 默认，另有 `fast`、`sharded` |
| `store:"coins,omitempty"` | — | — | 显式持久 / wire 字段名；选项只支持 `omitempty` |
| `remoteview:"public,member"` | 必须是 | 独立 | 将该字段加入两个服务端只读 Snapshot |

出现 `dao` 就只启用列出的能力；单写 `map=fast` 不合法，`persist,nopersist` 等矛盾组合也拒绝。`store:"-"` 不用于排除 DAO 字段；名字不能以 `_` 开头、不能重复。每个生成字段必须显式声明 `store`，没有 snake_case 回退；DAO 输入不再接受 `bson`。

### Remote Snapshot 的生成边界

`AllianceDao` 的上述声明生成同包的 `AlliancePublicSnapshot` / `AllianceMemberSnapshot`，以及 `PublicSnapshot()` / `MemberSnapshot()`、深拷贝和解码器。字段标记只接受小写 snake_case 视图名；不能空项或重复。

- 只能标记持久 Mongo DAO 的导出顶层字段。不能标记 `nocoll`、非持久、跳过字段或 Redis DAO。
- 标记一个嵌套结构字段，会包含该嵌套结构中的持久字段；不是只选同名 tag 的嵌套字段。嵌套字段本身不能再标 `remoteview`，敏感数据应拆分结构。
- Remote Entity 将相关字段的视图纳入已有提交 / outbox；无关字段改变不更新该视图；删除实体会失效全部视图。
- 视图载荷是完整 BSON Snapshot，并非客户端 PB 增量；客户端 EntitySync 仍走既有 PB 路径。
- 视图名称不是权限系统；不生成跨 DAO 聚合或派生计算。改字段形状会改变视图 schema，不兼容旧快照，也不自动重建已有缓存。

完整类型范围、注册、版本水位和升级边界见 [Remote 指南](../../docs/framework/guide/05-remote-mirror.md)。

### 必填的 store 键与变更告警（v1.40.0 起）

`//roost:dao`（Mongo 或内存 DAO）及其嵌套类型使用必填的专用 `store` tag 声明稳定键，推荐数字短键；业务代码仍用可读的 Go 名称：

```go
//roost:dao coll=players db=game schema=2
type PlayerDao struct {
    Experience int64                 `store:"1"`
    Equipment  map[int64]*Equipment   `store:"2"`
}

type Equipment struct {
    Strength int32 `store:"1"`
    Quantity int32 `store:"2"`
}
```

- 存入 Mongo 的键是字符串 `"1"`、`"2"`，不是 PB varint 编号；嵌套更新如 `2.7.1`，含义是 Equipment[7].Strength。
- 显式键允许原有字母开头标识符，或无前导零的正十进制数字；拒绝 `0`、`01`、`1name`、`.`、`$`、框架保留 `_` 前缀和同一类型内重复键。嵌套类型可以独立从 1 编号。
- `store` 对每个生成 DAO / 嵌套字段必填，包括 nocoll、nopersist；`dao:"-"` 排除的字段例外。缺失、空值或重复键立即报错。生成物内部仍输出 Mongo 驱动需要的 `bson`，业务定义不得直接写 `bson`。
- 由业务维护稳定编号，不按字段顺序自动分配；也允许显式字母键。仓内已有定义保留原物理键，不在换 tag 时隐式重编号。删字段后不要把它的键复用于另一种含义；改 Go 名可以保留原键。
- 已有 DAO 改键必须手工提升 `schema=` 并离线迁移所需数据，或使用新库；生成器不自动提升 schema，不兼容读旧键。只把 `bson:"原键"` 改写为 `store:"原键"` 不改变数据格式。
- PB 字段名/编号仍由同步定义生成，`store:"1"` 不等于 PB field 1，也不会进一步缩小 PB。通用 `SyncFields().WireName` / `Values()` 的字典键沿用 BSON 名，按字符串选字段的 Profile 或业务消费者要一同更新。
- 节省的是重复字段名、更新路径和未压缩 BSON 字节；不能据此承诺 Mongo CPU、磁盘或端到端 TPS 等比例改善。详见[对照报告](../../docs/performance/DATAENGINE-PERSIST-PATHS.md)。

重新生成时，键或 `omitempty` 变化会打印 `WARN: <类型>.<字段>: store tag changed "旧值" -> "新值"`，包含 schema/消费端检查提示。同类型字段交换键、嵌套字段及非持久字段也会报警；改 Go 字段名但保留相同键不报警。仅报警，不自动改数据或提升 schema；声明校验失败不会覆盖上一次产物。

告警比较输出目录中上一次生成的持久/回滚结构体和嵌套文档，须把生成物纳入版本控制。首次生成、删除基线或换到无基线目录无法判断历史变化；同一定义第二次生成不再重复报警。它不是运行期监控服务，也不维护跨所有 Git 历史的编号账本。此规则不改 `//roost:redisdao` 包装器或手写 Mongo 编解码结构体。

## 4. Entity：装配与执行策略

```go
//roost:entity id=1 entityKind=EntityKindPlayer category=entity.EntityCategoryOther businessPool=short
type Player struct {
    *entity.EntityBase
    entity.ComponentManager
    entity.DaoManager
    profile *ProfileComponent `comp:"CompTypeProfile"`
    dao     *db.PlayerDao      `dao:"players"`
}
```

这是结构示例，组件工厂、kind 常量和 import 由工程定义；优先通过 `roost add entity/component/dao` 创建完整接线。

| 参数 / tag | 含义 |
| --- | --- |
| `id` / `entityKind` / `category` | 类型编号、kind 表达式、category 表达式；必须与注册的 kind/category 一致 |
| `businessPool=short` | 默认短业务池；`long` 为长业务池。多目标有 long 则整条消息走 long；io 不是业务池 |
| `remote=managed` | Remote 托管，配套 `RemoteEntityBase`、Remote category 与运行时 Mod；普通实体用 `none` |
| `noPersist=true` | 声明实体不持久；不代替 DAO 字段逐项表达意图 |
| `lifetime` | `ephemeral`、`runtime_rebuild`、`persisted_hot_cold`、`resident`、`remote_managed`；应与实际持久与 Remote 模式一致 |
| `sync=true` | 生成 EntitySync builder 接线 |
| `syncNamespace` | 同步 wire namespace |
| `subjectPacker` | packer 工厂表达式；只有 `sync=true` 时可写 |
| `service` | 归属服务：只有这个服务写这个 kind（见下段）。不写时保持原行为，任何带 Nest 的服务都能写 |
| `preload=startup` | 起服预加载：归属服务起服时加载持久 owner 是本 sid 的这个 kind 的全部实体并登记路由。只能写在 `remote=managed` 且声明了 `service=` 的实体上；不写就按需冷加载 |
| `comp:"CompTypeProfile"` | 组件类型号表达式；可加 `,cold` 声明按需初始化 |
| `dao:"players"` | DAO 装配名称；可加 `,cold` 声明按需初始化，不是 DAO 定义字段的能力选项 |

`service=<name>` 只在实体标记上声明一次（不在 `roost.yaml` 再写一份）。取值必须是 `roost.yaml` 声明的业务服务、带 Nest；`remote=managed` 时还要有 `remote_entity`，否则 `roost generate` 报出实体与服务名。声明之后：锁住这个 kind 的 `//roost:nest` handler 只在归属服务的 Nest 引擎上注册（生成 `Register<File>NestHandlersOn(mgr)`，由 `internal/registry/ownership_gen.go` 的 `NewOwnedNestHandlersMod` 在归属服务的 Provide 里注册），别的服务发过去得到“handler 未注册”；`remote=managed` 的归属 kind 进 `ForeignKinds(service)`，每个装 `remote_entity` 的服务的 RemoteEntityMod 带 `WithForeignKinds(registry.ForeignKinds("<service>")...)`，别的服务的进程认领与写许可前被拒（`entity.ErrRemoteKindNotOwned`）；本地 kind 的 `service=` 只影响 handler 注册；`roost add lifecycle` 缺省落到归属服务；doctor 的 `entity-owner` 检查别的服务构造了它的 Sender。一个 handler 锁住两个不同服务的 kind、同一源文件里 handler 归属不一致、方法 handler 或原生步骤 handler 锁住归属 kind、同一实体包里两个实体声明不同服务，都在生成期拒绝。改了 `service=` 先 `roost project sync` 再 `roost generate`（bootstrap 与生成物不一致时 generate 拒绝并给出这条命令）。

归属服务拥有的 `remote=managed` kind 在实体加载后登记 Redis 路由（`registry.RouteKinds("<service>")`），它们之中写了 `preload=startup` 的起服预加载（`registry.PreloadKinds("<service>")`）；两份列表都在 `ownership_gen.go`，与 `ForeignKinds` 同一次扫描（联盟服务方案 §2.7）。路由是方法级策略：标了 `//roost:rpc route` 的方法要求它所在 rpc 的拥有者托管这类 kind，不标 route 的 rpc 不受影响（见 [CODEGEN_REFERENCE](CODEGEN_REFERENCE.zh-CN.md)）。

`remote=mirror`、`lifetime=mirror_cache` 不再生成实体。读已有完整 DAO 镜像时用 `//roost:mirror entityKind=... coll=...` DTO；按字段发布视图时用上一节的 `remoteview`。旧 `syncPacker` / `syncTopic` 不应使用，当前对应 `subjectPacker` / `syncNamespace`。

## 5. Nest 与 Remote 请求

```go
//roost:nest rollback=undo durability=strict
func handlerRename(player IPlayerEntity, name string) error {
    // 通过组件调用 DAO setter；进入时目标已加锁。
    return nil
}
```

| 选项 | 缺省 / 合法值 | 意义 |
| --- | --- | --- |
| `target` / `targets` | 自动推断；显式可写单个 / 逗号分隔目标 | 声明调度、锁定的实体参数；推断识别类型名含 Entity 的参数及其切片 |
| `rollback` | 默认 `state`；另有 `undo` | 全状态回滚或 setter 逆操作回滚 |
| `durability` | 默认 `async`；`memory`、`async`、`strict`、`pipelined` | 内存、WAL 准入、严格持久确认、流水线持久确认 |
| `sync` | 默认不启用 | 生成同步 Sender；不决定数据同步频率 |

`strict` 的本地 WAL 持久不等于 Mongo 已投影；`pipelined` 仍受运行时适用范围与准入约束，不是给任意 handler 加上的性能开关。原生 Saga 步骤要求 `strict` 或 `pipelined`。具体执行顺序见 [Nest 业务执行模型](NEST_RUNTIME.zh-CN.md)。

### PB 参数可由类型识别，不新增 tag

单玩家 handler 可直接接收 `*pb.AddGoldRequest` 并返回 `(*pb.AddGoldResponse, error)`，再用 `roost add endpoint AddGold --handler player` 创建转交代码。目标仍取认证会话，业务时间仍由 Nest 注入；只有当前协议、当前工程 PB 包的指针类型才匹配。手写入口不覆盖；新建标准 endpoint 上自动附加 `//roost:endpoint nest=AddGold`，用于持续生成独立的 `*_endpoint_gen.go` 转交。示例、所有权与空响应边界见[命令参考：PB 请求与响应直接接入 Nest](CODEGEN_REFERENCE.zh-CN.md#pb-请求与响应直接接入-nest)。

### 请求字段：`remote`

以下放在扫描范围内的共用请求包，例如 `game/requests`；**请求 struct 不需要额外的 `//roost` 标记**。

```go
import (
    "example.com/game/db"
    "github.com/tjbdwanghaibo/roost-core/framework/entity"
)

var _ db.AllianceMemberSnapshot // tag 不算 Go import 使用

type AllianceRead struct {
    AllianceViewRef entity.RemoteViewRef `remote:"db.AllianceMemberSnapshot,required"`
}
```

生成 `RemoteAccess()`、`Alliance()` / `MustAlliance()`；同一个请求可供多个 handler 和 Sender 使用。Nest 在 I/O 池准备视图，业务池访问器读取已准备快照；不赋予 Remote 写权限。

| tag 内容 | 缺省与用途 |
| --- | --- |
| `db.AllianceMemberSnapshot` | 必填的 Snapshot 类型，使用源文件的包别名 |
| `required` | 缺省可选；启用后缺失视图不能继续执行 handler |
| `consistency=monotonic` | 默认；也支持 `cached`、`strong`，实际保证由正式 resolver 提供 |
| `alias=alliance` | 预取键别名；默认从字段名去掉 Ref / ViewRef 等后缀推导 |
| `accessor=Alliance` | 生成访问器名；默认从 alias 推导 |
| `min_version=req.Version` | 最低版本表达式；未写则取引用中的 Version |
| `ttl_ms=1000` | 缓存 TTL 毫秒表达式 |
| `tenant=req.Tenant` / `policy=req.Policy` | 租户 / 策略表达式，交给 resolver |
| `allow_stale` | 允许陈旧结果的请求意图，仍受一致性模式和 resolver 约束 |

表达式中的请求接收者固定叫 `req`，不能写 handler 的局部参数名。不要用当前 Entity 总版本要求一个没有变化的字段视图；最低版本应来自已观察的该视图水位。`remote:"...,write"` 不支持；写入需声明真正的 Remote Entity 目标。`required` 不是公会成员权限校验。

## 6. 业务 PB、HTTP 与服务 RPC

### PB

```go
//roost:proto package=game.protocol go_package=example.com/game/protocol/pb;pb

//roost:protocol group=game handler=player
type GameProtocol interface {
    //roost:msg id=10001 tags=local
    Ping(PingRequest) PingResponse
}

type PingRequest struct { ClientTime int64 `pb:"1"` }
type PingResponse struct { ServerTime int64 `pb:"1"` }
```

`pb:"N"` 是字段号，不是消息号；`pb:"-"` 排除字段，`pb:"N,oneof=choice"` 声明 oneof 分组。字段号与消息 ID 各自查重，不能随意重排。`tags=local` 是协议分类元数据，不等于自动授权。只有请求参数的方法表示客户端推送，只有返回值的方法表示服务端通知。

### HTTP

```go
//roost:web method=POST path=/player/query body=json headers=Authorization:Token status=200
```

`method` 支持 GET / POST；GET 用 `body=raw`，JSON 模式用 `body=json`。`headers` 为逗号分隔的 `头名:字段名`，覆盖 JSON 中对应 string 字段，缺头时写空串。`compress=gzip` 按客户端支持和响应大小压缩；`errors` 指向本包错误映射器；`status` 为 2xx 或 `response`。handler 签名与完整示例见[命令参考](CODEGEN_REFERENCE.zh-CN.md)。

### 服务 RPC

接口声明用 `//roost:rpc service_type=mail capability=service.mail`，方法可写 `//roost:rpc affinity=playerID` 或 `affinity=request.Key()`。裸 affinity 参数要求 string；接口方法首参为 context，参数和返回值需命名。

**不支持**方法级 `reliable`：它虽出现在解析词表中，但后续校验明确拒绝；JetStream 通过 owner / client 的 `nats.rpc.transport=jetstream` 配置。不要把“解析器认识这个词”当成“业务可以使用”。

## 7. 配置表与属性

| 所在定义 | 标记 / tag | 用途 |
| --- | --- | --- |
| 配置表 struct | `//roost:table name=monster file=monster.csv json=monster.json key=ID` | 多行表，key 是 Go 字段名 |
| 单例配置 struct | `//roost:object name=world file=world.csv json=world.json` | 最多一行 |
| 配置字段 | `csv:"id" json:"id" title:"编号"` | CSV 列名、JSON 键、策划标题 |
| 配置字段 | `required:"true" unique:"true"` | 键须存在且非 null、唯一值；required 不等于禁止数值 0 |
| 配置字段 | `min:"1" enum:"a\|b\|c" ref:"monster"` | 下限、枚举、引用目标表主键 |
| 配置字段 | `parser:"json"` | 当前实现只按是否非空选择 JSON 解析，不按 tag 内容寻找自定义 Go 函数；推荐统一写 json |
| 属性 profile | `//roost:attribute index=1 max=64` | 属性编号起点、最大字段数 |
| 属性字段 | `attr:"hp"` / `attr:"-"` | 属性名 / 排除字段；省略名称用 snake_case |

表头和 JSON 键大小写敏感；规则在 CSV 转换与运行时加载 / reload 都检查。cfggen 使用 YAML schema，不使用 tablegen 的 Go tag，两套输出不能混用。属性只支持整数，profile 需声明 `dirtyMask uint64`；`_Power` 这样的下划线方法表示派生公式，不需要新的字段 tag。

## 8. 注册、生成与排错

```go
//roost:register phase=component order=-10
func RegisterComponents() error { return nil }
```

注册函数必须导出、包级、无参数，返回空或单个 error。执行阶段顺序为 `pre → kind → config → component → entity → protocol → nest → route → post`；同阶段按 order（默认 0）再按稳定顺序排列。生成器自己的注册函数通常已经带标记，业务不要重复注册。

推荐检查顺序：

1. 确认标记放在正确声明上，定义文件属于 `roost.yaml` 配置的扫描目录，feature 已启用。
2. 先生成 DAO，再 Entity，再 Nest；使用 `roost generate` 管理依赖顺序。
3. 检查 Snapshot / DTO import：tag 不算 Go 代码引用；共享请求不要定义在 handler 包造成 Sender 导入环。
4. 运行 `roost generate --check`、`roost id check`、工程编译与对应场景；生成成功不代表运行时 Mod、resolver 或授权已经接好。
5. 修改字段、schema、编号前检查存储 / 网络影响；不靠保留旧生成文件兼容旧格式。

维护本文时核对[注释参数词表](../internal/marker/options.go)与所属解析器：DAO 的 [parse](../internal/dao/parse.go) / [remote_views](../internal/dao/remote_views.go)、Entity 的 [parse](../internal/entity/parse.go)、Nest 的 [parse](../internal/nest/parse.go)、Protocol 的 [parse](../internal/protocol/parse.go)、配置表的 [main](../internal/tablegen/main.go)。词表只定义语法，具体生成器还会做类型与语义拒绝；不要把未支持的键写成可用能力。
