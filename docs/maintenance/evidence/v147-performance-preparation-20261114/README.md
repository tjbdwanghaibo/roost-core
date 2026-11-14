# v1.47 性能测量准备交接（2026-11-14，NOT RUN）

**正式 M1–M19、E1、E3 和 B2 规模测量均未启动：0 / 117 个调度调用。** 本目录保存命令、构建来源和调度器，不是性能结果。用户在正式窗口开始前要求交给下一位 agent；下一位须先按[总交接](../../HANDOFF-2026-11-14-v147-validation-performance.md)确认最终验证进度和独占窗口，再开始。没有运行的格不写 PASS、吞吐或容量结论。

E2 原生弱网调查已经单独完成，见[原始报告](../v147-native-weak-20261114.md)：原版 200 轮在实际负载下未复现，不是本矩阵的结果。M12 已有真实 2 人功能冒烟和离线报告守卫负向证据，见[入口说明](../../../../scripts/testdata/demo-robot/README.md)，不替代 100 / 200 人六档。

## 文件与已完成准备

- [run-segment.sh](run-segment.sh)、[run-micro.py](run-micro.py)、[timed-command.py](timed-command.py)：保留原 scratch 命令矩阵，改为相邻文件定位、可指定仓库 / 二进制 / 输出目录；每个段调用独立，非零即停。没有自动循环所有段。
- [demo-robot-retained.py](demo-robot-retained.py)：对固定 SHA-256 的正式 `scripts/perf/demo-robot.sh` 生成仅改变定位与清理的副本；负载、断言、成功统计不动。副本、原文、diff、hash 放在本次新输出目录。
- [prepared.json](prepared.json)、[binaries.sha256](binaries.sha256)、[toolchain.txt](toolchain.txt)、[mail-build.json](mail-build.json)：本机实际准备记录。`prepared-head.txt` 是最初准备时的工作树 HEAD，不代表所有二进制同一源码基线；以每类来源为准。
- [aoi-no-branches.patch](aoi-no-branches.patch)、[sync-aoi-views16.patch](sync-aoi-views16.patch)：只用于诊断构建，不合入生产实现；为避免补丁上下文的空白告警，入库为零上下文形式，使用 `git apply --unidiff-zero`，原摘要见 [diagnostic-patch-origins.json](diagnostic-patch-origins.json)。
- [scratch-sha256.json](scratch-sha256.json)、[packaging.diff](packaging.diff)、[entry-sha256.json](entry-sha256.json)：原 scratch 来源、入库定位 / 输出隔离变化和交接入口摘要。运行器只做了语法、路径与只读计划核对，尚未实际跑一段，不能称执行编排已实测。
- 原始准备材料仍在 `/Users/whb/roost/worktrees/v147/artifacts/perf/v147-remaining/`；原 scratch 位于 `/private/tmp/v147-run-{segment.sh,micro.py}`。**推荐从本目录入口继续**，其内层标签包含 `output_name`，且 M12 副本可重新生成，不依赖旧临时文件。

本机 Go 1.27.3、macOS arm64、10 个逻辑 CPU、32 GiB；正式运行仍须重新记录主机状态。`GOMAXPROCS=0 GOWORK=off GOFLAGS=-p=2`；M12 原入口还使用 `-mod=mod`，没有改动。已校核 14 个二进制摘要（含保留的修前 mail）一致。

## 16 段、117 次调用

“调用”是调度器启动一个测试二进制或脚本，不是测试断言数、业务请求数或样本数。每段包含 setup / down 的总预算 1800 秒，Python 留 120 秒收尾；实际耗时可能超过原计划估算，须用新段续跑，不能延长原段或丢掉失败样本。

