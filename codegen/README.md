# roost-codegen

roost-core 的项目脚手架与代码生成工具链（模块路径 `github.com/tjbdwanghaibo/roost-core/codegen/…`，game-demo 模板在 [`../demo/`](../demo/README.md)）。独立的 roost-codegen 仓库已归档，生成器随 roost-core 同一 tag 发布。

它用源码里的标记注释（`//roost:dao`、`//roost:nest` 等）驱动一组 Go 代码生成器，把持久化、同步、协议、配置表这些"写错就丢数据"的样板代码变成可重复生成、可 CI 校验的产物。生成物在运行期依赖 roost-core 的 `dataengine`、`entity`、`nest`、`safemap` 等包；生成器运行前会校验框架兼容下限。版本与升级记录以根目录 [CHANGELOG](../CHANGELOG.md) 和仓库 tag 为准（合仓前的生成器历史见 [codegen 历史变更记录](../docs/release/history/codegen-CHANGELOG.md)）。

## 文档分工

| 文档 | 写什么 |
| --- | --- |
| 本 README | 入口与快速上手：装 CLI、建工程、跑第一次生成 |
| [小白逐步操作手册](docs/BEGINNER_WORKBOOK.zh-CN.md) | 完全第一次使用，配合 `roost project next` 一步一步走 |
| [命令与标记参考](docs/CODEGEN_REFERENCE.zh-CN.md) | 每个命令、生成器的标记 / tag / 参数、产物与拒绝规则 |
| [项目生成器](docs/PROJECT_GENERATOR.zh-CN.md) | 工程生成、`roost.yaml`、Mod 装配、ID、升级与部署生成物 |
| [Nest 业务执行模型](docs/NEST_RUNTIME.zh-CN.md) | 生成的 Sender 与 handler 在 Nest 里怎样执行 |
| [Player TCP 接入](docs/PLAYER_ACCESS_TCP.zh-CN.md) | 玩家 TCP listener、鉴权、主动推送与上线验收 |
| [CFGGEN_META](docs/CFGGEN_META.zh-CN.md) | cfggen schema 的字段级参考 |
| [数据流：六条路径](docs/DATA_FLOW.zh-CN.md) | 生成工程里一次请求的字节往哪走、每层守什么、失败在哪层处理 |
| [CI/CD 实施方案](docs/CI_CD_IMPLEMENTATION.zh-CN.md) | 框架发布列车与生成工程的质量门禁、兼容矩阵、交付方式 |
| [Codegen 实现与维护](../docs/framework/impl/12-codegen.md) | 生成器实现、必须保持的不变量、源码与回归入口 |

## 生成器总览

| 生成器 | 输入标记 / 约定 | 产出 | 解决什么问题 |
| --- | --- | --- | --- |
| `dao` | `//roost:dao`、`//roost:redisdao` + 字段 `dao:"..."` tag | `gen_<name>_dao.go`、`gen_<name>_nested.go`、`gen_<name>_redis_dao.go` | 持久化 / 同步字段的 dirty 追踪、字段级 patch、事务回滚，全部经生成的 mutator 收口 |
| `protocol` | `protocol/def` 下的 `//roost:proto` `//roost:protocol` `//roost:msg` 标记 | `.proto`、pb Go 代码、msgid、player_bind、协议 handler、robot 注册表、协议 manifest | 用 Go struct 单一来源维护协议，也支持从 `.proto` 反向生成 |
| `entity` | `//roost:entity entityKind=...` | `<entity>_gen_wire.go` | Entity 工厂、组件 / DAO 装配、hook 接线 |
| `nest` | `//roost:nest [target=...] [sync] [rollback=state\|undo] [durability=memory\|async\|strict]` | `<source>_nest_gen.go`、`sender/`、`syncsender/`、`game/bootstrap/nest.go` | handler 的 invoke 包装、显式注册与可注入的类型化 Sender |
| `tablegen` | `configs/schema` 下 `//roost:table`、`//roost:object` | Go 访问代码、CSV 模板、CSV→JSON 数据 | 配置表 schema、策划填表模板与运行时数据的一致性 |
| `cfggen` | 一个 YAML schema 文件（tables / globals / beans） | 单文件 Go 绑定与类型化访问器 | 配置定义零手写 Go |
| `attribute` | `//roost:attribute` | `gen_<profile>_attribute.go` | 属性 profile 的计算 / 聚合代码 |
| `webroute` | `//roost:web` | 路由注册代码 | HTTP 路由注册不再手写 |
| `errcode` | `errcode.Define(code, name, msg)` 调用 | `docs/generated/errcode.csv` | 错误码清单导出与查重 |
| `servicerpc` | 手写服务接口上的 `//roost:rpc` | `<iface>_rpc_gen.go`（传输）与 `<iface>_rpc_assembly_gen.go`（装配） | 服务拆成独立进程后调用方只查接口，传输层从接口生成 |
| `roost id` | 各类标记中的 `id=/kind=/type=/code=` | —（校验命令） | 按 `roost.yaml` 的 ID 空间查冲突、分配下一个可用 ID |

