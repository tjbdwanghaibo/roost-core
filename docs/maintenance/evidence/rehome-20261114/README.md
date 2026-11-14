# 单个玩家换服真实进程演练（B-1，2026-11-14）

基线 `feat/v147`：`086524b35ce7937551e65b55d04d879f7e69449c`。macOS ARM64；只验同机独立进程，不扩大到 Linux、跨机、独立 Gate、account 长时间不可用或容量。

入口 `scripts/test-demo-rehome.sh`；夹具 `scripts/testdata/demo-drill/rehome*.{go.txt,py}`。当前 game-demo 生成两个真实 game（1000、1001），以及 account、alliance 等正式协作者；连接私有 Mongo 副本集、Redis、NATS JetStream，自起 etcd。客户端使用生成 PB 与玩家 TCP；钱包通过正式 GM/Nest 加 100，再经 TCP 扣 7。

故障点只注入到临时生成工程。每个点在对应正式步骤成功后写出含 PID、玩家号、实体号、迁服 ID 的文件栅栏，脚本核对身份才 SIGKILL；没有用 sleep 猜测 kill 时刻。core 的 checkpoint 故障通过 `go build -overlay` 注入，检出仓库的生产源码没有修改。使用原 WAL 重启源服，不搬或删除 WAL。

| kill 点 | 重启后的预期与验收口径 |
| --- | --- |
| accepted：account 已受理、Quiesce 前 | 登录源服触发 Resume；完成后绑定 1001，源档删除 |
| quiesced：会话已断开、既有请求已排空 | 同上；客户端必须实际读到断开 |
| leaving：离开钩子已返回成功 | 同上 |
| handed-off：真实 HandOffEntity 已完成 | 同上；搬档尚未开始 |
| committed：account 已提交、源档尚未删除 | **Q4 仍存在**：目标服可入场且数据连续；源服玩家请求被拒，源库旧副本保留。源服用 account 当前会话登录会在归属检查返回 player_elsewhere（100015，owner sid 1001），没有自动清扫或 Error 重试闭环，需运维对账；不能写成“源档自动清理已验收” |
| dropped：源档已删除 | 重启后只有目标档；目标服可入场 |
| checkpoint（额外因果对照） | 实际 Mongo 投影后、真实 WAL Ack 前暂停；HandOffEntity 必须等待。放行 Ack 后才允许交出，随后 SIGKILL/重启与上面相同 |

每个案例核对：account 在途/最终绑定；在途两服都拒绝入场；最终目标入场、背包 10→11、钱包仍 93（重发相同扣款键不再扣款）、旧服拒绝；两侧完整 Mongo 文档与版本留证；正常停机后两侧 `sidmove wal-drained`。committed 窗口作为 `KNOWN_LIMIT Q4` 输出，不因整套脚本退出 0 而隐去。这里的单一归属是 account 绑定与玩家入口；旧源档预加载后定时器仍可能写源库的既有边界没有被隔离或验证，Q4 的清理/隔离策略仍待维护者决定。

## 复跑

依赖和演练在同一 shell 生命周期中运行，避免工具执行环境回收未 fork 的后台 NATS。选择空闲 offset（不超过 38135）和应用端口；不使用共享 `~/.roost-it`，不 reset 共享环境。

```sh
export ROOST_MIRROR_LOCAL_HOME=/private/tmp/rehome-private
export ROOST_MIRROR_LOCAL_OFFSET=34000
bash scripts/mirror-local.sh up
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
GOWORK=off GOFLAGS=-p=2 DEMO_REHOME_OUT="$PWD/artifacts/rehome-run1" \
  bash scripts/test-demo-rehome.sh
GOWORK=off GOFLAGS=-p=2 DEMO_REHOME_OUT="$PWD/artifacts/rehome-run2" \
  bash scripts/test-demo-rehome.sh
# 必须以 CHECKPOINT_BYPASS 非零退出；它只改临时 overlay。
GOWORK=off GOFLAGS=-p=2 DEMO_REHOME_STAGES=checkpoint \
  DEMO_REHOME_MUTATION=skip-checkpoint DEMO_REHOME_OUT="$PWD/artifacts/rehome-mutant" \
  bash scripts/test-demo-rehome.sh
bash scripts/mirror-local.sh down
```

`DEMO_REHOME_PORT_SHIFT` 缺省 29000，`DEMO_REHOME_ETCD_PORT` 缺省 39579。证据目录必须分别指定。脚本清理自身生成工程与测试命名空间；私有依赖由调用方停止。保留原始失败日志，不把未运行或编译成功记成进程验收通过。

