# roost-codegen

> **维护线冻结（2026-09-20 起）**：本仓正在并入 roost-core（`core/codegen/` 与 `core/demo/`），`main` 只收 Bug 修复，其余改动进 `consolidation-v3` 分支。方案见 roost-core [三仓合一仓](https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md)。

roost 框架的项目脚手架与代码生成工具链：用源码里的标记注释（`//roost:dao`、`//roost:nest` 等）驱动一组 Go 代码生成器，把持久化、同步、协议、事件、配置表这些"写错就丢数据"的样板代码变成可重复生成、可 CI 校验的产物。版本与升级记录以 [CHANGELOG](CHANGELOG.md) 和仓库 release/tag 为准；生成器会在运行前校验框架兼容下限。

生成物依赖 [roost-core](https://github.com/tjbdwanghaibo/roost-core) 运行时（`dataengine`、`entity`、`nest`、`fmap` 等包），见[与 roost-core 的关系](#学习路径与-roost-core-的关系)。

文档按三级使用：完全新手先看 [小白逐步操作手册](docs/BEGINNER_WORKBOOK.zh-CN.md)，并在项目里反复运行
`roost project next`；熟练用户看 [项目生成器完整说明](docs/PROJECT_GENERATOR.zh-CN.md)；维护者再看生成器的
关键实现细节和 [Roost 实现原理](https://github.com/tjbdwanghaibo/roost-core/blob/main/docs/INTERNALS.md)。
框架仓库与生成项目的质量门禁、兼容矩阵、依赖升级、Release，以及 Shell/Docker/Kubernetes 三种交付方式见
[CI/CD P0/P1 实施方案](docs/CI_CD_IMPLEMENTATION.zh-CN.md)。

完全第一次使用先看 [小白逐步操作手册](docs/BEGINNER_WORKBOOK.zh-CN.md)；
逐个命令、marker、输入输出和完整用例见 [全功能使用手册与用例](docs/CODEGEN_REFERENCE.zh-CN.md)；
玩家 TCP listener、鉴权、主动推送与生产验收见 [Player TCP 接入](docs/PLAYER_ACCESS_TCP.zh-CN.md)；
cfggen 的 schema 字段级参考见 [CFGGEN_META](docs/CFGGEN_META.zh-CN.md)；
生成工程里"一次请求的字节往哪走、每层守什么、失败在哪一层处理、重放靠什么幂等"见
[数据流：六条路径](docs/DATA_FLOW.zh-CN.md)。

## 生成器总览

| 生成器 | 输入标记 / 约定 | 产出 | 解决什么问题 |
| --- | --- | --- | --- |
| `dao` | `//roost:dao`、`//roost:redisdao` + 字段 `dao:"..."` tag | `gen_<name>_dao.go`、`gen_<name>_nested.go`、`gen_<name>_redis_dao.go` | 持久化/同步字段的 dirty 追踪、字段级 patch、事务 rollback，全部经生成的 mutator 收口 |
| `protocol` | `protocol/def` 下的 `//x:proto` `//x:protocol` `//x:msg` `//x:view` 标记 | `.proto`、pb Go 代码、msgid、player_bind、协议 handler、robot 注册表、协议 manifest | 用 Go struct 单一来源维护协议，也支持从 `.proto` 反向生成（`-reverse-proto`） |
| `entity` | `//roost:entity entityKind=...` | `<entity>_gen_wire.go` | Entity 工厂、组件/DAO 装配、Snapshot、hook 接线 |
| `nest` | `//roost:nest target=... [sync] [rollback=state\|undo] [durability=memory\|async\|strict]` | `<source>_nest_gen.go`、`sender/`、`syncsender/`、`game/bootstrap/nest.go` | handler 的 invoke 包装、显式注册与可注入的类型化 Sender |
| `eventgen` | `event/def` 下 `Event` 前缀 struct + game 目录中 `DealEventXXX` 方法 | `event_def_gen.go`、`event_type_gen.go`、`event_type_impl_gen.go`、`<receiver>_event_gen.go` | 事件类型常量、`Type()` 实现与订阅分发代码 |
| `tablegen` | `configs/schema` 下 `//roost:table`、`//roost:object`（Go 元数据先行） | Go 访问代码（`configs/generated`）、CSV 模板、CSV→JSON 数据 | 配置表 schema、策划填表模板与运行时数据的一致性 |
| `cfggen` | 一个 YAML schema 文件（**meta 文件先行**，类似简化版 Luban）：表/对象/bean、字段类型、key/index/ref | 单文件 Go 绑定（行 struct 带 `cfg` tag、`RegisterGeneratedConfigData`、类型化访问器） | 配置定义零手写 Go：只写 meta + 一行注册；ref 在生成期查表存在性与类型一致、运行期由 configdata 查悬空引用 |
| `attribute` | `//roost:attribute` | `gen_<profile>_attribute.go` | 属性 profile 的计算/聚合代码 |
| `webroute` | `//roost:web` | 路由注册代码 | HTTP 路由注册不再手写 |
| `errcode` | 扫描 `errcode.Define(code, name, msg)` 调用 | `docs/generated/errcode.csv` | 错误码清单导出与查重 |
| `servicerpc` | `//roost:rpc service_type=... capability=...` 打在**手写的服务接口**上，方法上可选 `//roost:rpc affinity=...` | 两个文件：`<接口名小写>_rpc_gen.go`（线上类型、handler 注册、打字的 `BusClient`、capability 包装与名字，只依赖 roost-core）与 `<接口名小写>_rpc_assembly_gen.go`（`Server`、`OwnerCapabilities`、`ClientMod`，依赖 roost-kit/mods；M-10）。`-emit transport|assembly|all` 与 `-out` 让两半分开生成；`-dir` 可以是 import path（接口住在 core 领域包、装配住在 kit 时用，M-11） | 服务拆成独立进程后，调用方只查接口（`app.Lookup[mail.Mail]`），两种实现都满足它；传输层从接口本身生成，所以**漂移结构上不可能**而不是被检测到 |
| `roost id` | 扫描各类标记中的 `id=/kind=/type=/code=` | —（校验命令） | 按 `roost.yaml` 声明的 ID 空间检查冲突、分配下一个可用 ID |

所有生成器都可以独立运行（`cmd/<name>`），也可以由 `roost generate` 按依赖顺序统一编排：DAO → Event → Errcode → Protocol → Entity → Nest → Attribute → Config → WebRoute。

### servicerpc 速览（接口先行，不另立 def 文件）

```go
// mail/mail.go —— 唯一手写的跨进程契约
//
//go:generate go run github.com/tjbdwanghaibo/roost-codegen/cmd/servicerpc -dir .
//
//roost:rpc service_type=mail capability=service.mail
type Mail interface {
	Send(ctx context.Context, req SendRequest) (envelope Envelope, err error)
	//roost:rpc affinity=queue.Key()
	List(ctx context.Context, playerID int64, cursor string, limit int) (page Page, err error)
}
```

拥有者进程：`mods.RegisterAll(r, OwnerCapabilities(service)...)` + 一个手写的
`func (s *Server) run(ctx context.Context) error`。调用方进程：`mail.NewClientMod()`。
两者发布同一个 capability 名，所以**同一个进程里放不下两份**——registry 会拒绝第二个，
这正是想要的行为。

`-check` 用于 CI：先跑全部拒绝规则，再生成并**逐字节比对**磁盘上的文件，任何差异
非零退出，并区分"缺失"（没人跑过生成器）与"不一致"（文件被改过，或由另一个版本的
生成器产出）。它不写文件——一个会写的 check 模式第二次跑就变绿，门不住任何东西。

**它拒绝什么，和它生成什么一样重要**（每条都对应一个真实踩过的坑）：

| 拒绝 | 为什么 |
| --- | --- |
| 未命名的参数/返回值 | 线上类型没有字段名可用 |
| `time.Duration` / `time.Time` | 纳秒整数抓包读不懂，非 Go 调用方造不出。校验**递归进包内结构体**，因为真实形状是"Duration 藏在整体传递的 request 里" |
| **未导出字段** | 任何 codec 都会静默丢掉它，对面拿到的值不完整而不是报错 |
| 未导出类型、`any`、chan、func、嵌套超过 8 层 | 同上，或根本无法编码 |
| 首参不是 `context.Context` | 一个不接 ctx 的方法不做 I/O，没有理由是一次往返 |
| 包里没有 `ErrRequestInvalid` | 生成的 handler 需要一个带码的错误来回答"这一帧我读不懂"；用 `CodeInternal` 回答会把调用方的畸形请求报成服务端故障 |
| `affinity=` 指向不存在的参数，或裸 key 不是 string | 亲和 key 是在调用时从实参读的；派生形式写 `affinity=x.Key()` |
| **生成名与包内已有声明撞名** | `account` 有 `type Server struct`（服务器列表的一行），生成的进程壳也叫 `Server`。编译器只会说 "Server redeclared in this block" 并指向生成的文件——它告诉你重复在哪，不告诉你为什么在那儿、哪一边能动。生成的那边不能动：`pkg.Server` 在每个服务里都要读法一致。撞名的若是**函数或常量**更危险，它能在什么都编不过之前先改掉整个包的含义 |
| **一个包里两个被标记的接口** | 生成文件在包级别声明十几个固定名字，两份就是每个都声明两次。这条只是把一件早就成立的事说出来：`app.Service` 每进程一个，所以两个被标记的接口是两个独立部署的东西，而那应该是两个包。备选方案（按接口加前缀）会让 `pkg.Server` 在有些包里叫 `pkg.FooServer`，把成本转给每个读者，只为省这一次拆包 |

### cfggen 速览（meta 文件先行）

```yaml
# configs/schema/cfg.yaml —— 唯一手写的配置定义
package: cfg
tables:
  - name: monster
    key: id
    fields:
      - { name: id,       type: int32 }
      - { name: name,     type: string }
      - { name: scene_id, type: int32, index: true }
      - { name: drop_id,  type: int32, ref: drop }
```

```bash
go run github.com/tjbdwanghaibo/roost-codegen/cmd/cfggen -meta ./configs/schema/cfg.yaml -out ./cfg
```

业务侧接线只剩一行：`cfg.MustRegisterGeneratedConfigData(reg)`；读取用生成的 `cfg.MonsterTableFrom(snap)`，二级索引生成强类型访问器 `cfg.MonsterBySceneID(snap, 7)`（参数类型来自字段，不传索引名和字符串值）。运行期的悬空引用校验、原子热更、回滚由 roost-core/configdata 承担（`RegisterAutoTable` + `cfg` tag）。接真正 Luban 的方式见 roost-core `examples/lubanreal`（官方 luban CLI 真实生成的端到端示例）。

## 快速启动

### 1. 安装 roost CLI

推荐直接跟随最新 codegen，不需要把 codegen 加进业务 module：

    go run github.com/tjbdwanghaibo/roost-codegen/cmd/roost@latest help

或安装本地命令：

    go install github.com/tjbdwanghaibo/roost-codegen/cmd/roost@latest
    roost version
    roost env doctor
    roost help beginner

Windows 下使用仓库内脚本（不修改现有 `GOBIN`/`GOPATH`，只把 `go install` 实际使用的 Go bin 目录追加进用户 PATH）：

    powershell -ExecutionPolicy Bypass -File .\scripts\install-windows.ps1
    roost help

完整步骤见 [Windows 安装与 PATH](docs/PROJECT_GENERATOR.zh-CN.md#windows-安装与-path)。

### 安装后的内置帮助

`roost help` 会列出 CLI 和所有生成器能力；不需要打开仓库文档即可查询用途、参数、配置/marker 和最小示例：

```bash
roost help                    # 能力总览
roost help beginner           # 从空项目到玩家协议/Nest/Entity 的完整路径
roost help environment        # 版本、Go/Git/Make/Docker 与 PATH 自检
roost help access             # 玩家接入层与 transport 边界
roost help transport          # 有界 TCP、鉴权、主动推送和探针
roost help endpoint           # Protocol 到 Nest Sender 接线
roost help lifecycle          # Entity 加载、创建、销毁
roost help skill              # 稳定 roost-skill JSON 与 catalog
roost help dao                # DAO 配置、tag、命令和例子
roost help entity             # Entity marker 和装配例子
roost help nest               # handler、rollback、durability
roost help protocol           # 协议定义与输出目录
roost help versions           # latest 策略和最低版本
roost help all                # 输出全部专题

roost project --help          # 顶级命令上下文帮助
roost project deps --help     # 等价于 roost help versions
roost add dao --help          # 等价于 roost help dao
```

内置专题包括：`environment`、`beginner`、`project`、`versions`、`framework-release`、`generate`、`add`、`mods`、`access`、`transport`、`lifecycle`、`endpoint`、`skill`、`dao`、`entity`、`nest`、`protocol`、`cfggen`、`tablegen`、`eventgen`、`attribute`、`webroute`、`errcode`、`saga`、`replication`、`config`、`id`、`format`、`deploy`。常用别名也可查询，例如 `roost help start`、`roost help release-train`、`roost help player-tcp`、`roost help protocol-to-nest`、`roost help k8s`。

专题输出固定包含四部分：用途、命令、配置/标记和示例。未知能力会失败并提示先运行 `roost help`，适合脚本和 CI 发现拼写错误。

新项目可以一条命令在本机起全部服务：`make dev-up`（基础设施 compose）→ `make dev-run`（`deploy/dev/run.sh`：按托管服务 → 业务服务的顺序编译并启动每个进程、等各自 `/readyz`；每个服务的本机配置有自己的 ops 端口）→ game-demo 工程再 `make dev-smoke`（两个机器人走完整条链）；`make dev-status` / `make dev-stop` 查看与停止。

新项目 Makefile 同时提供四个更新入口：

```bash
make project-upgrade # 使用 @latest codegen 升级工程模板
make deps-update  # 按 roost.yaml 更新 core、kit、skill
make roost-up     # GOWORK=off go get -u ./...；然后 go mod tidy
make codegen-up   # go install roost-codegen/cmd/roost@latest
```

`project-upgrade` 直接通过 `@latest` 运行生成器，更新所有带生成标识的受控工程文件，并把 core、kit、skill、codegen 四个版本策略都设为 `latest`。`roost-up` 会更新项目实际引用的直接和间接依赖，变更 `go.mod/go.sum`；执行后应运行 `make ci` 并提交依赖文件。`codegen-up` 更新的是 Go bin 目录中的 `roost` 可执行文件。

旧项目的 Makefile 尚无 `project-upgrade` 时，先预览并执行一次迁移：

```bash
go run github.com/tjbdwanghaibo/roost-codegen/cmd/roost@latest project upgrade --root . --dry-run -core latest -kit latest -skill latest -codegen latest
go run github.com/tjbdwanghaibo/roost-codegen/cmd/roost@latest project upgrade --root . -core latest -kit latest -skill latest -codegen latest
```

迁移会把 `roost-up`、`codegen-up` 和其他当前工程指令一起写入 codegen 管理的 Makefile。没有生成标识的自定义 Makefile 会被拒绝，不会被静默覆盖。

创建新项目（生成 `roost.yaml` 描述 Service、Kit Mod、生成能力、版本和 ID 空间）：

    roost project new planet -module github.com/your-account/planet -services game,gate \
      -mods configdata,etcd,redis,mongo,nats,room,remote_entity

完全新手可以先使用 `-mods configdata` 创建轻量项目来学习生成流程。生成结果内含
`docs/QUICKSTART.zh-CN.md` 零基础教程、`docs/BEGINNER_WORKBOOK.zh-CN.md` 逐步操作手册、
`docs/FIRST_BUSINESS.zh-CN.md` 第一条完整业务链和
`docs/ROOST_YAML.zh-CN.md` 全字段参考，
不需要先理解全部 MongoDB、Redis、NATS、Nest WAL 和跨服能力。
进入项目后的第一条命令应是 `roost project next`（或 `make next`）；它根据真实进度只给一个动作和原因。
默认严格 doctor 会验证完整业务链，因此不要把尚未填写业务方法的空骨架误判成已完成。
开始生成 Nest handler 前，
执行 `roost add access player --service game`；它会补齐 Nest 运行依赖，生成器也会校验，避免产生半成品配置。
需要真实 TCP 接入时再执行 `roost add transport tcp`；生成层提供有界帧、连接/超时限制、背压和优雅关停，
并自动把 disabled 配置加入 Service。应用实现默认拒绝的鉴权文件后执行
`roost config enable player-tcp`，无需手工合并 YAML。

`-features`（生成哪些代码：13 个开关）与 `-mods`（运行时装配哪些 Kit Mod：14 个，依赖自动展开）的完整清单见 [docs/PROJECT_GENERATOR.zh-CN.md](docs/PROJECT_GENERATOR.zh-CN.md) 第 4、5 节。

新项目的 `roost.yaml` 默认将 core、kit、skill、codegen 都声明为 `latest`。`project new/sync/deps` 会把 core、kit、skill 作为三个直接依赖一次性执行 `go get ...@latest`，再 `go mod tidy`；因此 kit/skill 的旧下限不会把底层 core 降级。Go 不允许在 `go.mod` 的 `require` 中写查询值 `latest`，所以落盘的是本次解析出的具体版本与 `go.sum`，策略仍保存在 `roost.yaml`。普通 PR CI 只验证已经提交的具体依赖图；每周 `dependency-update.yml` 才会解析 latest、执行完整测试并创建升级 PR，正式发布继续使用已评审和测试过的同一依赖图。

每个新项目默认生成可直接启用的 CI/CD：

- `ci.yml`：Linux/Windows、race、生成幂等、全 Service 配置、ShellCheck、Compose、Kustomize 和镜像构建。
- `security.yml`：govulncheck、Dependency Review 和 CodeQL。
- `release.yml`：Linux amd64/arm64 包、SHA256、SBOM、provenance 和多架构 GHCR 镜像。
- `deploy-shell.yml`、`deploy-docker.yml`、`deploy-k8s.yml`：使用 staging/production Environment 晋级同一不可变制品。

所有第三方 GitHub Action 固定完整 commit SHA，并由 Dependabot 提交升级 PR。执行 `make cicd-check` 可在提交前检查业务代码和三套部署模板；具体 runner、Environment 和 Secret 配置见生成项目的 `docs/DEPLOYMENT.zh-CN.md`。

框架维护者发布 codegen 前还要执行发布列车校验：

```bash
roost framework verify --manifest ci/framework-release.yaml --expected-codegen v1.10.0 --lock framework-lock.json
```

tag 触发的 `framework-release` workflow 会锁定 core/kit/skill 精确版本，拒绝 replace 和框架内部伪版本，
按清单中的 `consumer_go` 动态矩阵生成真实消费项目，并对 Linux、Windows、macOS CLI 做 smoke。通过受保护 Environment
后才发布多平台压缩包、checksum、SBOM、provenance 和 framework-lock。详细约束见
[CI/CD P0/P1/P2 实施方案](docs/CI_CD_IMPLEMENTATION.zh-CN.md)。

### 2. 写一个带 dao 标记的 struct

在 `db/def/hero.go` 中定义（`roost add dao hero` 可以生成初始骨架）：

```go
package def

//roost:dao coll=heroes db=game dbscope=sid
type HeroDao struct {
	Name    string                          // 无 tag：persist + sync 全开
	Level   int32
	Exp     int64  `dao:"persist"`          // 只持久化，不同步给客户端
	Cache   int64  `dao:"nopersist,sync"`   // 只同步，不落库
	Tmp     int    `dao:"-"`                // 完全排除
	Items   map[int64]int32 `dao:"persist,sync,map=fast"`
	Friends []int64
	Equips  map[int64]*EquipInfo            // 嵌套 struct 自动递归生成
}

type EquipInfo struct {
	Level int32
	Star  int32
}
```

### 3. 运行生成

统一入口（要求项目根有 `roost.yaml`）：

    roost generate

或独立运行 dao 生成器：

    go run github.com/tjbdwanghaibo/roost-codegen/cmd/dao@latest -def ./db/def -out ./db

输出（输出目录需能推断出包名，即已有至少一个非生成的 `.go` 文件，否则用 `-pkg` 指定）：

    generated: db/gen_hero_dao.go
    generated: db/gen_equip_info_nested.go

再跑一次，内容没变就什么都不写（mtime 不动，增量构建保持热缓存）：

    unchanged: db/gen_hero_dao.go
    unchanged: db/gen_equip_info_nested.go

### 4. 生成物长什么样：dirty / undo / patch 三件套

生成的 DAO 使用**私有存储字段**，业务只能通过生成的 accessor/mutator 读写，无法绕过 dirty、rollback 与同步语义：

```go
// Code generated by tool/dao. DO NOT EDIT.
type HeroDao struct {
	id      int64
	tracker dataengine.Tracker // ① transaction-local persist + sync mask
	name    string
	items             *fmap.FastMap[int64, int32]        // map=fast
	equips            *fmap.SmallSafeMap[int64, *EquipInfo]
	// ...
}

var _ entity.DaoInterface      = (*HeroDao)(nil)
var _ nest.RollbackSnapshotter = (*HeroDao)(nil)
var _ nest.MutationParticipant = (*HeroDao)(nil)
```

**① tracker——每个字段一个掩码位，持久化变化进入当前 Nest transaction，同步变化留在 DAO tracker。** setter 先比较再标记；事务外修改持久化字段会 fail fast：

```go
const (
	heroDaoFieldName uint64 = 1 << iota
	heroDaoFieldLevel uint64 = 1 << iota
	// ...
)

func (d *HeroDao) markExpDirty() {
	if err := nest.MarkPersist(d, heroDaoFieldExp); err != nil {
		panic(err)
	}
	d.tracker.MarkSync(heroDaoFieldExp)
}
func (d *HeroDao) markCacheDirty() {
	d.tracker.MarkSync(heroDaoFieldCache) // dao:"nopersist,sync"
}
```

**② undo——mutator 在 undo 策略的 Nest 事务里先登记逆操作再改值**（`rollback=undo` 的 handler 回滚时逐字段撤销，不做全量快照恢复）：

```go
func (d *HeroDao) SetLevel(v int32) {
	if d.level != v {
		if tx := nest.CurrentRollbackTx(); tx != nil && tx.Policy() == nest.RollbackUndo {
			old := d.level
			d.recordUndo(tx, heroDaoFieldLevel, func() error { d.level = old; return nil })
		}
		d.level = v
		d.markLevelDirty()
	}
}
```

`recordUndo` 注册失败会直接 panic：一次逃出回滚覆盖范围的修改会静默破坏事务保证，宁可炸在现场。同理，`MarshalPersist` / `MarshalSync` 内部 `bson.Marshal` 失败也会 panic——接口没有错误位，返回 nil 等于把数据静默丢掉。

**③ patch——map 的键级修改记录在当前事务的 `PersistChange` 中，投影为 MongoDB 路径级 `$set`/`$unset`**，而不是整字段重写：

```go
func (d *HeroDao) markItemsKeyDirty(key int64, val int32) {
	if path, ok := dataengine.MapPatchPath("items", key); ok {
		if err := nest.MarkPersistSet(d, heroDaoFieldItems, path, val); err != nil { panic(err) }
	} else {
		if err := nest.MarkPersistFull(d, heroDaoFieldItems, "items"); err != nil { panic(err) }
	}
	d.tracker.MarkSync(heroDaoFieldItems)
}
```

`PrepareMutation(change)` 将新建/migration/replace 编码为 Put，将普通修改编码为字段级 Patch，将删除编码为带 version 的 tombstone。Patch 不携带全量 fallback。

嵌套 struct（如 `EquipInfo`）生成到 `gen_<name>_nested.go`，内嵌 `dataengine.DirtyHook`；`Init()` 把它的修改通知接到父 DAO 的对应字段掩码上，深层修改照样进入事务 patch。

业务读写约定：标量与嵌套对象用 `Get<Field>()`，slice 用 `Get<Field>(idx)` / `Add<Field>` / `Set<Field>All` / `Range<Field>` / `<Field>Len`，map 用 `Get<Field>(key)` / `Set<Field>(key, val)` / `Del<Field>(key)` / `Range<Field>` / `<Field>Len`。

## DAO 标记语法参考

### struct 级标记

    //roost:dao coll=<collection> db=<database> [dbscope=global|sid]

- `coll=`、`db=` 必填；`dbscope` 默认 `global`，`sid` 表示按服分库（`dataengine.DatabaseServer`）。
- 标记必须**紧邻 struct**（相邻 1–2 行）或位于该 struct 的 doc 注释组内；标记和 struct 之间可以有普通文档注释。
- Redis DAO：`//roost:redisdao key=<字段路径> prefix=<键前缀> [mode=ref-hmap|raw] [key_type=...] [version=...] [ttl=<Go duration>] [name=...]`，其中 `key=`、`prefix=` 必填，`mode` 默认 `ref-hmap`，`key_type` 缺省时按字段路径从 struct 定义推断。

### 字段级 `dao:"..."` tag

| 写法 | 含义 |
| --- | --- |
| 无 tag | 默认 persist + sync 全开（安全默认值） |
| `dao:"-"` | 完全排除该字段 |
| `dao:"persist"` / `dao:"sync"` | 只开列出的能力，未列出的关闭 |
| `dao:"nopersist,nosync"` | 显式全关（字段仍在内存中，但不落库不同步） |
| `map=small\|fast\|sharded` | map 存储实现：`small`（默认，`fmap.SmallSafeMap`）、`fast`（`fmap.FastMap`）、`sharded`（`fmap.ShardedSafeMap`）；`fast`/`sharded` 仅支持 string 或整数 key |

**tag 一旦出现就必须表明 persist/sync 意图**：`map=...` 是辅助选项，必须与 persist/sync 意图同写。支持的字段类型：基础类型、slice、map、嵌套 struct（值或指针）；固定长度数组、`chan`、指向非本地命名类型的指针会在生成期被拒绝（提示用 `dao:"-"` 排除）。

### 常见错误与 fail-fast 报错

生成器对"看起来能过、实际埋雷"的写法一律报错而不是静默跳过。以下是真实运行输出（v1.6.0）：

只写辅助选项（v1.4 会静默关掉 persist+sync，是数据丢失陷阱）：

```go
Items map[int64]int32 `dao:"map=fast"`
```

    parse definitions: hero.go: dao HeroDao: field Items: dao tag "map=fast" does not
    state persist/sync intent; add persist/sync (or nopersist,nosync to disable both explicitly)

选项拼写错误（v1.4 会静默忽略）：

```go
Name string `dao:"persits,sync"`
```

    parse definitions: hero.go: dao HeroDao: field Name: dao tag has unknown option
    "persits" (valid: persist, nopersist, sync, nosync, map=small|fast|sharded, -)

孤儿标记——标记和 struct 之间隔了别的声明（v1.4 会静默丢弃，得到一个"不会持久化的 DAO"）：

    parse definitions: hero.go: line 3: //roost:dao marker is not attached to a struct
    (it must be adjacent to, or inside the doc comment of, a struct type declaration)

分组 `type (...)` 声明只放一个标记——第二个 struct 消费同一标记时报错（否则多个类型静默写进同一 collection）：

```go
//roost:dao coll=heroes db=game
type (
	AlphaDao struct{ Name string }
	BetaDao  struct{ Name string }
)
```

    parse definitions: hero.go: line 3: //roost:dao marker binds to multiple structs;
    grouped type declarations need one marker directly above each struct

缺必填参数：

    parse definitions: hero.go: line 3: //roost:dao on HeroDao requires coll= and db=

其余硬错误：`//roost:dao` 后无任何参数、`persist` 与 `nopersist` 同写、未知 `map=` 值、`map=fast|sharded` 配非 string/整数 key、字段私有存储名与框架保留名（`id`/`tracker`/`persistPatch*`）冲突。

### 孤儿生成文件清理

定义被删除或改名后，下一次生成会自动移除对应的旧生成文件（输出形如 `removed orphan: db/gen_hero_dao.go`），包括删光所有定义的情况。安全护栏：只有**同时满足**「文件名匹配本生成器的 `gen_*_dao.go` / `gen_*_nested.go` 模式」且「文件以 `// Code generated by tool/dao. DO NOT EDIT.` 头开始」的文件才是清理候选，手写文件永远不会被删。

## roost CLI 统一命令

    roost project new <name> -module <path> # 脚手架新项目；module 必填
    roost project sync|diff|doctor
    roost project upgrade [--root dir] [-core version] [-kit version] [-skill version] [-codegen version]
    roost generate                      # 按序执行 roost.yaml 启用的全部生成器
    roost generate --changed            # 只执行受 git 变更影响的生成器
    roost generate --check              # CI 门禁：临时副本中重新生成并比对
    roost generate --dry-run            # 打印生成计划
    roost generate --force              # 内容未变也重写输出
    roost add service|mod|access|transport|module|protocol|entity|component|handler|lifecycle|endpoint|skill|event|table|dao|webroute|errcode|saga
    roost config check --service <name>
    roost config enable|disable player-tcp
    roost project next [--workflow first-business|player-tcp]
    roost id next <kind> | roost id check
    roost format check

零基础首层业务不需要手写协议 Registry、Entity 工厂、Manager、import、字段 tag 或 Sender 接线：

    roost add access player --service game
    roost add transport tcp
    roost add entity Player
    roost add component Profile --entity Player
    roost add dao Player --entity Player
    roost add handler RenamePlayer --entity Player --component Profile
    roost add protocol RenamePlayer --group game --handler player
    # 编辑 Request 字段和 handler 参数后
    roost add endpoint RenamePlayer --handler player
    roost add lifecycle Player --service game
    roost generate
    roost project doctor --workflow first-business

Component 与 DAO 会自动挂到目标 Entity；Component 同时生成 `IProfileEntity` 这类窄接口，
Entity wire 生成对应 Component/DAO getter。业务在
Component 中写玩法方法，通过 DAO 的 `SetXxx` 等生成方法更新状态；Nest handler 进入前
已经持有 Entity 锁，不要在业务层重复加锁。生成项目中的
`docs/FIRST_BUSINESS.zh-CN.md` 提供完整可复制例子；`docs/PROTOCOL_TO_NEST.zh-CN.md` 解释边界契约，
`docs/PLAYER_ACCESS_TCP.zh-CN.md` 解释可运行 TCP 接入、帧协议、鉴权和上线验证。

`internal/frameworkdeps/generated.go` 只导入稳定包
`github.com/tjbdwanghaibo/roost-skill/skill`。旧项目若还保留 `/skillv2`，运行
`make project-upgrade`（或 `roost project sync`）会迁移受控生成文件。

Saga 脚手架：

    roost add mod saga -service game
    roost add saga AllianceRally -service game -steps ReserveTroops,CreateMarch,CreateRally

完整说明见[项目生成器使用说明](docs/PROJECT_GENERATOR.zh-CN.md)。

### Nest handler 标记（配合 dao 生成物）

新项目的 Nest handler 使用显式 capability 标记：

```go
type BagOwner interface { Bag() BagComponent }

//roost:nest target=player sync
func handlerUseItem(owner BagOwner, itemID int64) error { /* ... */ }
```

未声明时生成器默认使用 `rollback=state durability=async`，业务修改会先进入 Nest WAL。需要请求返回前确认 durable admission 时声明：

```go
//roost:nest target=player sync rollback=undo durability=strict
func handlerTransfer(owner BagOwner, target TargetOwner, itemID int64) error { /* ... */ }
```

`rollback` 支持 `state/undo`，热路径推荐 `undo`（对应 DAO mutator 的字段级 undo）；`durability` 支持 `memory/async/strict`，只有明确不持久化的 handler 才应显式使用 `durability=memory`。

生成的 `sender` / `syncsender` 包包含可注入的 `*Sender` 类型：接入层通过 `NewBagSender(nestClient)` 构造，调用带 `context.Context`、可返回入队错误的方法。生成器不生成包级 Send/Sync，也不读取全局 `nest.Nest`。完整说明见 [Nest 业务执行模型](docs/NEST_RUNTIME.zh-CN.md)。

## 关键实现细节

想读懂或扩展这套工具链，这几个设计决定值得先知道：

### 语法级 AST 解析，不做类型检查

所有解析器基于 `go/ast`（`parser.ParseComments`），**不引入 `go/types`**：不需要下载依赖、不需要目标项目能编译，单文件即可解析，生成器保持零外部依赖（本模块仅依赖 `yaml.v3`）。代价是拿不到跨包类型信息，所以：

- 嵌套 struct 通过**同一 def 目录内**的名字解析递归发现（`internal/dao/parse.go` 的第三遍扫描）；
- 类型分类靠语法形状（`classifyField`）。模板无法生成正确代码的形状——固定长度数组、`chan`、`*map[...]...`——在**生成期**就被拒绝，而不是留给下游项目的编译错误：`format.Source` 只保证语法合法，不保证语义正确。

### 内容哈希幂等与 `--check` 门禁

每个生成器写文件都走 `internal/genutil.WriteIfChanged`：内容与磁盘一致就不写，mtime 不动，重复生成对增量构建零扰动。普通 `roost generate` 会把项目复制到同级临时目录（跳过 `.git`/`bin`/`dist`/`log` 和 codegen 临时目录），完成全部生成器与 `GOWORK=off go mod tidy`，然后一次提交生成物及 `go.mod/go.sum`。提交计划去重并保存旧内容用于回滚；如果生成期间业务定义、配置或目标文件被另一进程修改，会拒绝覆盖并要求重跑。框架依赖解析也在临时项目进行，只把最终依赖文件写回。

`roost generate --check` 使用同一隔离机制，在副本里跑全部生成器，对含 `Code generated` 头的文件（以及 `configs/data/*.json`、`docs/generated/`）做 SHA-256 快照比对——有差异即报 `generated files are stale: ...` 并失败，工作区不被修改。CI 里把它作为"生成物必须已提交且最新"的门禁。

### `-force` 在编排层与单生成器的差别

- 单生成器的 `-force` 只是跳过 `WriteIfChanged` 的内容比较，无条件重写（用于怀疑输出被手工污染时强制刷新）。
- `roost generate --force` 把 `-force` 转发给每个支持它的生成器。
- **tablegen 是例外**：它的输出采用"文件已存在即拒绝覆盖"（`%s exists; pass -force to overwrite`）而非内容哈希语义，所以编排层对 tablegen **永远**传 `-force`，否则 schema 变更后每次再生成都会失败。
- `--check` 内部跑生成器时不传 force：快照比对基于内容，强制重写与哈希短路的结论一致。

### golden 测试防模板回归

`internal/genutil.AssertGolden` + 各生成器的 `testdata/golden/`（如 `internal/dao/testdata/golden/gen_hero_dao.go`）把**完整生成物**锁成字节级基线：片段断言（`strings.Contains`）看不见的模板漂移，golden diff 一定看得见。有意修改模板后刷新：

    go test ./internal/dao -run TestGoldenGeneratedOutput -update

`internal/dao/failfast_test.go` 则把上文每一条 fail-fast 报错固化为回归测试——每条错误都对应一个历史上真实存在过的静默陷阱。

## 学习路径与 roost-core 的关系

建议阅读顺序（由浅入深）：

1. **用**：本 README 快速启动 → [docs/PROJECT_GENERATOR.zh-CN.md](docs/PROJECT_GENERATOR.zh-CN.md)（roost.yaml、Kit Mod 装配、ID 管理、CI）→ [docs/NEST_RUNTIME.zh-CN.md](docs/NEST_RUNTIME.zh-CN.md)（Nest 执行模型）。
2. **读生成物**：`internal/dao/testdata/def/` 是最全的输入样例，`testdata/golden/` 是对应的完整产物——两边对照读，比读模板快。
3. **读实现**：每个生成器都是同一结构：`parse.go`（标记/AST → 中间定义）→ `gen.go` + `template_*.go`（text/template → `format.Source` → `WriteIfChanged`）→ `main.go`（flag 与文件编排）。从 `internal/dao` 读起，其余生成器同构。
4. **编排层**：`internal/roost/generate.go`（顺序、`--changed`/`--check`/`--force`）、`manifest.go`（roost.yaml 校验）、`add.go`（脚手架）、`id.go`（ID 空间扫描）。

**与 roost-core 的关系**：roost-codegen 只在**生成期**运行，本身不被业务依赖；生成出来的代码在**运行期**依赖 roost-core 提供的接口与容器——`dataengine.Tracker`/`DirtyHook`/`Mutation`、`entity.DaoInterface`、`nest.RollbackTx`/`RollbackSnapshotter`/`MutationParticipant`、`fmap`、`migration.MigrateDAO`，以及 `go.mongodb.org/mongo-driver/v2/bson`。升级 roost-core 后重新生成即可让产物对齐新接口；更新策略由 `roost.yaml` 的 `versions` 字段统一管理。

## 从 v1.4 升级到 v1.5

v1.5 的主题是把 DAO 解析从"静默容错"改为 **fail-fast**，以下行为变化需要注意：

1. **`dao:"map=fast"` 这类只写辅助选项的 tag 直接报错**。v1.4 中它会静默关闭 persist 与 sync（数据丢失陷阱）；按原意图改写为 `dao:"nopersist,nosync,map=fast"` 或 `dao:"persist,sync,map=fast"`。
2. **未知选项、未知 `map=` 值、`persist` 与 `nopersist` 冲突均为解析错误**，不再被忽略。
3. **孤儿标记报错**：`//roost:dao` 没有绑定到任何 struct（隔了其他声明）不再被静默丢弃。
4. **分组声明双绑定报错**：一个标记命中 `type (...)` 中多个 struct 直接失败；每个 struct 上方各放一个标记。
5. **产物文件名修正**：`HeroDao` 的产物从 `gen_hero_dao_dao.go` 修正为 `gen_hero_dao.go`，旧文件会被孤儿回收自动清理。
6. **孤儿回收覆盖"删光定义"场景**：删除或改名定义后，对应旧生成文件在下次生成时自动移除（仅限带 `DO NOT EDIT` 头的生成文件，手写文件不受影响）。
7. **DAO 存储字段私有化**（v1.4 引入，v1.5 延续）：业务读写必须走生成的方法。这是有意的源码不兼容变更，升级后重新生成 DAO 并把直接字段访问迁移为方法调用。
8. 标记与 struct 之间**允许普通 doc 注释**（v1.5 放宽）：`//roost:dao` 可以放在 doc 注释组最上方。