所有生成器都能独立运行（`cmd/<name>`），也可以由 `roost generate` 按依赖顺序统一编排。每个生成器的写法、参数与拒绝规则见[命令与标记参考](docs/CODEGEN_REFERENCE.zh-CN.md)。

## 快速上手

### 1. 安装 roost CLI

推荐直接跟随最新版本，不需要把 codegen 加进业务 module：

    go run github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest help

或安装本地命令：

    go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest
    roost version
    roost env doctor
    roost help beginner

`roost help` 列出全部能力，`roost help <能力>` 输出用途、命令、配置 / 标记和最小示例，不需要打开仓库文档；专题清单见[参考 §0](docs/CODEGEN_REFERENCE.zh-CN.md#0-内置帮助系统)。Windows 安装与 PATH 见[项目生成器](docs/PROJECT_GENERATOR.zh-CN.md#windows-安装与-path)。

### 2. 创建项目

    roost project new planet -module github.com/your-account/planet -services game,gate \
      -mods configdata,etcd,redis,mongo,nats,syncbus,remote_entity

完全新手可以先用 `-mods configdata` 创建轻量项目，不必先理解 MongoDB、Redis、NATS、Nest WAL 和跨服能力。进入项目后第一条命令是 `roost project next`（或 `make next`），它按真实进度只给一个动作和原因。`-features` 与 `-mods` 的完整清单、`roost.yaml` 字段、生成的 CI/CD 与部署模板见[项目生成器](docs/PROJECT_GENERATOR.zh-CN.md)。

新项目可以一条命令在本机起全部服务：`make dev-up`（基础设施 compose）→ `make dev-run`（按托管服务 → 业务服务的顺序编译启动并等各自 `/readyz`）→ game-demo 工程再 `make dev-smoke`（两个机器人走完整条链）；`make dev-status` / `make dev-stop` 查看与停止。

### 3. 第一条业务

零基础的首条业务不需要手写协议 Registry、Entity 工厂、Manager、import、字段 tag 或 Sender 接线：

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
    roost add protocol EnterGame --group game --handler player
    roost add endpoint EnterGame --handler player --lifecycle Player   # 登录时 GetOrCreate
    roost generate
    roost project doctor --workflow first-business

Component 与 DAO 自动挂到目标 Entity；业务在 Component 里写玩法方法，通过 DAO 的生成方法改状态。Nest handler 进入前已经持有 Entity 锁，不要在业务层重复加锁。生成项目里的 `docs/FIRST_BUSINESS.zh-CN.md` 有完整可复制例子。

### 4. DAO 与生成

在 `db/def/hero.go` 中定义：

```go
package def

//roost:dao coll=heroes db=main dbscope=sid
type HeroDao struct {
	Name    string                          // 无 tag：persist + sync 全开
	Level   int32
	Exp     int64  `dao:"persist"`          // 只持久化，不同步给客户端
	Cache   int64  `dao:"nopersist,sync"`   // 只同步，不落库
	Tmp     int    `dao:"-"`                // 完全排除
	Items   map[int64]int32 `dao:"persist,sync,map=fast"`
}
```

运行（要求项目根有 `roost.yaml`）：

    roost generate

输出 `generated: db/gen_hero_dao.go`；再跑一次内容没变就不写（`unchanged: ...`，mtime 不动）。生成的 DAO 使用私有存储字段，业务只能经生成的 getter / mutator 读写，绕不过 dirty、回滚与同步语义；tag 语法、生成物与解析错误见[参考 §4](docs/CODEGEN_REFERENCE.zh-CN.md#4-dao-生成器)。CI 用 `roost generate --check` 确认生成物已提交且最新。

## 读源码的顺序

1. **读生成物**：`internal/dao/testdata/def/` 是最全的输入样例，`testdata/golden/` 是对应的完整产物——两边对照读，比读模板快。
2. **读实现**：每个生成器同一结构：`parse.go`（标记 / AST → 中间定义）→ `gen.go` + `template_*.go`（text/template → `format.Source` → `WriteIfChanged`）→ `main.go`（flag 与文件编排）。从 `internal/dao` 读起，其余同构。
3. **编排层**：`internal/roost/generate.go`（顺序、`--changed` / `--check` / `--force`）、`manifest.go`（roost.yaml 校验）、`add.go`（脚手架）、`id.go`（ID 空间扫描）。

生成器只在生成期运行，本身不被业务依赖；升级 roost-core 后重新生成即可让产物对齐新接口。共同的实现约定（语法级 AST 解析、内容哈希幂等、`--check` 隔离、`-force` 语义、golden 测试）与必须保持的不变量见 [Codegen 实现与维护](../docs/framework/impl/12-codegen.md)。
