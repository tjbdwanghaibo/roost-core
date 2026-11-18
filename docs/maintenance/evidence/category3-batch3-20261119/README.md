# 第三类：未归因偶发 / 疑点的复现与否证，第三批（2026-11-19）

范围：维护者 11-18 定的第三类剩下三项——E-2（No.32②，C++ 客户端高负载弱网偶发）、CellWorld `AddIngress` 失败分支用例（B-12）、PhysX + LevelComponent“玩法联合链”一条。前两批见 [category3-anomalies-20261118](../category3-anomalies-20261118/README.md)、[category3-batch2-20261118](../category3-batch2-20261118/README.md)。

机器：macOS 26 arm64（Apple M5，10 核），**不空闲**：桌面应用常驻（CorpLink、Claude、WebKit 等），swap 已用约 6.2G / 7.1G（全程不变）。三项串行，同一时间只跑一组负载。基线为 main `a2866561`（v1.57.1）。codebase-memory-mcp 本次连不上（连接关闭），代码定位按当前源码与 rg。没有起私有依赖，没碰 `~/.roost-it` 的共享数据。时间按本地（UTC+8）；`e2/load40-env.txt` 里的时刻是 UTC。

## 1. E-2：C++ `native_weak` 的 `send_stall` 在高负载下报 `send failed`

### 原始现象（2026-11-06，`6771a0ac` 登记）

load average 40～55 时，`native_weak` 的 `send_stall` 模式约 10 次里报过 1 次 `send failed`，没有现场。

### 夹具与契约（读源码）

- `client/cpp/tests/weak.py` 的 `send_stall`：服务端握手后把接收缓冲设成 4096、发出就绪包，然后**固定睡 0.8s 再关闭连接**（停读）。
- `client/cpp/tests/weak.cpp`：收到 `ROOST_READY` 后发一个 16 MiB 请求，`request_timeout_ms = 200`；期望关闭原因含 `socket send timeout`。
- `client/cpp/src/client.cpp` 网络线程：循环开头先查发送期限（到了就 `socket send timeout: result unknown`），再 `select`，再 `send`；`send` 返回非“会阻塞”的错误时抛 `send failed`；`recv` 读到 0 是 `peer closed`。任何一种都走 `fail()`：终结连接、作废全部在途请求（`ROOST_CLOSED` 带原因）。

所以客户端从“就绪”到“请求进入发送”这段时间是夹具没有控制的：服务端的关闭时刻固定在就绪后 0.8s，客户端的发送期限是“请求时刻 + 200ms”。前者若先到，客户端如实报的是对端关闭引起的错误，而不是发送超时。

### 做了什么

**(a) 高负载复现（未插桩的判定逻辑 + 只在失败分支加信息的诊断构建）**

- 诊断副本（只在会话 scratchpad 构建，不入库，精确补丁 [e2/diag.diff](e2/diag.diff)）：`send failed` / `receive failed` / `peer closed` 的原因串后面附 errno、已发 / 总字节、距就绪、距请求进入发送、距发送期限；`weak.cpp` 在发请求前打印主线程“就绪 → 请求”的毫秒数；`weak.py` 打印服务端实际睡了多久。另加只在设了 `ROOST_DIAG_DELAY_MS` 时生效的人为延迟（用于 (b)）。成功路径的判定与正式代码一致。
- 构建：`cmake -S <diag-src> -B <scratch>/diag-build -DROOST_BUILD_TESTS=ON && cmake --build <scratch>/diag-build --target roost_native_weak -j4`（AppleClang，build type 空，与正式脚本一致）。
- 负载：40 个自有 `yes >/dev/null`，先预热 60s；然后 `ctest --test-dir <diag-build> -R '^native_weak$' --timeout 60 -V` 连跑 200 轮，每轮记 1 分钟 load，失败时存 `lsof` / `netstat`。驱动 [e2/run-load.sh](e2/run-load.sh)（`caffeinate -i bash run-load.sh <diag-build> <out> 40 200 540`），退出时 kill + wait 自己的 40 个 `yes`。
- 结果（[e2/load40-rounds.txt](e2/load40-rounds.txt)、[e2/load40-env.txt](e2/load40-env.txt)）：**200 / 200 轮全过**（每轮恰好 5 个 `PASS weak`，共 1000 个场景），用时 5 分 45 秒。1 分钟 load 26.97～42.66，**138 轮在 load ≥ 40 时跑**（比 11-14 那轮的最大 10.39 高得多）。200 轮里主线程“就绪 → 请求”全部是 0ms，服务端实际睡 800～809ms，单轮 CTest 1.59～1.91s。收尾核对 40 个 `yes` 全部不在。

