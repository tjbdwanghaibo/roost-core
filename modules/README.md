# 业务模块

这里是 `roost module add` 安装的业务模块源码。模块是一个现成的业务单元：状态结构体、组件、Nest handler、协议、错误码和场景测试。安装时把源码写进工程，之后归工程所有，`project sync` / `upgrade` 不改写。

它不是生成器输出。生成器（`roost generate`、`roost add`）只产出框架骨架；模块是业务代码，只是提前写好、验收过。设计与取舍见[业务模块与配方方案](../docs/maintenance/FEATURE-2026-11-10-business-modules.md)。

## 现有模块

| 模块 | 做什么 | 依赖 | 状态字段 | 场景数 |
| --- | --- | --- | --- | --- |
| [wallet](wallet/module.yaml) | 玩家货币：余额；`Add`（调用方自己判重的发放与扣除）；`Apply`（带业务键、在最近 128 次的去重窗口内只生效一次）；消费协议 `WalletSpend`；服务端发放 handler `WalletGrant` | 无 | `PlayerDao.Wallet`（同步）、`PlayerDao.WalletLedger`（只落库） | 8 |
| [checkin](checkin/module.yaml) | 每日签到：按业务日（`clock.BusinessDay`）切分，连续天数加成，奖励经货币模块在同一笔事务里发放 | wallet | `PlayerDao.Checkin`（同步） | 6 |

| [chat](chat/module.yaml) | 聊天：世界频道与一对一私聊的发送（`ChatSend`）、向在线玩家的推送（`ChatMessage`）、按序号翻历史（`ChatHistory`）、给服务端用的系统公告（`AnnounceWorld`）；经托管的 chat 服务存储，替换它默认拒绝一切的 collaborators。game-demo 用的就是它 | 无（要求业务服务 `uses: [chat]` 与玩家 TCP 接入，后者安装时代办） | 无 | 8 |
| [enter](enter/module.yaml) | 进入游戏：`EnterGame` 端点加载或创建 Player，每次进入都跑一遍“首次初始化”（做过没有记在档里），新玩家的初始发放写在 `game/entities/player/enter_first.go`；安装时打开玩家 TCP 接入与自身视图 | 无 | `PlayerDao.Enter`（只落库） | 3 |

以 `roost module list` 的输出为准。一个新工程装 `enter` 加需要的业务模块，就有了登录后的第一条请求、客户端可见的状态和业务本身。

## 使用

```sh
roost project new shop -module example.com/shop -template game
cd shop
roost module list                  # 看有什么
roost module plan checkin          # 只预览：依赖、追加的字段、分配的号、写入的文件
roost module add checkin           # 安装（连同依赖 wallet）并运行生成器
roost module verify                # 模块自带场景 + build / vet / id check / generate --check
```

agent 用 `--json`：`list`、`plan`、`add`、`verify` 都支持；`plan` 与 `add` 输出同一个结构，`applied` 区分是否已执行。

