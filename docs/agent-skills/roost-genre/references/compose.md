# 按需求组合：用 roost-core 定制自己的服务逻辑

适用：用户给的是一段需求描述（“做一个有货币、签到和每日任务的服务”），不是“照某类玩法出一个 demo”；或者要在已有的 roost 工程上继续加功能。照某类玩法出 demo 用各玩法的 reference。

链接相对 roost-core 根目录。命令以目标检出为准：`roost module` 在 v1.43.0 之后的 main 上才有（v1.43.0 及更早的 tag 没有），更早的 tag 没有这条路，只能手写（第 4 步）。

## 0. 先说清楚能做到什么

- **有现成模块的需求**：一条命令装进工程，带验收过的场景测试。目前有十二个模块：`enter`（进入游戏：建档、首次初始化、进入钩子、打开自身视图）、`wallet`（货币）、`checkin`（每日签到）、`task`（每日任务）、`stamina`（体力）、`shop`（金币买道具、限购）、`bag`（堆叠背包，自带道具表：加道具改 `configs/table/item.csv`）、`profile`（名字、等级、经验）、`chat`（聊天：世界与私聊）、`leaderboard`（总分榜）、`mailbox`（邮件列表与领附件：金币内置，装了 `bag` 就能发道具）、`skillbook`（启动编译的技能目录与查询协议）。新工程一般都先装 `enter`。以 `roost module list` 为准。
- **这一档交付到哪**：业务逻辑写完、离线场景通过、有“进入游戏”端点（建档，第 4B 步）、客户端可见的状态接上自身视图（第 4A 步）。**不含**登录鉴权（`auth.go` 缺省拒绝，谁是哪个玩家由它决定）、真实依赖、Robot 与网络验收——[SKILL §3.6](../SKILL.md) 的“网络验收”在这里没有做，报告里要写明。
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

  `-template game` 带 Player 与 World 两个实体、九个托管框架服务。有实体状态的模块目前挂在 `Player` 上，要求这个形状；skillbook 是服务级只读目录。
- **已有工程**：`cd` 进去，先读 `roost.yaml`：`modules` 段是已装的模块（每个模块拥有的文件、DAO 字段、号），`services`、`features`、`ids` 是装配现状。再 `"$ROOST" module list`，带 `*` 的是已装的。不要重装、不要覆盖业务改过的文件。

## 3. 装现成模块

```bash
"$ROOST" module list --json                 # 模块、依赖、状态字段、场景数
"$ROOST" module plan checkin --json         # 只预览：不改动工程
"$ROOST" module add checkin                 # 连同依赖（wallet）安装并运行生成器
"$ROOST" module verify                      # 每个模块的场景数 + build / vet / id check / generate --check
```