**(b) 机制注入：让“就绪 → 请求”变慢，看会报什么**

[e2/inject.sh](e2/inject.sh)：同一诊断二进制、无额外负载，`ROOST_DIAG_DELAY_MS` = 0 / 400 / 550 / 650 / 1000 / 2000，每档 3 次。

用正式（HEAD）的 `weak.py`（[e2/inject-red.txt](e2/inject-red.txt)）：

| 延迟 | 结果（3 / 3 一致） |
| --- | --- |
| 0、400ms | PASS，`socket send timeout: result unknown` |
| 550ms | **`send failed [diag errno=32 sent=1021868/16777232 since_ready_ms=808 since_admit_ms=103 deadline_in_ms=60]`**——与历史原文同一句，EPIPE，离发送期限还差约 60ms 时服务端已关闭 |
| 650ms | `peer closed`（请求刚进入发送、服务端已关闭） |
| 1000、2000ms | `request not admitted`（请求之前连接已因对端关闭终结） |

16 MiB 请求的构造与拷贝本身约占 100ms，所以主线程在就绪后被拖住约 0.5s 以上，原夹具就必然变红，且红的文字正是历史上的 `send failed`。

### 归因与处理

- **归因：测试计时（夹具），不是客户端缺陷。** 客户端在“发送期限之前对端已关闭”时报对端关闭类错误、终结连接、作废在途请求，符合 `fail()` 的契约；`send_stall` 想验的是“对端停读时客户端靠发送期限退出”，但夹具让对端关闭与客户端的发送期限赛跑。注入实验确定性地复现了历史原文；历史那一次是否正是这个原因**没有现场**，属推断（同一句错误文本、同样只在重负载下出现）。
- **纯 CPU 负载复现不了**：40 个 `yes` 把 load 推到 40+，主线程延迟仍是 0ms——macOS 调度器对短睡眠后醒来的线程照顾得很好。历史的 load 40～55 来自并行编译 / race 测试（内存压力、IO），这次没有模拟那种负载，不说“load 40 已验证无问题”。
- **夹具修正**（`client/cpp/tests/weak.py`，只改测试）：`send_stall` 的服务端停读后**一直持有连接，直到客户端进程结束**（`threading.Event`，最多 6s），不再固定 0.8s 后关闭。修后同一注入（[e2/inject-green.txt](e2/inject-green.txt)）0～2000ms 六档各 3 次全部 `PASS weak send_stall socket send timeout: result unknown`；`scripts/test-client-cpp.sh` 5 / 5 通过；未插桩构建上 `native_weak` 另跑 5 次全过。
- 修后的保证与限制：`send_stall` 只可能以客户端自己的发送期限结束，与调度延迟无关；仍是本机回环上的应用层停读，不是 WAN 丢包 / 真实移动网络（`weak.py` 头注释原样）。客户端侧 `select` / 期限检查的顺序没有改（不需要改）。

## 2. B-12：CellWorld `AddIngress` 失败分支的用例

### 结论：已在 2026-11-17 补上，本轮在当前 HEAD 上复验，没有新增代码

remaining.md 的状态表 B-12 行还写着“AddIngress 失败分支仍缺用例”，是没跟上：用例 `TestCellworldStartAfterInputsClosedTakesTheNodeDown`（`modules/cellworld/files/game/modules/cellworldhost/runtime_test.go.tmpl`）随 [d-small-items-20261117 §3](../d-small-items-20261117/README.md) 合入，KNOWN-LIMITS 的“CellWorld Runtime”一条早已写明。契约见该证据 §3（Node 就绪后 `app.AddIngress` 因 App 已关输入失败时，`Start` 停下 Node、从 Host 拿下、回到未启动，错误点名 `"cellworld"`）。

