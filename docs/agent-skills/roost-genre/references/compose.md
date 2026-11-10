# 按需求组合：用 roost-core 定制自己的服务逻辑

适用：用户给的是一段需求描述（“做一个有货币、签到和每日任务的服务”），不是“照某类玩法出一个 demo”；或者要在已有的 roost 工程上继续加功能。照某类玩法出 demo 用各玩法的 reference。

链接相对 roost-core 根目录。命令以目标检出为准：`roost module` 在 v1.43.0 之后的 main 上才有（v1.43.0 及更早的 tag 没有），更早的 tag 没有这条路，只能手写（第 4 步）。

## 0. 先说清楚能做到什么

- **有现成模块的需求**：一条命令装进工程，带验收过的场景测试。目前只有两个模块：`wallet`（货币）、`checkin`（每日签到）。以 `roost module list` 为准。
- **没有现成模块的需求**：用 `roost add …` 建骨架、手写业务。框架给的是事务、持久化、同步、定时、跨进程这些底座，规则要写。写法照已装模块的文件。
- **框架不支持的需求**：如实说明并给出处，不拼假 demo。见 [SKILL §6](../SKILL.md) 与[能力清单“明确没有 / 不适合的”](../../../framework/CAPABILITIES.md#明确没有--不适合的)。

交付时这三类要分开报告（第 6 步）。

## 1. 拆需求、查能力

把需求拆成条目，每条在[能力清单](../../../framework/CAPABILITIES.md)里查：末尾的“需求条目 → 能力”速查表先看，再看各分组表的“怎么接入”与“状态与主要边界”。每条得出一个结论：

| 结论 | 怎么办 |
| --- | --- |
| 有现成模块 | 第 3 步安装 |
| 有框架能力、需手写业务 | 第 4 步手写；先读清单“文档”列里的契约 |
| 有玩法夹具或 game-demo 的参考 | 第 4 步手写，照参考的做法；夹具与 demo 不能直接装进别的工程 |
| 只有设计 / 原型 / 不支持 | 告诉用户，给出处；不做 |

拿不准某项能力的边界时读 [KNOWN-LIMITS](../../../maintenance/KNOWN-LIMITS.md)，不要凭印象承诺。

## 2. 工程

```bash
set -euo pipefail
export GOWORK=off
CORE=/path/to/roost-core                  # roost-core 检出
WORK=/path/to/work
NAME=mygame; MOD=example.com/mygame
mkdir -p "$WORK"
(cd "$CORE" && go build -o "$WORK/roost" ./codegen/cmd/roost)   # CLI 与 CORE 同版本
ROOST="$WORK/roost"; DIR="$WORK/$NAME"
```

- **新工程**：

  ```bash
  "$ROOST" project new "$NAME" -module "$MOD" -out "$DIR" -template game --skip-deps
  cd "$DIR"
  go mod edit -replace "github.com/tjbdwanghaibo/roost-core=$CORE"
  ```

  `-template game` 带 Player 与 World 两个实体、九个托管框架服务。现有模块都挂在 `Player` 上，要求这个形状。
- **已有工程**：`cd` 进去，先读 `roost.yaml`：`modules` 段是已装的模块（每个模块拥有的文件、DAO 字段、号），`services`、`features`、`ids` 是装配现状。再 `"$ROOST" module list`，带 `*` 的是已装的。不要重装、不要覆盖业务改过的文件。

## 3. 装现成模块

```bash
"$ROOST" module list --json                 # 模块、依赖、状态字段、场景数
"$ROOST" module plan checkin --json         # 只预览：不改动工程
"$ROOST" module add checkin                 # 连同依赖（wallet）安装并运行生成器
"$ROOST" module verify                      # 每个模块的场景数 + build / vet / id check / generate --check
```

- `plan` 的 JSON：`modules[].status` 为 `install`（将安装）、`installed`（已装）、`installed_other_version`（已装的是另一版，安装器不动它）；`prerequisites` 是会先执行的 `roost add`；`ids`、`state`、`files` 是将分配的号、追加的 DAO 字段、写入的文件。
- `add` 报错时按错误文字处理，不要绕过：
  - `already exists and is not this module's file`：落点被业务文件占着。读那个文件，告诉用户，由用户决定保留自己的实现还是让位。
  - `cannot be installed yet`：缺 Mod、feature 或托管服务，错误里给了要执行的命令。
- 装完之后模块文件就是业务代码：要改规则直接改（例如 `game/entities/player/checkin_component.go` 里的奖励），改完再 `module verify`。改了行为就同步改模块自带的测试，不要删测试。

## 4. 手写模块库里没有的功能

照已装模块的结构写（装了 wallet 就以它为样板，文件清单在 `roost.yaml` 的 `modules.wallet.files`）。一个挂在 Player 上、带一条客户端协议的功能 `<feature>`，按这个顺序：

1. **状态**：新建 `db/def/<feature>.go`，定义嵌套结构体，字段只写 `store` 键（嵌套类型内从 `"1"` 起编号）：

   ```go
   package dbdef

   // QuestState 是每日任务的进度。
   type QuestState struct {
   	Day      int64 `store:"1"`
   	Progress int32 `store:"2"`
   	Claimed  bool  `store:"3"`
   }
   ```

   在 `db/def/player.go` 的 `PlayerDao` **末尾**加一个顶层字段，字段名与存储键带功能名：

   ```go
   	// Quest 是每日任务给玩家本人看的进度。
   	Quest QuestState `store:"quest" dao:"persist,sync"`
   ```

   - 同步与否按这个顶层字段决定：`persist,sync` 下发给玩家，`persist` 只落库。既有可见状态又有服务端账本就写两个字段、两个结构体。不要在嵌套结构体的字段上写 `dao` 标签。
   - 只能加在末尾（同步的字段号按定义顺序排）；不要改已有字段的 `store` 键。
2. **组件**：`"$ROOST" add component <Feature> --entity Player`，在生成的 `game/entities/player/<feature>_component.go` 里写业务方法。状态只经生成的 setter 改（`component.owner.Dao().GetQuest().SetProgress(…)`）；组件不保存事务会改的状态、不加锁。要动货币就调同一个实体上的 `component.owner.WalletComp().Apply(key, delta)`——同一笔事务，业务键要能唯一标识这一次发放。
3. **错误码**：`"$ROOST" add errcode <feature>_<reason>`（每个一条），生成在 `internal/errors/`，把说明文字改掉。
4. **协议**：`"$ROOST" add protocol <Feature><Action> --group game --handler <feature>`，在 `protocol/def/<feature>_<action>.go` 里给 Request / Response 加字段（Response 保留 `Code`、`Reason`）。
5. **Nest handler**：`"$ROOST" add handler <Feature><Action> --entity Player --component <Feature>`，把 `game/handler/<feature>_<action>.go` 改成收 PB 请求、回 PB 响应：

   ```go
   //roost:nest rollback=undo durability=strict
   func handlerQuestClaim(target player.IQuestEntity, request *pb.QuestClaimRequest, nowUnixMilli int64) (*pb.QuestClaimResponse, error) {
   	result, err := target.QuestComp().Claim(nowUnixMilli)
   	if err != nil {
   		return nil, err
   	}
   	return &pb.QuestClaimResponse{Reward: result.Reward}, nil
   }
   ```

   - 一个 handler 一个文件，文件名就是 handler 名的蛇形写法（`roost generate` 与 `add endpoint` 按文件名找）。
   - 最后一个参数写 `nowUnixMilli int64` 就能拿到这条消息冻结的业务时间；客户端不传时间。
   - `pb` 是 `<module>/protocol/pb`，要先跑一次 `"$ROOST" generate` 才有。
6. **端点**：`"$ROOST" generate`，再 `"$ROOST" add endpoint <Feature><Action> --handler <feature>`，再 `"$ROOST" generate`。得到 `game/controllers/<feature>/` 下的手写入口与生成的转交。玩家身份取认证会话，不取请求字段。
7. **场景测试**：照 `game/handler/wallet_test.go` 写 `game/handler/<feature>_test.go`：替身提交器、`nest.NewEngine`、生成的 `syncsender`，覆盖成功、拒绝时零写入、重复请求、提交被拒后回滚。要拨时间用 `clock.SetSource`（`game/handler/checkin_test.go` 有例子）。测试名加功能前缀；只断言自己的事实（同一个 DAO 上有别的功能的字段）。

不挂在玩家身上的状态（房间、公会、全服对象）用独立实体：`"$ROOST" add entity <Name>` + `add dao <Name> --entity <Name>`，多人争用的对象把实体标记里的 `category` 改成 `entity.EntityCategoryWorld`。多实体事务、定时器、跨进程等见能力清单对应行的文档；这些比单实体功能复杂，先读契约再写。

不要做的事：不改带 `_gen` 的文件；不在 handler 里等网络、磁盘或另一个 Nest 请求；不自开 goroutine 访问实体；不用进程级全局变量在功能之间传状态。

## 5. 验收

```bash
"$ROOST" module verify                                            # 已装模块 + 工程级检查
go test -mod=mod -race -count=1 -v -run '^Test<Feature>' ./game/handler 2>&1 | tee "$WORK/feature.log"
grep -c '^--- PASS' "$WORK/feature.log" || true                   # 自己数：等于你写的场景数
grep -c '^--- SKIP' "$WORK/feature.log" || true                   # 必须为 0
"$ROOST" project doctor || true
```

- `module verify` 全部 `ok`、退出码 0。它已经包含 `go build`、`go vet`、`roost id check`、`roost generate --check`。
- 手写功能的场景数自己核对：`-run` 写错或用例被跳过时 `go test` 照样报 `ok`。
- `project doctor` 在 `-template game` 的新工程上必然退出 1：account / chat / platform / session 四个托管服务的 collaborators 是拒绝桩，上线前由项目实现。除这四项外不应有 FAIL。
- 这一档是离线验收：替身提交器，没有 Mongo / NATS / Redis，没有真实客户端。要真起进程见 [SKILL §4](../SKILL.md)。

## 6. 向用户报告

逐条对照需求，分四栏：

1. **装了哪些模块**：名字、各自通过的场景数、追加的 DAO 字段、分配的协议号与错误码（`roost.yaml` 的 `modules` 段有）。
2. **手写了哪些功能**：文件清单、场景数、你做的业务假设（数值、日界、上限都是样例）。
3. **没做的**：框架不支持或只有设计的，给出处；需要用户决定的。
4. **验收到哪一步**：`module verify` 与手写场景的实际输出；明确写“离线、替身提交器，未连真实依赖、未经真实客户端”。

临时目录用完即删，检查没有遗留的测试进程。

## 7. 边界

- 模块只有两个，都要求 `-template game` 的 `Player`。13 类玩法与 game-demo 的业务还不是模块，不能用 `roost module add` 装；它们的做法可以参考，代码要手写进工程。
- 没有卸载；分配过的号与存储键不回收。已装模块不会随 roost-core 升级自动更新。
- 模块没有 Init / Shutdown、后台循环、定时器类型、压测步骤。
- 细节与出处：[modules/README](../../../../modules/README.md)、[命令参考“业务模块”](../../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#业务模块roost-module)、[KNOWN-LIMITS“业务模块”](../../../maintenance/KNOWN-LIMITS.md#业务模块roost-module)。