## 运行结果

最终脚本连续两遍通过，输入脚本 / 夹具 SHA-256 相同。每遍六个指定 SIGKILL 点加一个 checkpoint 因果场景，共 14 个案例；两遍源服与目标服正常停机后的 WAL 均排空。结果字段、输入摘要见 [results.json](results.json)，原始进程日志、两侧文档与绑定快照保留在工作树 `artifacts/rehome-run1`、`artifacts/rehome-run2`。

`waitProjectedRecordsConfirmed` 的日志共 18 个去重样本：16 个普通样本为 0～29,926 µs，两个主动压住 Ack 的样本分别为 327,755、321,758 µs。测量发生在并行开发负载下，只是这次等待阶段的观测，不是 HandOff 全程延迟或性能门限。

变异 `DEMO_REHOME_STAGES=checkpoint DEMO_REHOME_MUTATION=skip-checkpoint` 退出 1：`CHECKPOINT_BYPASS: hand-off returned before checkpoint release`。现场同时存在 `checkpoint-held` 与 `reached.json(stage=handed-off)`，没有 `checkpoint-waiting`，证明是实际 Mongo 投影后的 Ack 被压住时提前交出；没有把编译失败当作变异红。原始日志与标记保留在 `artifacts/rehome-mutant`。静态检查已通过：`bash -n`、Python 编译、`git diff --check`，以及 `GOWORK=off GOFLAGS=-p=2 go test -run '^(TestRealProcessDependencyEntriesRemainReachable|TestTrackedGoFilesAreGofmtClean)$' -count=1 .`。未跑全量慢套件。

开发期的失败原文保留在本地 artifacts 和汇总证据中：

- dev1：独立工具会话的 `mirror-local up` 结束后 NATS 被工具环境回收，服务启动报 `no servers available`；改为同一 shell 生命周期管理依赖与演练。
- dev2：开发中修改了正在被 Bash 读取的脚本，读取偏移失效；后续固定脚本版本运行，不计作产品失败或验收。
- dev3：客户端未启动 bootstrap，计算 Mongo 主键时报 `kind 1 category is not registered`；夹具显式登记生成 Player 声明的 kind/category。
- dev4：六个 SIGKILL 点均通过，但 checkpoint 额外场景未建立前提。最初压住 RehomePrepare 的 Ack，demo 交出前还会经 LeaveScene 提交最后位置，导致真实 HandOff 正确地等那条“未投影”记录，30 秒后中止。故障目标改为指定玩家最后 LeaveScene 的已投影 Ack，确保检验的是“投影已完成但 checkpoint 未确认”；没有改变业务行为或放宽断言。

## 最后文档批的回写建议

B-1 对应 remaining.md No.11、61、65 的验证部分、68 的真实进程部分与 No.91 的同机部分。完成证据只关闭同机六点的验收缺口；Q4 搬档崩溃窗口、Q14 登录触发恢复、跨机 / Linux / HA 与独立 Gate 的未测部分继续保留。KNOWN-LIMITS 由最终文档批统一修改。


可直接回写的文字（同机验收与 Q4 分开）：

> **两服真实进程换服验收**（2026-11-14）：`scripts/test-demo-rehome.sh` 在 macOS ARM64 私有 Mongo 副本集 / Redis / NATS / etcd 上连续两遍通过，受理、断开排空、离开钩子、交出实体、account 提交、删除源档六点分别 SIGKILL 源服并用原 WAL 重启；前四点由登录续做。核对 account 单一绑定、两服玩家入口、背包 11 / 钱包 93 与扣款去重、Mongo 到达次数 1、源档和最终 WAL 排空。额外 checkpoint 对照要求真实投影已完成但 Ack 被压住时 HandOff 等待；移除该等待的变异以 CHECKPOINT_BYPASS 失败。该结论不包含 Linux、跨机、独立 Gate、account 长时间不可用与物理断电。

> **搬档提交后的崩溃窗口（Q4，未关闭）**：account 已改绑、源档尚未删时源服崩溃，目标服可入场且资产连续，但旧源档在源库保留。源服用 account 当前会话登录会在归属检查返回 player_elsewhere，不保证触发补搬、删除或 Error 重试；需运维对账。源档预加载后定时器仍可能写旧源库的既有边界未被本次验收隔离或验证，清理 / 隔离策略待维护者决定。不能把 account / 玩家入口唯一归属描述为所有旧内存写入都已隔离。