- `plan` 的 JSON：`modules[].status` 为 `install`（将安装）、`installed`（已装）、`installed_other_version`（已装的是另一版，安装器不动它）；`prerequisites` 是会先执行的 `roost add`；`ids`、`state`、`files` 是将分配的号、追加的 DAO 字段、写入的文件；可选 `runtime` 是自动接入服务启动与停机的入口，安装后连同目标 service、requires 记进 roost.yaml，不用改业务 Service；见 [服务级接线](../../../../modules/README.md#服务级启动与停机)。
- `add` 报错时按错误文字处理，不要绕过：
  - `already exists and is not this module's file`：落点被业务文件占着。读那个文件，告诉用户，由用户决定保留自己的实现还是让位。
  - `cannot be installed yet`：缺 Mod、feature 或托管服务，错误里给了要执行的命令。
- **联动文件**：输出里的 `glue <模块> with <另一个模块>` 是两个模块之间的接线（例如 chat 在玩家进入游戏时把他加进在线名单、签到成功推进每日任务），两个模块都装着时安装器自动写，不论先装哪个；`plan --json` 的 `glue` 数组列出会写的文件。它们归工程所有，规则直接在文件里改。`module verify` 把它们的场景单独列一行 `<模块>+<另一个模块>`。
- 模块自带的客户端协议（如 wallet 的 `WalletSpend`：客户端发起的消费）是样例入口。需求里没有对应的客户端操作时，告诉用户它存在，由用户决定保留还是删掉这条协议与端点。
- 装完之后模块文件就是业务代码：要改规则直接改（例如 `game/entities/player/checkin_component.go` 里的奖励），改完再 `module verify`。改了行为就同步改模块自带的测试，不要删测试。

## 4. 手写模块库里没有的功能

照已装模块的结构写（装了 wallet 就以它为样板，文件清单在 `roost.yaml` 的 `modules.wallet.files`）。

**一个模块都没装时**，先补上模块安装会替你做的两件事，否则后面的步骤没有 `PlayerDao`、`add endpoint` 与 `add transport tcp` 也会被拒绝：

```bash
"$ROOST" add dao Player --entity Player
"$ROOST" add access player --service game
```

一个挂在 Player 上、带一条客户端协议的功能 `<feature>`，按这个顺序：

1. **状态**：新建 `db/def/<feature>.go`，定义嵌套结构体，字段只写 `store` 键（嵌套类型内从 `"1"` 起编号）：

   ```go
   package dbdef

   // TaskState 是每日任务的进度。
   type TaskState struct {
   	Day      int64 `store:"1"`
   	Progress int32 `store:"2"`
   	Claimed  bool  `store:"3"`
   }
   ```

   在 `db/def/player.go` 的 `PlayerDao` **末尾**加一个顶层字段，字段名与存储键带功能名：

   ```go
   	// Task 是每日任务给玩家本人看的进度。
   	Task TaskState `store:"task" dao:"persist,sync"`
   ```

   - 同步与否按这个顶层字段决定：标了 `sync` 的字段才会下发，`persist` 只落库。既有可见状态又有服务端账本就写两个字段、两个结构体。不要在嵌套结构体的字段上写 `dao` 标签。标了 `sync` 还要打开自身视图客户端才收得到，见第 4A 步。
   - 只能加在末尾（同步的字段号按定义顺序排）；不要改已有字段的 `store` 键。
2. **组件**：`"$ROOST" add component <Feature> --entity Player`，在生成的 `game/entities/player/<feature>_component.go` 里写业务方法。状态只经生成的 setter 改（`component.owner.Dao().GetTask().SetProgress(…)`）；组件不保存事务会改的状态、不加锁。同一个实体上的别的组件（模块的或自己写的）都用 `component.owner.<Other>Comp()` 直接调，在同一把锁、同一笔事务里。要动货币就调同一个实体上的货币组件，同一笔事务。两个入口按“谁来保证只做一次”选：
   - **你的状态里记着这一次做过没有**（今天领过、这个订单到过账）：先判自己的状态，再 `component.owner.WalletComp().Add(delta)`，不留回执。周期性发放（每日、每局、每个订单）都用这个。
   - **没有别的事实可判**（客户端带请求号的消费）：`WalletComp().Apply(key, delta)`，同一个 key 在去重窗口内重复调用只生效一次。窗口只留最近 128 次带业务键的加减，是短期防重发，不是永久账本：不要拿按天、按局递增的键去用它。
3. **错误码**：`"$ROOST" add errcode <feature>_<reason>`（每个一条，一码一个文件），生成在 `internal/errors/`，把说明文字（生成时是 `TODO`）改掉。模块是一个功能一个文件，两种布局都可以。业务拒绝就从组件 / handler 里返回这些带码的错误，生成的端点会把它转成回包的 `Code` / `Reason`；不用在 controller 里自己拼响应。
4. **协议**：`"$ROOST" add protocol <Feature><Action> --group game --handler <feature>`，在 `protocol/def/<feature>_<action>.go` 里给 Request / Response 加字段（Response 保留 `Code`、`Reason`）。
5. **Nest handler**：`"$ROOST" add handler <Feature><Action> --entity Player --component <Feature>`，把 `game/handler/<feature>_<action>.go` 改成收 PB 请求、回 PB 响应：

   ```go
   //roost:nest rollback=undo durability=strict
   func handlerTaskClaim(target player.ITaskEntity, request *pb.TaskClaimRequest, nowUnixMilli int64) (*pb.TaskClaimResponse, error) {
   	result, err := target.TaskComp().Claim(nowUnixMilli)
   	if err != nil {
   		return nil, err
   	}
   	return &pb.TaskClaimResponse{Reward: result.Reward}, nil
   }
   ```

   - 一个 handler 一个文件，文件名就是 handler 名的蛇形写法（`roost generate` 与 `add endpoint` 按文件名找）。
   - 最后一个参数写 `nowUnixMilli int64` 就能拿到这条消息冻结的业务时间；客户端不传时间。
   - `pb` 是 `<module>/protocol/pb`，要先跑一次 `"$ROOST" generate` 才有。
   - 请求没有字段时也保留 `request` 参数，形状与带字段的一样。
   - 不对客户端开放的写入（登录时的初始化、别的功能触发的发放）也写成 Nest handler，参数用普通类型，不建协议和端点，由服务端代码经生成的 Sender 调用。
6. **端点**：`"$ROOST" generate`，再 `"$ROOST" add endpoint <Feature><Action> --handler <feature>`，再 `"$ROOST" generate`。得到 `game/controllers/<feature>/` 下的手写入口与生成的转交。玩家身份取认证会话，不取请求字段。
   - 第一次 `generate` 之后 `game/controllers/<feature>/` 里已经有一个返回 `not implemented` 的入口桩（文件头写着 application-owned）。它没被改过时 `add endpoint` 会把它换成转交入口，不用手写、也不会被拒绝。
7. **场景测试**：照 `game/handler/wallet_test.go` 写 `game/handler/<feature>_test.go`：替身提交器、`nest.NewEngine`、生成的 `syncsender`，覆盖成功、拒绝时零写入、重复请求、提交被拒后回滚。要拨时间用 `clock.SetSource`（`game/handler/checkin_test.go` 有例子）。测试名加功能前缀；只断言自己的事实（同一个 DAO 上有别的功能的字段）。
   - getter 与替身提交器不借用模块测试文件里的类型（那些文件可能被改）；自己写的几个功能之间可以共用一个夹具文件。
   - 拨时间的写法（`clock` 是 `infra/base/clock`）：

     ```go
     var now atomic.Int64
     now.Store(time.Date(2026, 11, 10, 9, 0, 0, 0, time.UTC).UnixMilli())
     clock.SetSource(func() time.Time { return time.UnixMilli(now.Load()) })
     t.Cleanup(func() { clock.SetSource(nil) })
     // 之后 now.Add(毫秒数) 就是拨快业务时间；handler 的 nowUnixMilli 跟着变。
     ```

     `clock.SetSource` 是进程级的：一个用例只建一个 fixture，后建的会顶掉前一个的时钟。
   - 需求里给了数的（上限 20、每 6 分钟、每天 2 次），测试里把数字写死，不要引用被测代码里的常量——否则常量改错了测试照样通过。
   - 没有天然幂等键的写入（“每完成一局上报一次”）重发会多记一次。需要防重就让请求带一个业务键（局号、请求号）并在自己的状态里判重；不需要就把这条假设写进报告。

**日界**：“今天”用 `int64(clock.BusinessDay(time.UnixMilli(nowUnixMilli)))`（`infra/base/clock`；返回类型是 `clock.Day`，存进 `int64` 字段要转换），得到 `YYYYMMDD` 的业务日，直接存进状态、按大小比较；业务时区与每日重置时刻是部署配置（缺省 UTC 零点）。不要用 `nowUnixMilli / 86400000` 自己算。每日清零按惰性做：当天第一次写入时发现存的业务日不是今天就清。跨天后没有新写入时，客户端经自身视图看到的仍是昨天的记录——让状态带上业务日字段，客户端比对它判断是否已是新的一天。

**随时间恢复的数值**（体力、冷却）：不要用定时器逐点加。状态里存“上次结算后的值”和“下一点恢复的时刻”（已满时为 0），每次写入前先按 `nowUnixMilli` 把该恢复的点数结算进去（零头保留、到上限就停）。两个字段都同步给客户端，由客户端按它们推算当前值与倒计时——存的值只在写入时更新，客户端直接显示它会看到“不恢复”，这条约定要写进报告。

不挂在玩家身上的状态（房间、公会、全服对象）用独立实体：`"$ROOST" add entity <Name>` + `add dao <Name> --entity <Name>`，多人争用的对象把实体标记里的 `category` 改成 `entity.EntityCategoryWorld`。多实体事务、定时器、跨进程等见能力清单对应行的文档；这些比单实体功能复杂，先读契约再写。

不要做的事：不改带 `_gen` 的文件；不在 handler 里等网络、磁盘或另一个 Nest 请求；不自开 goroutine 访问实体；不用进程级全局变量在功能之间传状态。

## 4A. 让客户端看到状态

**装了 `enter` 模块就不用做这一步**：安装器已经代办，`roost project doctor` 里有一行 `OK self-sync`。没装时：标了 `sync` 的字段要打开玩家自身视图才会推给客户端，别的模块不会替你打开：

```bash
"$ROOST" add transport tcp                 # 生成玩家 TCP 接入与 auth.go；roost.yaml 的 access.player.transports 多出 tcp
```

在 `roost.yaml` 的 `access.player` 下手工加一行 `self_sync: true`（与 `transports` 同级），然后：

```bash
"$ROOST" project sync
"$ROOST" project doctor || true            # 应出现一行 OK self-sync
```

之后 Player 主 DAO 里所有标了 `sync` 的顶层字段（模块的与手写的）都推给玩家本人，不用写 packer；说明见 [PLAYER-ACCESS §7](../../../framework/PLAYER-ACCESS.md#7-玩家看自己的状态self_sync)。真实客户端要连上还差两件，属于登录鉴权，不在这一档：实现 `internal/access/player/tcp/auth.go`（缺省拒绝一切），再 `"$ROOST" config enable player-tcp`；`"$ROOST" project doctor --workflow player-tcp` 列出剩下的项。

## 4B. 进入游戏：建档与首次初始化

**装 `enter` 模块即可**（`"$ROOST" module add enter`）：它带 `EnterGame` 端点和首次初始化，新玩家的初始发放写进它的 `game/entities/player/enter_first.go`（例如 `component.owner.WalletComp().Add(100)`）。手写的功能要在玩家进入时顺带做事（上线登记、拉离线期间的数据），不要改进入端点：在自己的包里写

```go
func init() {
	gameenter.OnEntered("<功能名>", func(ctx context.Context, registry *app.Registry, entered gameenter.Entered) error {
		// entered.PlayerID、entered.First（这次是否首次进入）、entered.FirstAtUnixMilli（首次进入的时间）
		return nil
	})
}
```

（`gameenter` 是 `<module>/game/enter`。）钩子在事务之外执行，失败只记日志、下次进入再调；必须成功的初始发放仍写在 `enter_first.go` 里。

下面是不装它时的手写做法，也是它内部的做法。

Nest handler 的目标必须是已存在的实体。新玩家没有 Player，所以客户端登录后的第一条请求必须是一个“进入游戏”端点，由它加载或创建 Player；没有它，场景测试照样全绿（测试里直接造了实体），真实进程里每条玩法请求都会答 `entity_not_found`。`-template game` 已带 `game/lifecycle/player.go`，只差端点：

```bash
"$ROOST" add protocol EnterGame --group game --handler player
"$ROOST" generate
"$ROOST" add endpoint EnterGame --handler player --lifecycle Player
"$ROOST" generate
```

生成的 `game/controllers/player/enter_game.go` 调 lifecycle 的 `GetOrCreate`，不经过 Nest，是业务文件。

**新玩家的初始发放**（送金币、满体力）不要按“这次是不是新建的”判断：建档后、发放前进程崩了，这个玩家就永远拿不到。做法是：发放写成一个不对客户端开放的 Nest handler，“发过没有”记在自己的状态里（一个只落库的布尔字段），每次进入游戏在 `GetOrCreate` 之后都经生成的 Sender 调一次它，已发过就什么都不做。入口里这段手写串接没有离线测法（场景测试只到 Nest handler），在报告里列为未测。

## 5. 验收

```bash
"$ROOST" module verify                                            # 已装模块 + 工程级检查
go test -mod=mod -race -count=1 -v -run '^Test(<FeatureA>|<FeatureB>)' ./game/handler 2>&1 | tee "$WORK/feature.log"
grep -c '^--- PASS' "$WORK/feature.log" || true                   # 自己数：等于你写的场景数
grep -c '^--- SKIP' "$WORK/feature.log" || true                   # 必须为 0
go test -mod=mod ./...                                            # 其余生成代码自带的测试
"$ROOST" project doctor || true
```

- `module verify` 全部 `ok`、退出码 0。它已经包含 `go build`、`go vet`、`roost id check`、`roost generate --check`。
- 手写功能的场景数自己核对：`-run` 写错或用例被跳过时 `go test` 照样报 `ok`。
- `project doctor` 在 `-template game` 的新工程上必然退出 1：account / chat / platform / session 四个托管服务的 collaborators 是拒绝桩，上线前由项目实现。除这四项外不应有 FAIL。
- 这一档是离线验收：替身提交器，没有 Mongo / NATS / Redis，没有真实客户端。要真起进程见 [SKILL §4](../SKILL.md)。

## 6. 向用户报告

逐条对照需求，分四栏：

1. **装了哪些模块**：名字、各自通过的场景数、追加的 DAO 字段、分配的协议号与错误码（`roost.yaml` 的 `modules` 段有）。
2. **手写了哪些功能**：文件清单、场景数、分配到的号（手写功能的号不在 `roost.yaml` 里：组件号看 `//roost:component`，错误码看 `errcode.Define`，协议号看 `//roost:msg`）、你做的业务假设（数值、日界、上限都是样例）。
3. **没做的**：框架不支持或只有设计的，给出处；需要用户决定的。
4. **验收到哪一步**：`module verify` 与手写场景的实际输出；明确写“离线、替身提交器，未连真实依赖、未经真实客户端、没有 Robot 与网络验收；端点层（错误转成回包）只有生成器自己的用例”。

临时目录用完即删，检查没有遗留的测试进程。

## 7. 边界

- 模块目前十二个；除 chat、leaderboard、skillbook 外都要求 `-template game` 的 `Player`，chat / leaderboard / mailbox 分别要求业务服务 `uses: [chat]` / `[rank]` / `[mail]`（`-template game` 都已有）。模块之间自动接上的只有联动文件做的那几件（进入时聊天上线与拉全服邮件、签到推进每日任务）；其余要自己写：升级奖励（`profile_levelup.go`）、新玩家初始发放（`enter_first.go`）、别的每日任务何时推进（调 `TaskComp().Advance`）、何时上榜（调 `leaderboard.Submit`）、什么操作扣体力（调 `StaminaComp().Spend`）。装了 shop 要把样例商品目录换成自己的。13 类玩法与 game-demo 的其余业务还不是模块，不能用 `roost module add` 装；它们的做法可以参考，代码要手写进工程。
- 没有卸载；分配过的号与存储键不回收。已装模块不会随 roost-core 升级自动更新。
- 模块已支持 `runtime` 服务 Init / Shutdown 接线；定时器类型分配、模块自带压测步骤仍未提供。
- 细节与出处：[modules/README](../../../../modules/README.md)、[命令参考“业务模块”](../../../../codegen/docs/CODEGEN_REFERENCE.zh-CN.md#业务模块roost-module)、[KNOWN-LIMITS“业务模块”](../../../maintenance/KNOWN-LIMITS.md#业务模块roost-module)。