| 顺序 / 段名 | 调用数 | 工作与固定配置 | 状态 |
| --- | ---: | --- | --- |
| `s1-micro` | 33 | M2 Move count6；M9 Relation 50x count3；M18 Group 200x count3；M5/E1 base、head、no-branches 按此顺序交替 10 轮 | NOT RUN |
| `s2-heartbeat` | 24 | M1 两轮 cb=0/5/10/20µs × hot=0/64；另两轮 group/slice=2/50、4/100、2/100、4/50µs，cb10 | NOT RUN |
| `s3-sync` | 12 | M4 periodic/on_change 交替两轮；M8 sync/async 交替两轮，16 视野诊断构建；M14 demo/game 两轮，各 60s | NOT RUN |
| `s4-nest-game` | 4 | M3 batch/dispatch 两轮；1000 玩家、10000 实体、10Hz、dirty5、embedded TCP、Sync、short8/io8/q65536、每档5m | NOT RUN |
| `s5-gate-budget` | 3 | M6 Gate+self_sync 15m，CPU0/short8/io8/q65536；M7 healthy1000/stalled50、4096B、20/100Hz 各120s | NOT RUN |
| `s6-catchup-mail` | 3 | M10 3000 帧，**256/64 KB/s**；M13 standalone/Cluster，同一修后 mail.test，2000x count3 | NOT RUN |
| `s7-block-aoi` | 2 | M16 concentrated，blocks0/4，各10m；CPU0/short8/io8/q65536，1000/10000，Hz1/dirty1 | NOT RUN |
| `s8-alliance-effects` | 6 | M11 effect workers16/1 交替3轮，512写额度，RETRY=1；测量 Join 最多8次 attempt | NOT RUN |
| `b2-footprints` | 2 | 真实业务500成员、16镜像；合成1000/5000/10000成员、16 DTO副本单列 | NOT RUN |
| `s9-demo100` | 3 | M12 100人：同秒、20/s、10/s；每档全新私有依赖与工程 | NOT RUN |
| `s10-demo200` | 3 | M12 200人：同秒、20/s、10/s；每档全新私有依赖与工程 | NOT RUN |
| `s11-cellworld-robot` | 1 | M15 完整 CellWorld Robot 1000人，60s、active5、ROBOT_ONLY+CAPACITY | NOT RUN |
| `s12-gate-pacing` | 5 | E3 独立 Gate，100/s、30s，共5轮，CPU0 | NOT RUN |
| `s13-generated-tcp` | 4 | M17 50连接各300s：strict独立实体、strict+self_sync、pipelined独立实体固定开环1500/s、pipelined同实体闭环 | NOT RUN |
| `s14-checkpoint128` | 6 | M19 同一实验树，background_checkpoint=0/1 交替3轮，concurrency128，STACKS=1 | NOT RUN |
| `s15-checkpoint256` | 6 | M19 同上，concurrency256 | NOT RUN |