标了同步的状态要打开玩家自身视图客户端才收得到。装 `enter` 模块时安装器会代办（`roost add transport tcp`、`access.player.self_sync: true`、`roost project sync`）；不装它就自己做，步骤见 skill 的[按需求组合 §4A](../docs/agent-skills/roost-genre/references/compose.md#4a-让客户端看到状态)。

安装之后直接改工程里的文件（例如在 `game/entities/player/checkin_component.go` 里改奖励规则），改完再跑 `roost module verify`。模块库里没有的需求用 `roost add entity / component / dao / handler / protocol / endpoint` 手写，可以照着已装模块的文件写。

## 安装做了什么

1. **前置骨架**：实体还没有主 DAO 时执行 `roost add dao <Entity> --entity <Entity>`；模块有协议而工程还没有玩家接入时执行 `roost add access player --service <service>`。
2. **状态**：把模块的状态字段追加到实体主 DAO 的**末尾**。类型是模块自带的嵌套结构体（`db/def/<module>.go`）。
3. **组件**：把模块的组件接到实体上。
4. **文件**：把 `files/` 下的模板渲染后写进工程。落点已被业务文件占用时拒绝，不覆盖。
5. **记录**：在 `roost.yaml` 的 `modules.<name>` 记下这个模块拥有的文件、字段和分配到的号。
6. **生成**：运行全部生成器。

第 2～6 步任何一步失败，模块写过的业务文件与 `roost.yaml` 恢复原样。已安装的模块不再处理，重复执行没有变化。没有卸载命令：分配过的号与存储键不回收，要去掉一个模块时按 `roost.yaml` 里的归属清单手工删。

## 写一个模块

目录结构：

```
modules/<name>/
  module.yaml          清单
  files/<工程内路径>.tmpl   模板，按安装后的真实路径存放
```

### 清单

```yaml
name: wallet                 # 与目录名相同
summary: 一句话说明
requires: []                 # 先装的模块
conflicts: []                # 不能同装的模块
needs:
  mods: [nest]               # 业务服务必须有的 Mod
  features: [protocol, entity, dao, errcode]
  uses: []                   # 业务服务必须 uses 的托管框架服务（rank、mail……）
entity: Player               # 状态与组件挂在哪个实体上
state:                       # 追加到主 DAO 末尾的顶层字段
  - field: Wallet            # 以模块名开头
    type: WalletState        # 模块在 db/def 里定义的嵌套结构体
    store: wallet            # 存储键，以模块名开头，发布后不要改
    dao: persist,sync
    comment: 写进 DAO 定义的注释
component: Wallet            # 挂在实体上的组件
protocols: [WalletSpend]     # 模板里 {{ID:protocol:<name>}} 用到的名字
errcodes: [wallet_short]     # 模板里 {{ID:errcode:<name>}} 用到的名字
access:                      # 要求的玩家接入形态，由安装器代办（见下）
  transport: tcp             # 没有玩家 TCP 接入就先 roost add transport tcp
  self_sync: true            # 打开 access.player.self_sync：标了 sync 的字段推给玩家本人
  lifecycle: true            # Entity 没有 lifecycle 就先 roost add lifecycle <Entity>
replaces: []                 # 要接管的生成桩（见下），必须也在 files/ 里
verify:
  packages: [./game/handler]
  run: ^TestWallet
  pass: 8                    # 恰好这么多条通过、零跳过
```

`replaces` 用于生成器建一次、默认拒绝一切、留给项目实现的文件——目前认识托管服务的 `internal/service/<name>/collaborators.go`。安装时只有它仍是没改过的生成桩才替换；业务改过就拒绝安装。chat 模块用它接管 chat 服务的协作者。替换后的文件里不要再出现 `is not configured` 这句话：`project doctor` 靠它判断“还是拒绝桩”。

`files` 不写在清单里，由 `files/` 目录读出。清单与模板必须对得上：模板用到的每个 `{{ID:...}}` 都要声明，声明的每个名字都要用到。

### 模板占位

| 占位 | 替换成 |
| --- | --- |
| `{{MODULE}}` | 工程的 Go module 路径 |
| `{{PROJECT}}` | 工程名 |
| `{{GAME_SERVICE}}`、`{{GAME_SERVICE_PKG}}` | 模块装进的业务服务名与它的 Go 包名 |
| `{{ID:protocol:<name>}}`、`{{ID:errcode:<name>}}`、`{{ID:component:<name>}}` | 安装时从 `roost.yaml` 的 `ids` 段分配的号 |

### 必须遵守的约定

- **状态自带**：只动自己的状态结构体，不往别的模块的结构体里加字段。挂在已有实体上的状态一律是嵌套结构体，不新建 DAO。
- **同步按顶层字段决定**：`dao: persist,sync` 下发给客户端，`dao: persist` 只落库。既有客户端可见状态又有服务端账本的模块写两个字段（wallet 就是）。嵌套类型内部的字段只写 `store` 键。
- **号不写死**：协议号、错误码、组件号一律用 `{{ID:...}}`。
- **文件名带模块名**：`db/def/<module>.go`、`internal/errors/<module>.go`、`game/handler/<module>_<handler>.go`、`protocol/def/<module>_<protocol>.go`、`game/controllers/<module>/`。协议与 Nest handler 一个一个文件（`roost generate` 按文件名找它们）。
- **自己的 controller 域**：`//roost:protocol ... handler=<module>`，不往别的模块的 controller 里加方法。
- **跨模块协作**：依赖的模块的组件在同一个实体上时，在自己的组件里直接调用它（同一把实体锁、同一笔事务，见 checkin 调 wallet）；不在同一个实体上时用多实体 Nest handler。不用进程级全局变量。
- **测试只断言自己的事实**：同一个 DAO 上还有别的模块的字段。例如断言“我的字段同步、我的账本不同步”，不要断言“同步字段恰好是这几个”。测试替身（getter、提交器）自己带一份，不借用别的模块的测试文件——那些文件归工程所有，可能被改。
- **长期“只做一次”记在自己的状态里**：每日、每局、每个订单这类周期性发放，先判自己的状态再调 `WalletComp().Add`；不要拿递增的键去占货币的去重窗口（它只留最近 128 次）。
- **日界用业务日**：`clock.BusinessDay(time.UnixMilli(nowUnixMilli))`，不自己除以一天的毫秒数。
- **回滚统一走 DAO**：组件不保存事务会改的状态，不登记 undo。
- **业务时间**：handler 的最后一个参数写 `nowUnixMilli int64`，由 Nest 填这条消息冻结的业务时间；测试用 `clock.SetSource` 拨。

### 写的顺序

最省事的做法是先在一个真实工程里把功能手写出来、跑通，再搬成模板：

1. `roost project new demo -module example.com/demo -template game --skip-deps`，`go mod edit -replace` 指向本仓库。
2. 用 `roost add component / protocol / errcode` 和手写把功能做完，`roost generate`，写场景测试并跑通。
3. 把业务文件拷到 `modules/<name>/files/`，加 `.tmpl` 后缀，把 module 路径换成 `{{MODULE}}`、把号换成 `{{ID:...}}`。
4. 写 `module.yaml`。
5. 在全新工程上 `roost module add <name>` 与 `roost module verify`；再和别的模块一起装一遍。

## 改这里的东西之后

```sh
GOWORK=off go test -count=1 -run 'TestModule|TestAppendDaoFields' ./codegen/internal/roost
bash scripts/test-modules-generated.sh
```

第一条包含在生成工程上真实安装并验收的用例（约一分钟）。第二条是发版门禁里的那一条：全新工程、自定义 module 名、逐个模块安装、组合安装、重复安装无变化。
