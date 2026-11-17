# 不带 game 的开房间工程：account_auth 拆分与真实进程演练（2026-11-17）

目标：`roost project new <名> -template lobby` 生成的工程（只托管 account、lobby、relay，没有 game）从客户端登录到 relay 连接全程可用。改动见 CHANGELOG `[Unreleased]` 的“不带 game 的开房间工程能登录、能拿 gt1 了”一条；当前契约在 [LOBBY.md §3.1](../../../framework/LOBBY.md)、[modules/account_auth](../../../../modules/account_auth/README.md)、[modules/portal](../../../../modules/portal/README.md)。

## 设计

- login 里只属于 account 的五个文件（协作者、开发登录、渠道钩子、玩家号计数器和它们的测试）原样搬进新模块 `account_auth`（`git mv`，内容不变，生成出的文件逐字节相同）；login 与 portal 都 `requires: [account_auth]`。用的是现成的 `requires` 机制，没有引入“按工程类型部分安装”。
- portal 不再 import game 侧的包：握手票据的 `enter:` 前缀由入口自己的 `EnterTicketPrefix` 拼，与 login 认证器的前缀由新联动 `portal + login` 的 1 条场景用真实认证器核对。
- 安装器：工程没有运行 Nest 的业务服务时，`needsBusinessService()` 为假的模块（入口全在托管服务上）可以装，记录的 `service` 为空；联动清单新增 `with.<对方>.requires`，`portal + enter` 还要 login。
- 生成器不预装：`-template lobby` 与 `roost help lobby` 的最后一行提示 `roost module add portal`；装之前 doctor 的 `collaborators:account` 报 FAIL 并指向这条命令。

## 文件

| 文件 | 内容 |
| --- | --- |
| [lobby-project.txt](lobby-project.txt) | `-template lobby` 生成、`module add portal`、build / vet / `generate --check` / `module verify`、doctor 装前 1 FAIL、装后 0 FAIL |
| [mkconf.py.txt](mkconf.py.txt) | 演练配置：端口、隔离环境的 Redis / NATS、私有 etcd、键前缀 `lrportal` |
| [run3.sh.txt](run3.sh.txt) | 起停 account、lobby、relay 三个真实进程（生成工程的 `bin/app`） |
| [drillclient.go.txt](drillclient.go.txt) | Go SDK（`client/relay`）客户端流程 |
| [drill-run.txt](drill-run.txt) | 演练每一步的结果（全部 OK） |
| [processes.txt](processes.txt) | 进程起停、运行时 etcd 键 |
| [generated-compare.txt](generated-compare.txt) / [gen4.sh.txt](gen4.sh.txt) | 旧生成器（HEAD 187e4a24）与新生成器对 plain、`-template game`、game-demo、`-template game` + `module add login portal` 的逐文件对比 |
| [verify-game-login-portal.txt](verify-game-login-portal.txt) | 新生成器 `-template game` + `module add login portal` 工程的 `roost module verify` |
| [game-demo-check.txt](game-demo-check.txt) | 新生成器的 game-demo：tidy、build、vet、`module verify`（含三个联动） |
| [head-red-before.txt](head-red-before.txt) | HEAD 上改动之前已经红的两处（login 场景数 37 / 38，占位检查误判 Go 复合字面量） |
| [mutate.py.txt](mutate.py.txt) / [mutations.txt](mutations.txt) | 6 处变异，全部变红 |
| [codegen-targeted-final.txt](codegen-targeted-final.txt) | 模块相关 codegen 用例（`-run`）最终全绿 |

## 演练

环境：`source ~/.roost-it/roost-dataengine-it/env.sh`（只 source，不打印）提供 Redis、NATS；隔离环境没有 etcd，另起私有 etcd（127.0.0.1:23791），用完停掉。端口 19300-19302（ops）、19380（account web）、19381（lobby）、19310 / 19320（relay），避开本机已有的 dev-run。

1. 两个账号 `/portal/login` 带 `game` 各拿到 gt1；B 另经 `/portal/game-token` 再换一张。
2. 与 game 有关的两个端点：`/portal/servers` 200，区服为空、推荐 0；`/portal/enter` 400，`560104`（server_invalid）。
3. A 建房，B 加入，两人连上 relay（A 是房主）。
4. 游戏通道两个方向、房间通道两个方向各收发一帧。
5. A 主动转交，B 收到 `YouAreHost`（handoff），两人都收到 `HostChanged`；lobby 的房主变成 `dev:bob1`。
6. B 踢 A：A 断开（kicked），A 再 join 答 403 kicked，A 拿旧令牌连 relay 收到 `Closed{kicked}`。

三个进程日志 0 条 ERROR；停止后 `ps` 无残留，私有 etcd 已停，演练写入的 `roost:lrportal:*` Redis 键已删（剩 0）。

## 不在这份证据里的

- 跨机器部署、`portal.services` 自动填 lobby 地址（入口的服务清单只有名字没有地址，客户端从配置拿 lobby 地址）。
- 全量 codegen 与 pretag：按分工只跑了模块相关用例。