### 复验（[b12/](b12/)）

- 全新 `-template game` 工程（`go mod edit -replace` 指回本 worktree）`roost module add cellworld`，然后 `roost module verify cellworld`：**17 passed (want 17), 0 skipped**，go build / go vet / id check / generate --check 全过（[b12/cellworld-verify.txt](b12/cellworld-verify.txt)；17 = 10 条装配契约 + 11-17 后加的 7 条实体适配）。
- `go test -mod=mod -race -count=1 -v -run '^TestCellworld' ./game/modules/cellworldhost ./game/managers`：17 条全过（[b12/cellworld-race.txt](b12/cellworld-race.txt)）。
- 变异：沿用 [cellworld-mutate.py](../d-small-items-20261117/cellworld-mutate.py) 在渲染出的 `runtime.go` 上做 4 条变异、每条只跑该用例、跑完恢复：**4 / 4 被杀**（[b12/cellworld-mutation.txt](b12/cellworld-mutation.txt)）——M1 忽略 AddIngress 错误 → `Start succeeded although the App no longer accepts inputs`；M2 失败不停 Node → `Node.Run still holds 2 subscriptions`；M3 不调 `fail` → `left the stopped Node's done / cancel behind` + `still attached to the host`；M4 `fail` 不 detach → `the refused Runtime is still attached to the host`。
- 没发现缺陷，不改代码。仍保留：只在 macOS 本机跑；Linux race 20 轮属 C-L6；业务 Entity 适配的分工按现状（D21 后半）。

## 3. PhysX + LevelComponent“玩法联合链”

KNOWN-LIMITS 原文：“PhysX 动作执行器与 LevelComponent 的真实联合玩法尚未验收；RSS 页来源未逐区域核对。”这一条没有写复现方式，两半性质不同，分开处理。

### 3A. “PhysX 动作执行器与 LevelComponent 的真实联合玩法”：不是偶发，是没有实现的集成

- 读源码：`gameplay/level` 只产出动作请求（`PendingActions` / `RequestSettled`），执行器由宿主提供；仓库里没有任何代码把 LevelComponent 的动作接到物理房间（`rg "gameplay/level"` 只有 dungeon 模块在用；`wiring/physicsroom` 里的 `level` 是物理关卡描述，不是 `gameplay/level`）。
- PhysX native ABI（本机链出的 `libroost_physx.dylib` 导出）只有 `rp_create / destroy / add_pose / step_control / read / joint_add / joint_remove / version / abi_version`：没有传送 / 重生（直接设动态刚体位姿）、没有区域触发事件。[LEVEL-COMPONENT](../../../framework/LEVEL-COMPONENT.md) 已写明示例里的 checkpoint / spawn_wave / settle 都是测试替身，“不表示 native PhysX 已具备传送／重生”。
- **归因：框架范围边界（未实现的集成），没有可复现的东西。** 要“验收联合玩法”得先做一个真实执行器（以及 native 侧的传送 / 区域事实），这是功能工作，需要维护者决定做不做（见汇报里的 A / B）。在决定之前按“一”的限制写，不再挂在遗留里当待查问题。

### 3B. “RSS 页来源未逐区域核对”：本轮逐区域核对了一次（macOS，8 分钟）