合计 117；旧 remaining.md 写14段，本次把两种 checkpoint 并发分段，并单列 B2。精确参数以 `--plan` 输出为准，原需求见 [remaining §3](../known-limits-remaining-20261114/remaining.md#3-m空闲机器上的运行序列)。其旧 M10 数值是误用字节，应为 KB/s；M6/M16 的旧 CPU4 已按本轮要求改0；M16 显式8/8/q65536；不执行旧 M12 `clean` 示例。

## 接手与执行

这是 macOS 调度入口（状态采样用 `sysctl`、`pmset`）；不声称 Linux 运行已验证。先完成总交接要求，确认没有其他 agent 编译 / race / 索引 / 功能负载。每段开始实测 load，约2–4可作为本机空闲参考；记录真实值、swap、电源和进程，不为获得某个 load 数字额外造负载。每段结束核对自有孤儿进程和依赖端口，再开始下一段。

本机 integration 是 `/Users/whb/roost/worktrees/v147`；报告工作树基于 `48ed0113`，其自身不含后续邮件修复，**不能把报告工作树当最终运行源码**。运行树至少应包含 `d2457b26` 邮件夹具修复，实际最终 HEAD 由总交接确定；编译前再核对受测源码与已准备二进制来源。M13 已从 `fef75aee` 编译。

```bash
export ROOST_PERF_REPO=/Users/whb/roost/worktrees/v147
export ROOST_PERF_BINS=/private/tmp/v147-perf-bin
export ROOST_PERF_OUTPUT_ROOT="$ROOST_PERF_REPO/artifacts/perf/v147-remaining"
export ROOST_CHECKPOINT_TREE=/Users/whb/roost/worktrees/v147-background-checkpoint
entry="$ROOST_PERF_REPO/docs/maintenance/evidence/v147-performance-preparation-20261114"
# 合入本报告后在 integration 使用该 entry；或将 entry 指向报告工作树同目录。
bash "$entry/run-segment.sh" s1-micro --plan
# 只有独占窗口明确开始后才执行：
bash "$entry/run-segment.sh" s1-micro
```

不把16段包进无人检查的总循环。各段 stdout 要另存，新结果目录已有时会拒绝运行。`runs.json` 保存逐次 exit / 命令 / 环境 / 起止 / load / swap / battery；`segment-result.json` 分别保存 attempted（已尝试启动）、finished（已记录退出，含失败/超时）、succeeded（exit0）、failed（非零）、unfinished（已尝试但运行器异常未写完记录）、unstarted（从未尝试）标签；相邻 `.control/` 保存 up/down 与包含依赖的总时长。`active.json` 是当前自有子进程定位，前后 `processes-*.txt` 是排查材料；它们不是自动证明没有孤儿。首次运行前用 `--plan` 检查可执行文件存在，工具版本与端口可用。

需要依赖的段在**同一 shell 生命周期**里 up → source 私有 env → run → down；up 失败立即进入 down，不运行负载。默认 `ROOST_MIRROR_LOCAL_HOME=/private/tmp/v147-formal-mirror-20261114`、offset18000。该私有根与全部端口归本轮独占，不是共享缓存；若已被其他工作使用，重新协调一个全新的 HOME/端口，记录变更。只 `down`，保留数据，不 `clean` / reset / 删目录。fixture 自己默认的唯一键前缀 / 临时工程回收仍按原逻辑执行，报告注明范围。

M12 内部独立使用每档 `$out/mirror`、offset36600、应用shift31000、etcd35479，perIP512；启动前原入口检查完整端口组合。六档绝不复用前档数据。保留副本将 `drill_cleanup` 改成停止本工程进程、保留日志并停本例 etcd，将 mirror `clean` 改 `down`。源脚本 SHA 固定为 `19e37b8de92ae98e062ee45151ac1ef4e7cca408e0d18682ccbb7d5ea3c5b77d`；变化时先审计并重新记录摘要，不绕过守卫。M12 helper 尚未实际运行过；以前的2人冒烟用的是原正式入口。

### 失败、期限与续跑

普通启动 / 配置 / 断言非零先停、保留并分析，不能直接宣称通过或无条件跑后面。已知 M8 “任意样本超过50ms”门禁退出1，可在确认仅此原因后继续独立未运行格；报告仍保留 exit1，同时列 P99/max/超限数，不能用 P99 合格替换原门禁。

```bash
# 例：已完成 M3 第1、2格后段预算不足，只预览剩余两格。
bash "$entry/run-segment.sh" s4-nest-game \
  --output-name s4-resume1 --select 'm3-(3|4)-.*' --plan
# 确认窗口、前次收尾与新目录后，去掉 --plan 执行。
```

`--select` 对完整 run label 做 regex fullmatch。运行器不自动判断“是否先前已跑”，操作者依据所有 `runs.json` 及 failed / unfinished / unstarted 列表选择，不能只看 unstarted 就认定其余已验；finished 只表示已记录退出，不表示成功。失败格是已执行的失败样本，不能当作“未跑”悄悄补绿。确需诊断复跑时使用新名称、注明原因、保留两份。M3/M4/M6/M16/E3 的内层 `ROOST_PERF_LABEL` **总是追加本次 output_name**，plan 与实际一致，避免正式脚本内层目录冲突；M12/M17/联盟日志等从新外层 out 派生。每次续跑用全新且全局唯一的 output_name。

每段共享从 shell 开始计算的1800s截止，负载在截止前120s结束预算；超时先向自有进程组 TERM，最多10s再 KILL，外部依赖命令也有期限。异常终止不保证 daemon 已收尾：保留目录、通过本轮 PID/路径核对后只停本轮进程。原始数据、未知 WAL、业务库、其他 worktree 均不删除。不要通过延长门槛掩盖 timeout。

## 二进制来源与复建

本机现成二进制在 `/private/tmp/v147-perf-bin`，摘要见 [binaries.sha256](binaries.sha256)。文件缺失可按下面流程在**新的**构建根复建；不能 reset / clean 已保留的实验工作树。新编译路径、Go版本和 VCS 信息可能改变二进制 hash，必须保存新源码提交、补丁、编译命令和摘要，不能假称与旧二进制逐字一致。

| 二进制 | 源码 / 构建 |
| --- | --- |
| `aoi-base.test` | `b4699758`，`go test -c ./framework/sync/entitysync/policy` |
| `aoi-head.test` | `c91b6d93`，同包 |
| `aoi-no-branches.test` | `c91b6d93` + 本目录 `aoi-no-branches.patch` |
| `hb-g{2,4}-s{50,100}.test` | `ee5ad2a4`，只改 `framework/nest/try_batch.go` 两个常数；`./gameplay/heartbeat/internal/bench` |
| `sync-aoi-views16` | `357263ba` + 本目录 `sync-aoi-views16.patch`，`go build ./scripts/perf/sync-aoi` |
| `movement.test` / `cellworld.test` / `gateway.test` | `357263ba` 的 `./gameplay/movement` / `./gameplay/cellworld` / `./infra/network/gateway` |
| `gate.test` | `357263ba`，`-tags integration ./wiring/gate` |
| 当前 `mail.test` | `fef75aee`，`-tags integration ./service/mail`；修复提交 `d2457b26` |
| `mail-before-cluster-357263ba.test` | 原 `357263ba`，只保留作配置失败证据，**不能用于正式 M13** |

当前 mail SHA-256：`be360a92d2a7a28707d992cc990cb3e1e120d733aea780752dfc6e53bcffc89d`；旧版 `2a9fa377ea722d837a1b9b1a9c2e371c66c909597161392ed84f77d0d2c15852`。最初 sandbox 因 Go cache 写入拒绝，保留失败日志后授权升级编译成功；此处只有编译，不执行 benchmark。父任务另有 [Cluster 夹具修复红绿](../mail-cluster-probe-20261114.md)，1x 功能样本不替代正式2000x。单机和 Cluster 两档必须使用同一修后二进制。

以下是完整复建示例，**交接整理时没有执行**；应先协调编译窗口。`entry` 用上节值，新路径必须不存在。

```bash
export GOMAXPROCS=0 GOWORK=off GOFLAGS=-p=2
rebuild_root="$(mktemp -d /private/tmp/v147-rebuild.XXXXXX)"
export ROOST_PERF_BINS="$rebuild_root/bin"
mkdir "$ROOST_PERF_BINS"
git -C "$ROOST_PERF_REPO" worktree add --detach "$rebuild_root/base" b4699758
git -C "$ROOST_PERF_REPO" worktree add --detach "$rebuild_root/head" c91b6d93
git -C "$ROOST_PERF_REPO" worktree add --detach "$rebuild_root/nobranches" c91b6d93
git -C "$rebuild_root/nobranches" apply --unidiff-zero "$entry/aoi-no-branches.patch"
(cd "$rebuild_root/base" && go test -c -o "$ROOST_PERF_BINS/aoi-base.test" ./framework/sync/entitysync/policy)
(cd "$rebuild_root/head" && go test -c -o "$ROOST_PERF_BINS/aoi-head.test" ./framework/sync/entitysync/policy)
(cd "$rebuild_root/nobranches" && go test -c -o "$ROOST_PERF_BINS/aoi-no-branches.test" ./framework/sync/entitysync/policy)
for group in 2 4; do
  for slice in 50 100; do
    tree="$rebuild_root/hb-g$group-s$slice"
    git -C "$ROOST_PERF_REPO" worktree add --detach "$tree" ee5ad2a4
    python3 - "$tree" "$group" "$slice" <<'PY'
from pathlib import Path
import sys
p=Path(sys.argv[1])/'framework/nest/try_batch.go'
s=p.read_text()
for before,after in [('const tryBatchGroup = 4','const tryBatchGroup = '+sys.argv[2]),('const tryBatchSlice = 100 * time.Microsecond','const tryBatchSlice = '+sys.argv[3]+' * time.Microsecond')]:
    assert s.count(before)==1
    s=s.replace(before,after)
p.write_text(s)
PY
    git -C "$tree" diff >"$ROOST_PERF_BINS/hb-g$group-s$slice.patch"
    (cd "$tree" && go test -c -o "$ROOST_PERF_BINS/hb-g$group-s$slice.test" ./gameplay/heartbeat/internal/bench)
  done
done
git -C "$ROOST_PERF_REPO" worktree add --detach "$rebuild_root/common" 357263ba
(cd "$rebuild_root/common" && go test -c -o "$ROOST_PERF_BINS/movement.test" ./gameplay/movement)
(cd "$rebuild_root/common" && go test -c -o "$ROOST_PERF_BINS/cellworld.test" ./gameplay/cellworld)
(cd "$rebuild_root/common" && go test -c -o "$ROOST_PERF_BINS/gateway.test" ./infra/network/gateway)
(cd "$rebuild_root/common" && go test -tags integration -c -o "$ROOST_PERF_BINS/gate.test" ./wiring/gate)
git -C "$ROOST_PERF_REPO" worktree add --detach "$rebuild_root/views16" 357263ba
git -C "$rebuild_root/views16" apply --unidiff-zero "$entry/sync-aoi-views16.patch"
(cd "$rebuild_root/views16" && go build -o "$ROOST_PERF_BINS/sync-aoi-views16" ./scripts/perf/sync-aoi)
git -C "$ROOST_PERF_REPO" worktree add --detach "$rebuild_root/mail" fef75aee
(cd "$rebuild_root/mail" && go test -tags integration -c -o "$ROOST_PERF_BINS/mail.test" ./service/mail)
shasum -a 256 "$ROOST_PERF_BINS"/*.test "$ROOST_PERF_BINS/sync-aoi-views16" >"$rebuild_root/binaries.sha256"
```

若选择从更新的最终集成树重编而不是重建历史基线，先验证相关源码差异并明确记录新来源；不得把不同实现当成仅参数变化。原准备时父任务已核对相关 Nest/Sync/Heartbeat/CellWorld/Movement/Gateway/Mail/Gate/NestGame 源码与当时集成树无差异；之后邮件 helper 的明确变化已单独记录，最终集成检查仍由下一位完成。

## 每项结果的边界

| 项 | 夹具边界 / 必须分开记录的量 |
| --- | --- |
| M1 | 内存注册 handler 的 Nest 心跳投递与 CPU 回调；不是 DAO/WAL/Mongo 吞吐。看实际投递/执行、冲突转单目标率、分配和尾延迟。 |
| M2、M5/E1、M9、M18 | Move/AOI/Relation/Group 微基准；不是客户端网络或持久业务容量。E1 按 base/head/no-branches 的10轮原始日志做 benchstat；去分支不能单独证明代码布局因果。 |
| M3、M6、M16 | 生成 DAO/Entity + Nest，分别 embedded TCP 或 Gate/NATS 接线；该 nest-game 夹具无真实 Mongo，不能因有 DAO 就称完整持久链。 |
| M4 | sync-business 生产生成接线的 periodic/on_change 比较；无 Mongo 依赖。 |
| M7 | 实际 TCP 健康/停读连接出站预算演练；报告拒绝、关闭、预算和健康连接结果，不只脚本 PASS。 |
| M8 | 16视野非生产常数的 Sync AOI 诊断构建，loopback TCP；P99、max、超50ms计数、exit及每 tick backlog分别保存。 |
| M10 | Gate慢链路追帧演练；可能预期断线仍 PASS，要读 deadline reason 和实际帧/字节/关闭原因。 |
| M11 | 真实联盟服务 / NATS / Mongo / Redis 路径；测量 Join 最多8次，logical成功、attempt总数与retryable/unknown分别报告，unknown不能盲重试。建盟准备重试不是Join测量attempt。 |
| B2 | 500成员是真实业务 / 权威文档与16镜像读缓存；1k/5k/10k是合成 BSON/DTO 形状，不经业务准入或存储网络，heap差不是RSS。 |
| M12 | 完整11服务、真实依赖、新工程 Robot。count 是完整场景数，不是msgTPS；分别列成功/失败/最终错误、exit、一致性断言、启动replay pending。Saga未知/等待与World sync timeout若只有日志，只计**日志出现次数**，不是事务数或排队事务数；保留Robot原始JSON，不以宽泛grep的0推断没有等待。 |
| M13 | 真实 Redis 单机/Cluster 账本2000x微基准；PASS说明断言通过，不等于全服邮件业务容量。 |
| M14 | CellWorld 游戏/演示尺度测量，记录夹具内CPU/扇出/缓存成本；不当作M15完整网络容量。 |
| M15 | 完整 CellWorld Robot 网络与服务链；原始exit、成功、超时、延迟与一致性分开，1000人数不是50ms达标结论。 |
| M17 | 生成客户端TCP→Nest→正式DAO/WAL/Mongo持久写，区分strict/pipelined/self_sync、开闭环；1500/s是提供速率，成功完成TPS和最终数据库一致性另计。 |
| E3 | `dropped`是客户端未发送的节拍；与服务器拒绝/超时/心跳拥塞分别列。五轮全部保留。 |
| M19 | 同一旧实验树开关A/B；RPC failed/slow可以只写日志而脚本PASS，必须逐档统计失败/≥3s时间簇及对应栈。 |

E1 的 benchstat 已安装于 `/Users/whb/go/bin/benchstat`；汇总时按实际完成的10轮分别提取三类日志，不因续跑而重复计算已完成格，保留 stdout 与样本数。没有独占机器或有外部负载时如实记条件，不用一次绿色宣布容量。

## B3 / M19 实验保留

实验工作树 `/Users/whb/roost/worktrees/v147-background-checkpoint`，分支 `experiment/v147-background-checkpoint`，HEAD **`300be3a0`**，原基线 `fe83c60a`。完整[实验报告副本](../checkpoint-experiment-20261114/README.md)随本交接保存，**只复制报告，不合实验代码**。源码丢失时可从保留提交建立新 detached worktree，再设置 `ROOST_CHECKPOINT_TREE`；引用不存在时先找回实验提交，不能静默换成发布树。

M19 两档都在该同一实验树执行，配置0/1各3轮，分别128/256并发。`STACKS=1` 在调用阶段至少1s无完成时采样，每阶段至多2次，500ms检查；两档一致开启，但实际触发的栈打印有额外开销，须注明。`storeCheckpoint` 栈份数是相关性材料，不能直接证明历史3s整体停顿根因。完整夹具在这个采样开关下的正式运行尚未进行。
