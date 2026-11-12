# skillbook：服务启动时把游戏的技能定义编译成一份只读目录，客户端与别的功能读同一份

“技能定义”是 `game/skills/` 下的 JSON 文件（框架 `gameplay/skill` 的定义格式）；“编译”是把它们变成可执行的 `skill.Program`。skillbook 只管这份目录的编译、发布与查询，不执行技能。

## 能力

- **客户端协议**（`protocol/def/skillbook_catalog.go`）
  - `SkillCatalog`：请求 `SkillCatalogRequest{}`，应答 `SkillCatalogResponse{Code, Reason, Skills, Warnings}`。返回本服务启动时编译出来的技能 ID（已排序）与编译告警的总数。目录没在运行时应答带 `Code` / `Reason`。
- **服务端入口**（包 `game/skillbook`，`skillbook.go`）
  - `Start(registry) (func(context.Context) error, error)`：模块的运行期入口，由生成的 bootstrap 在业务 Init 之前调用。编译全部定义并把目录发布成能力 `skillbook.catalog`；任何一个定义解析失败、编译出错或 ID 重复，返回错误、不发布半份结果，服务起不来。返回的停止函数把目录标成已停止。
  - `Read(registry) ([]string, int32, error)`：技能 ID 列表（复制出来的切片）与告警数。
  - `LookupProgram(registry, id) (*skill.Program, error)`：按 ID 取启动时编译好的只读 Program；不在每次请求时重新编译。
  - `Resolver(registry) (skill.ProgramResolver, error)`：给技能运行时恢复 checkpoint 用的解析器；它的 `ResolveProgram(id, digest)` 同时核对游戏逻辑摘要，定义改过规则的同名技能接不到旧的施法上。
  - `CompileEnvironment() skill.CompileEnvironment`（`skillbook_rules.go`）：工程的技能编译环境，启动、doctor 与别的功能用同一份。
  - 包 `game/skills`（`catalog.go`）：`CompileAll(environment) (*Catalog, error)`，把内嵌的 `*.json` 全部解析、编译。
  - `Controller.HandleSkillCatalog`（`game/controllers/skillbook/skillbook_catalog.go`）：协议端点，不经过 Nest。
- **登记点**（`game/skillbook/skillbook_rules.go`）
  - `OnCompileEnvironment(name string, apply func(*skill.CompileEnvironment))`：往编译环境里加业务的 Host 能力、状态目录等。只在包 `init` 里登记，运行期不能换；空名、nil、重名 panic。每次取环境都从独立的缺省环境出发，按名字顺序调用，全部扩展完之后统一算摘要。
- **状态**：无玩家状态。目录是服务级、启动后只读的内存对象。
- **配置**：没有配置表。留给工程的文件是 `game/skills/*.json`（技能定义，缺省带一个 `fireball.json`，ID 是 `skill.<工程名>.fireball`）与 `game/skillbook/skillbook_rules.go`（编译环境）。
- **错误码**：无自己的业务码。目录不可用时的错误不带码，客户端收到的是通用的内部错误码。

## 能力边界

- **不做的事**：不执行技能（没有施法协议、技能 Runtime、Host、冷却、目标选择——那些在 combat 模块或工程自己的玩法里）；不做“玩家学会了哪些技能”的状态；不做热更新，改定义要重新构建并重启；不给客户端下发技能的具体内容，只有 ID 与告警数；不做客户端侧的技能逻辑。
- **启动即校验**：定义是内嵌进二进制的（`go:embed`），启动时一次编译完；有错误级诊断就拒绝起服。告警不阻止启动，只计数。
- **只读与并发**：目录发布之后不再改；`Read` 返回的切片与内部不共享。停止之后 `Read`、`LookupProgram`、`Resolver` 都返回错误。
- **生命周期**：只支持启动一次；重复 `Start` 会因为能力已登记而失败，并且不影响已在运行的目录（场景 `TestSkillbookDuplicateStartPreservesTheRunningCatalog`）。
- **前提**：没有模块依赖；需要协议功能。自动接线只在生成的 bootstrap 里生效，手动构造服务要用带模块的构造入口。每个业务进程各自编译一份，进程之间不共享。
- **已知限制**（详见 `docs/maintenance/KNOWN-LIMITS.md`）：“运行期接线的边界”一条（`runtime` 入口在框架 Mod 启动后、业务 Init 前运行；只支持一次 Init、串行 Shutdown）。`docs/framework/CAPABILITIES.md` 把它标为“仅目录，不包含技能执行 Host、热更新或客户端技能逻辑”。

## 复用方式

- **需求正好是它**：`roost module add skillbook`，没有依赖。combat 依赖它，装 combat 时一起装上。
- **需求多一点**
  - 加技能、改技能：在 `game/skills/` 下增改 `*.json`（一个文件一个技能，ID 不能重复），删掉样例 `fireball.json`。
  - 技能定义里要用业务自己的 Host 能力或状态：在自己的文件里 `init` 登记 `OnCompileEnvironment("<名字>", func(e *skill.CompileEnvironment) { … })`，照 `combat/with/skillbook` 的写法；或直接改 `skillbook_rules.go`。
  - 不要改 `skillbook.go` 里“先全部编译成功、再发布”的次序，也不要在请求路径上重新编译。
- **别的功能要用**
  - 施法时取 Program：`program, err := skillbook.LookupProgram(registry, id)`。
  - 恢复持久化的施法：`resolver, err := skillbook.Resolver(registry)`，交给技能运行时。
  - 要与目录同一份编译环境（例如算摘要、离线检查）：`skillbook.CompileEnvironment()`。
- **不适合直接用的情形**：需要运行期热更技能、按玩家或按服下发不同的技能集、需要把技能数据完整下发给客户端。值得借鉴的是“启动时编译、失败拒绝起服、发布后只读”的服务级目录写法，以及用摘要防止新定义接到旧存档上的做法。

## 联动

- `combat/with/skillbook`：登记 `OnCompileEnvironment("combat", …)`，往编译环境的状态目录里加战斗用的两条状态（`stun`、`silence`）。combat 的清单里这条联动没有声明验证场景。
- combat（依赖 skillbook）在自己的运行期入口里调 `skillbook.Resolver`、`skillbook.LookupProgram` 与 `skillbook.CompileEnvironment()`。
- skillbook 自己没有 `with/` 目录。

## 验证范围

- `roost module verify skillbook`：11 条场景（`^TestSkillbook`，包 `./game/skillbook` 9 条、`./game/controllers/skillbook` 2 条）。覆盖：启动编译出缺省目录、编译环境的钩子在算摘要之前执行、应答排序且是副本、编译失败不发布任何东西、缺输入拒绝、重复启动不破坏运行中的目录、停止后拒绝读取且可重复停止、取回启动时的 Program 并核对摘要、发布的 Program 表归目录所有；端点读到启动目录、缺上下文或请求时拒绝。
- `game/skills` 包里另有一条 `TestSkillCatalogCompiles`（缺省定义能编译），不在上面的计数里。
- 全部是进程内的离线场景，不经过网络；`SkillCatalog` 经真实 TCP 的路径没有在本模块的场景里验证。