原结论（[PHYSICS-ROOM-SERVICE 长稳一节](../../../framework/PHYSICS-ROOM-SERVICE.md#长稳rss-与原生内存2026-10-29macos-arm64)，10-29 20 分钟样本）：RSS 每分钟 +0.035MB、footprint 持平、RSS − footprint 11→15.8MB，“推断为干净页换入，没有逐区域核对”。

做法（[physx-level/](physx-level/)）：

- 本机 PhysX 4.1.2 静态库（`/Users/whb/roost/.physics-deps/build`）与当前 `bridge.cpp` 用 clang++ 链出 `infra/physics/physx/lib/libroost_physx.dylib`（gitignore，用完删除）；`GOWORK=off CGO_ENABLED=1 go test -c -tags physx ./wiring/physicsroom`。
- `TestPhysicsRoomSoak`（机关 8 房间 × 20 逻辑客户端），`ROOST_PHYSICS_SOAK_MINUTES=8 ROOST_PHYSICS_SOAK_INTERVAL=15`，`caffeinate -d -i`（[physx-level/run.sh](physx-level/run.sh)）；运行中在 45 / 120 / 210 / 300 / 390 / 465s 取 `ps` RSS、`footprint`、`vmmap --summary`，另在 02:53:49 与 02:57:31 两次取逐区域 `vmmap -w -interleaved`（[physx-level/detail.sh](physx-level/detail.sh)，按“区域类型 / 共享模式 / 映射文件”汇总后求差，[physx-level/regions.py](physx-level/regions.py)）。vmmap 会短暂挂起目标进程。负载 6.18（起）→ 3.63（止），测试 PASS，物理均频 50.00Hz（两端窗口 47.45 / 48.76，含 vmmap 挂起）。

结果：

| 时刻 | RSS | footprint | RSS − footprint | Go 映射 VM_ALLOCATE 常驻 / 脏 | MALLOC_SMALL（PhysX 堆） |
| --- | ---: | ---: | ---: | --- | --- |
| 45s | 54.7M | 41.8M | 12.9M | 28.0M / 28.0M | 9888K |
| 120s | 56.5M | 41.9M | 14.6M | 29.7M / 28.0M | 9888K |
| 210s | 57.1M | 42.7M | 14.4M | 30.2M / 28.9M | 9888K |
| 300s | 57.2M | 42.3M | 14.9M | 30.3M / 28.4M | 9888K |
| 390s | 57.2M | 42.5M | 14.8M | 30.3M / 28.6M | 9888K |
| 465s | 57.3M | 42.9M | 14.4M | 30.3M / 29.0M | 9904K |

逐区域两点差（A→B，3.7 分钟，[physx-level/regions-a-b.txt](physx-level/regions-a-b.txt)）：ps RSS +160K；本进程私有部分里 Go 映射（VM_ALLOCATE PRV/COW）常驻 +96K、测试二进制 `__TEXT`（干净、文件映射）+48K、MALLOC_SMALL +16K、栈 ±32K；`libroost_physx.dylib` 各段与 MALLOC_TINY 不变。系统共享缓存的 `__TEXT` +1.2M 不计入 ps RSS（vmmap 总常驻约 297M，ps RSS 约 57M）。

判断：

- **确认（本次条件）**：RSS 的增长集中在前 2～3 分钟（Go 映射常驻 28.0 → 30.3M），之后每分钟约 +0.04MB，与 10-29 的斜率同量级。逐区域看，这段增长落在两处：Go 映射里**常驻但不脏**的页（VM_ALLOCATE 常驻 − 脏 在 120s 后稳定在 1.3～1.9M，与 Go 的 `go_released_bytes` 3.0～4.7M 同向——Go 归还给系统的页在 macOS 上没有内存压力时仍留在常驻里，footprint 不算它们）和测试二进制代码段的按需换入（干净页）。PhysX 原生堆（MALLOC_SMALL 9888→9904K）与 dylib 各段不增长。没有发现泄漏形态。
- **修正原推断**：原文“增长落在干净页（代码段等文件映射）”只说对一部分；本次样本里更大的一份是 Go 已归还但仍常驻的页，也不进 footprint。
- **限制**：8 分钟单次、macOS 本机、两个逐区域点；20 分钟原样本的 RSS − footprint 11→15.8MB 本次是 12.9→14.4～14.9M（早期上升后平），没有逐区域重做 20 分钟；Linux 的 RSS / 原生堆仍是 C-L7，小时级与房间反复建销仍是 C-S4。

## 收尾

- 自有进程：40 个 `yes`（驱动退出时 kill + wait，核对 0 个残留）、soak 测试进程（正常结束 exit 0）；没有起私有依赖。收尾 `ps` 检查见汇报。
- 构建产物（`e2/diag-build`、`e2/base-build`、`b12/cwtest` 工程、`e3soak.test`）在会话 scratchpad，不入库；`infra/physics/physx/lib/libroost_physx.dylib` 用完删除。
