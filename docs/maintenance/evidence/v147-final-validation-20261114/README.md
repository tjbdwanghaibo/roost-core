# v1.47 功能验收与交接状态（2026-11-14）

模块、玩法与真实组的代码基线为 `f62ceec7e2036c3971299177bafa10cca64d894f`，执行 HEAD `48ed0113ac17897b8bb1eebd8821c75b10813f3d`（差异仅为 E2 报告）。后续生成检查在 `fef75aee6d61a8986444de3980cc7db0c24a3765` 完成；shutdown 测试修正合入后的最终 Go 单包复验基线为 `aff286349fb1bca215e8aa761665826d03628389`，用户要求交给后续 agent 后，root 已于 `2026-11-14T05:12:46.443237+00:00` 主动中止该轮。平台为 macOS arm64 / Go 1.27.3，统一 `GOMAXPROCS=0 GOWORK=off GOFLAGS=-p=2`。各功能组允许并行；耗时与小规模 Robot 延迟只作验收诊断，不能用于性能、容量或长稳结论。没有 push 或发布。

**交接状态：模块、玩法、真实依赖功能组及生成漂移检查已完成；修正旧断言后的 60 分钟 Go 单包复验未完成，结论为 USER HANDOFF INTERRUPTED（用户要求交接，root 主动中止），不当作实现失败或整包通过。本轮未启动正式性能测量，不宣称整轮验收通过。初轮和 30 分钟复验都失败，不能记成最初全套退出 0。** 本页将四组结果合并记录；便于机器读取的精简计数及输入 JSON 摘要见 [summary.json](summary.json)。

## 完成状态与计数口径

| 验收项 | 实际结果 | 解释 |
| --- | --- | --- |
| 根 `go test ./...` 初轮 | exit 1；165 包 ok、25 包无测试；仅 `codegen/internal/roost` 失败 | 默认包级 10 分钟总时限，600.678s；没有把这一轮写成全绿 |
| `codegen/internal/roost` 30 分钟复验 | exit 1；6 条真实断言失败，随后整包超时 | 不是只有超时；原始失败和栈保留 |
| shutdown / singleton 定向修复 | 31 顶层 PASS、0 FAIL/SKIP；漏计 Mod 变异红，还原绿 | 初次定向又暴露第 7 个同源旧预期；仅改测试/证据，保留独立固定预算 |
| `codegen/internal/roost` 最终单包复验 | **USER HANDOFF INTERRUPTED；未完成** | `aff28634`，766.372s 后按交接要求 SIGTERM；已观察 123 顶层 PASS、0 条失败，不能计整包通过；其余 165 包未重跑 |
| `go generate ./...` 与源码漂移 | exit 0；diff exit 0、status 空、HEAD 前后相同 | 在 `fef75aee` 完成；后续 shutdown 合并只改测试/证据，无生产生成改动 |
| 完整模块安装矩阵 | exit 0；5 个消费者、11 份 verify JSON 全部 ok | 全安装为 42 模块 + 47 联动；正序和逆序各 89 项、1027 条场景、0 SKIP、4 项工程检查通过 |
| 13 玩法默认矩阵 | 13/13 成功；默认 182 次顶层 PASS，自定义 module 复验 182 次 PASS；0 SKIP | 默认主玩法 180 + SLG 联盟附加 2；13 份 PB registry 存在，build/vet/ID/生成漂移检查均完成 |
| 13 玩法 REAL 矩阵 | 13/13 脚本成功；208 次顶层 PASS，0 FAIL/SKIP | 208 = 脚本重跑的默认 182 + REAL 额外阶段执行 26；不是 208 个不同真实恢复用例 |
| demo 真实档 | 14 顶层 PASS，0 FAIL/SKIP | 包含 mailbox、Terrain WAL SIGKILL、生成 Gate、NativeSaga/effect 等 |
| remote 默认/race 档 | 18 顶层 PASS，0 FAIL；**3 个专用入口 SKIP** | 跳过项列在下节，不计通过，也不替代 mirror-local 独立故障矩阵 |
| alliance 功能档（PERF=0） | 11 PASS，0 SKIP | 包含 SocialTeam、SocialMatchmaking、SocialRehome |
| seamless 默认档 | 15 STEP 全过；普通 sid 1003 Robot 2/2 成功 | 0 failure/drop/cancel；原 p95 16s 功能门槛通过，2 个样本不是容量证据 |
| seamless TERRAIN=1 | 7 STEP 全过 | 跨格编辑、地形移动、Cell 恢复、块一致性、建筑 create/remove |

模块矩阵覆盖依赖安装、增量安装、重复安装无变化、自定义 module、正序/逆序全安装及脚本内整工程 `go test`。11 轮 verify 的 PASS 分别为 `25, 17, 25, 35, 66, 191, 213, 238, 477, 1027, 1027`；合计 3341 是**跨重复阶段的执行次数**，不是独立场景数。每份 verify 均有且仅有四项工程检查：build、vet、ID、`generate --check`。foundry/citadel 两份全安装工程的计数完全一致，重复安装与整工程测试后的工作树检查均干净。模块场景仍使用离线提交替身，不与后面的真实依赖组混称。

Remote 的三个跳过分别是 `TestGeneratedRemoteMirrorLocal`（要求独立 `scripts/mirror-local.sh test` 入口，本轮没有额外启动该故障矩阵）、`TestGeneratedRemoteMirrorLocalProcess`（前者的子进程入口）、`TestGeneratedRemoteMirrorReaderProcess`（`TestGeneratedRemoteMirrorGuildSummary` 的子进程入口）。原始日志逐项保存了跳过理由。

## 13 玩法的实际执行范围

| 玩法 | 默认顶层 PASS（含附加） | 自定义 module PASS | REAL 额外阶段 PASS | REAL 脚本全部 PASS |
| --- | ---: | ---: | ---: | ---: |
| slg | 9（主7 + 联盟2） | 9 | 4 | 13 |
| moba | 28 | 28 | 6 | 34 |
| mmorpg | 14 | 14 | 3 | 17 |
| card-rpg | 17 | 17 | 1 | 18 |
| tower-defense | 11 | 11 | 1 | 12 |
| survival-extraction | 13 | 13 | 1 | 14 |
| merge-casual | 16 | 16 | 1 | 17 |
| idle-sim | 13 | 13 | 1 | 14 |
| auto-chess | 13 | 13 | 1 | 14 |
| party-room | 19 | 19 | 1 | 20 |
| turn-pvp | 10 | 10 | 4 | 14 |
| coop-roguelike | 10 | 10 | 1 | 11 |
| social-sim | 9 | 9 | 1 | 10 |
| 合计 | 182 | 182 | 26 | 208 |

26 次 REAL 阶段执行对应按玩法计的 22 个不同顶层测试名。MOBA 结算、MMORPG 正常恢复、turn timer/recipe 有分阶段同名执行；合作/经营各一个父用例含两个崩溃子场景，子场景没有重复计入顶层 PASS。`-run '^$'` 的仅编译输出不计 PASS。

- SLG 的四项 REAL 是活动、Player 到账、Remote 权威交接、RPC 接收期限；MOBA 使用固定夹具身份，包含恢复/结算及 ROUTING；MMORPG 是正常持久化 write/recover 与 SIGKILL 恢复，本轮没有扩跑跨 sid 专项。
- card-rpg、tower-defense、survival-extraction、merge-casual、idle-sim、auto-chess 分别验证其已接入的真实 Mongo/文件 WAL 崩溃恢复、期限或离线推进及奖励重试；各自脚本限定范围内的第三进程核对按原用例执行，不推广成所有玩法均含同样步骤。
- party-room 验证真实 Mongo/WAL 局内 SIGKILL、心跳推进、结算/领奖/删档与第三进程；本轮不是独立 Gate 组同步联合验收。
- turn-pvp 的 GameRoom 持久定时器 seed/recover 与独立配方的真实账号/匹配/Room/Turn/Gate/NATS/Mongo/WAL seed/recover 是两套分别通过的模型。
- coop-roguelike 的 partial_vote/first_reward、social-sim 的 growing/harvested 是 Sender→真实存储的 WAL-only SIGKILL 子场景，不含 TCP、真实登录或 Gate。

默认档的 TCP/PB 或 KCP Robot 验证使用替身存储或夹具鉴权；**不能把默认网络档与独立 REAL 存储档拼成全部玩法都通过了真实账号/Gate/存储联合链路**。turn-pvp 独立配方的真实联合链单列，不推广给其余玩法。测试驱动使用各脚本规定的 race；普通 `go build` 构建的服务子进程不因此被称为带 race。本轮没有 Linux、跨机 HA、断电、真实客户端引擎设备、容量或小时级长稳结论。

已有开发批的精确故障屏障、变异及边界仍见 [地形 WAL 崩溃](../terrain-wal-crash-20261114/README.md)、[Room/Turn 两套真实链路](../turn-real-20261114/README.md)、[合作/经营恢复](../session-real-20261114/README.md)、[GM/purchase/activity](../operations-real-20261114/README.md)。链接保留这些先前批次证据，不把所有专项或变异都算作本轮再次执行。P8 的非 WAL-only 窗口、完整 demo 的 64 字段组合上限、rehome Q4 等限制不会因本页的矩阵通过而消失。

## 原失败及复验

根 Go 初轮使用 `GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0 go test ./...`，原始退出码是 1。超时栈显示当时在运行 `TestModuleEquipmentInstallsIndependentlyAndAddsAttributeLater (19s)`，包累计默认 10 分钟预算到限；不是该场景执行了 10 分钟。保留原日志和 165 包 ok / 25 无测试的统计，只重跑生成器 CLI 包。

第二轮实际命令为 `GOWORK=off GOFLAGS=-p=2 GOMAXPROCS=0 go test -count=1 -timeout 30m ./codegen/internal/roost`，在整包 30 分钟到限前已有 **6 条断言失败**：新增 Room timer 后实际 28 Mods，旧测试仍固定 27 / 123s / 128s；singleton 的 116s 已不足以区分开关，手改 129s 的旧字符串替换也静默失效。定向组又发现移除 session 的第 7 个同源旧预期。维护 README 的 6–14 分钟旧估计导致本次误选 30 分钟；现行 `scripts/pretag.sh` 已采用 60 分钟，记录的 42 模块整包曾需 2024–2151 秒，不能推荐 30 分钟作为完整预算。

修复提交 `21c7183f865deb04ab2c88c3ab7c18a2b20b6223` 已合入 `aff28634`。生产 Mod 与预算算法未改，测试继续独立枚举 bootstrap 并固定核对真实 Room driver；显式夹具是 28 Mods / 126s / 131s，默认 CLI 多 manager，是 29 Mods / 129s / 134s。31 项定向（shutdown 25 + singleton 6）全部通过，103.834s；预算故意漏计一个 game Mod 的负对照以配置/部署断言失败，35.587s；按原字节还原生产文件后同一用例通过，34.904s。原六条失败、第七条定向红、变异红与复绿均保留在[修复证据](../shutdown-assertions-20261114/README.md)，没有降低业务门槛或改场景数量来通过。

最终单包于 `2026-11-14T05:00:00.070340+00:00` 在 `aff286349fb1bca215e8aa761665826d03628389` 开始，以 `-count=1 -timeout 60m -v` 执行。**用户在该轮运行期间要求交给另一个 agent，root 于 `2026-11-14T05:12:46.443237+00:00` 按交接要求主动中止。结论是 USER HANDOFF INTERRUPTED，该轮未完成。** wrapper 原样记录 `status=failed`、子进程 `exit=-15`，外层 shell 返回 241；这对应主动 SIGTERM，不当作实现断言失败。运行 766.372s，停止记录观察到 123 个顶层 PASS、`observed_failures=[]`，当时正在进入 `TestModuleMatchroomGenerated`；不把这些已输出的用例或之前定向通过当作整包通过。

停止元数据保留了 PGID 19153 的现场进程和精确 SIGTERM 列表，最终 `group_after=[]`；本轮 Go 进程组没有残留。后续接手者应先读主交接入口，在代码与资源状态明确后重新执行该包完整验收，保留本次中止日志。正式性能本轮取消启动，不能用功能窗口的耗时填充性能结论。

`go generate ./...` 已在 `fef75aee6d61a8986444de3980cc7db0c24a3765` 退出 0；独立 `git diff --exit-code` 为 0，`git status --porcelain` 为空，HEAD 前后完全相同。核对时间为 `2026-11-14T04:51:21Z`，原日志与 JSON 元数据保留；该结果与稍后的测试修复分别记录。

13 玩法默认矩阵首次在沙箱中执行，13 类均因写 Go module cache 遇到 `operation not permitted` 而停在生成期，退出 1，未进入业务场景。原目录保留；获准使用 `require_escalated` 后，在独立 retry1 目录完整重跑并通过。没有删除缓存或改代码绕过。这是执行权限失败，不记作实现负对照，也不抹去首轮失败。

## 原始证据与保留范围

以下是本机原始证据路径，以代码文字记录；不把临时绝对路径作为仓内规范链接。入库的精简事实与输入 JSON SHA-256 在 [summary.json](summary.json)，原始日志没有被精简 JSON 替代。

| 组 | 原始证据位置 | 实际保留 |
| --- | --- | --- |
| 根 Go / 生成 | `/private/tmp/v147-final-go-test-20261114.log`、`/private/tmp/v147-final-go-summary.json`、`/private/tmp/v147-final-roost-retry-20261114.log`；最终 `/private/tmp/v147-final-roost-60m-20261114.{log,json}`，停止记录 `/private/tmp/v147-final-roost-handoff-stop-20261114.json`；生成 `/private/tmp/v147-final-go-generate-20261114.{log,json}` | 两轮失败保留；最终 60 分钟轮按交接要求中止，原始 wrapper 退出和单独停止原因均保留；生成及独立漂移检查通过。定向原始目录 `/private/tmp/v147-shutdown-assertions-20261114/` 与入库修复证据均保留 |
| 模块矩阵 | `/private/tmp/v147-final-modules-20261114.log` 与 `/private/tmp/v147-final-modules-20261114/` | 5 个完整消费者、11 份 verify JSON/stderr、安装/计划/整工程日志、summary；约80MiB，未清理 |
| 玩法默认 | `/private/tmp/v147-final-genres-20261114/` 与对应 `.console.log`；成功档 `/private/tmp/v147-final-genres-20261114-retry1/` 与对应 `.console.log` | 首轮失败和 retry1 原目录均在；含 report.json、生成工程、matrix/test 日志、自定义 module JSONL、工具 |
| 玩法 REAL | `/private/tmp/v147-final-genres-real-20261114/` 与对应 `.console.log`；调度器 `/private/tmp/v147-final-genres-real-run.sh`、`/private/tmp/v147-final-genres-real-run.py` | 13 玩法目录、分阶段日志、turn recipe seed/recover、report/端口/清理 JSON；offset24000 依赖执行 `down` 后保留目录 `/private/tmp/v147-final-genres-deps-24000-20261114/roost-dataengine-it` |
| demo/remote/alliance/seamless 真实组 | `/private/tmp/v147-final-real-20261114/` | run.sh、environment/stages/result、完整 bash-x 日志、alliance 日志、seamless 原始 evidence.json/服务日志、外存 dependency-logs、up/clean 日志、summary 和清理核对 |

真实组的外层调度实际执行了 `mirror clean`，删除了**该组新建的 offset18500 私有镜像数据库根**；共享/业务数据未动，依赖日志在清理前已复制到外部证据目录。标准脚本也按原逻辑删除本组临时生成工程。因此该组不能再回查原数据库文件，不能写成“所有原始 DB/WAL 均保留”。

玩法 REAL 的 offset24000 用 `down` 停机而未删除依赖目录；但标准夹具成功后会清理各自唯一测试库/流/键，保留目录不意味着每条测试业务记录或 WAL 仍可完整回查。两组依赖和应用均已停止；真实组清理核对记录其私有根已不存在、预留端口无监听，玩法组核对62个预留端口无监听。Go 缓存没有清理。

## 可复跑入口

在匹配上述代码版本的检出中执行，使用新的绝对输出目录，避免覆盖已留存证据。运行环境需要 Go 缓存写权限；同类沙箱错误应保留原失败并切换到获准的可写环境，不删除缓存。以下是功能命令，不含容量档。

```sh
export GOMAXPROCS=0 GOWORK=off GOFLAGS=-p=2
go test -timeout 60m ./... > /private/tmp/v147-go-rerun.log 2>&1
# 本次其他包已通过；以下单包在交接时被中止，后续需重新完整执行：
go test -count=1 -timeout 60m -v ./codegen/internal/roost > /private/tmp/v147-roost-rerun.log 2>&1
go generate ./... > /private/tmp/v147-generate-rerun.log 2>&1
git diff --exit-code
git status --porcelain
ROOST_MODULES_REPO_ROOT="$PWD" ROOST_MODULES_OUTPUT=/private/tmp/v147-modules-rerun bash scripts/test-modules-generated.sh
python3 scripts/test-genres-generated.py --out /private/tmp/v147-genres-rerun
```

真实依赖先选独占目录与合法空闲端口；`up → source env.sh → 脚本 → down` 保持在同一 shell 生命周期，并在退出时停掉自己的进程。本次玩法组 offset24000、turn 业务 shift35000、etcd57779/57780；另一真实组 offset18500、alliance base46100/etcd58479、seamless etcd58379及其脚本固定业务端口。原计划offset18000会与另一组端口冲突，已在启动前改为18500。完整预检和环境见两份保留的外层调度器。

```sh
set -euo pipefail
export ROOST_MIRROR_LOCAL_HOME=/private/tmp/v147-genres-deps-rerun
export ROOST_MIRROR_LOCAL_OFFSET=24000 ROOST_IT_MONGO_CACHE_GB=0.25
genre_out=/private/tmp/v147-genres-real-rerun
for genre in slg moba mmorpg card-rpg tower-defense survival-extraction merge-casual idle-sim auto-chess party-room turn-pvp coop-roguelike social-sim; do
  mkdir -p "$genre_out/$genre"
done
trap 'bash scripts/mirror-local.sh down' EXIT
bash scripts/mirror-local.sh up
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
unset ROOST_GENRE_MODULE
export ROOST_MOBA_ROUTING=0
export ROOST_GENRE_KEEP=1 ROOST_TURN_PVP_PORT_SHIFT=35000 ROOST_TURN_PVP_ETCD_PORT=57779
ROOST_GENRE_OUTPUT="$genre_out/slg" ROOST_SLG_REAL=1 bash scripts/test-slg-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/moba" ROOST_MOBA_REAL=1 ROOST_MOBA_ROUTING=1 bash scripts/test-moba-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/mmorpg" ROOST_MMORPG_REAL=1 bash scripts/test-mmorpg-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/card-rpg" ROOST_CARD_RPG_REAL=1 bash scripts/test-card-rpg-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/tower-defense" ROOST_TOWER_DEFENSE_REAL=1 bash scripts/test-tower-defense-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/survival-extraction" ROOST_SURVIVAL_EXTRACTION_REAL=1 bash scripts/test-survival-extraction-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/merge-casual" ROOST_MERGE_CASUAL_REAL=1 bash scripts/test-merge-casual-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/idle-sim" ROOST_IDLE_SIM_REAL=1 bash scripts/test-idle-sim-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/auto-chess" ROOST_AUTO_CHESS_REAL=1 bash scripts/test-auto-chess-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/party-room" ROOST_PARTY_ROOM_REAL=1 bash scripts/test-party-room-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/turn-pvp" ROOST_TURN_PVP_OUT="$genre_out/turn-pvp/recipe" ROOST_TURN_PVP_REAL=1 bash scripts/test-turn-pvp-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/coop-roguelike" ROOST_COOP_ROGUELIKE_REAL=1 bash scripts/test-coop-roguelike-generated.sh
ROOST_GENRE_OUTPUT="$genre_out/social-sim" ROOST_SOCIAL_SIM_REAL=1 bash scripts/test-social-sim-generated.sh
```

另一真实组在**新的 shell**及独立私有依赖环境内依次执行以下入口；先做同样的端口预检。本次的完整环境与历史 `clean` 行为以保留的 `run.sh` 为准。以下复跑示例用 `down` 保留目录，不复用本次已删除的18500私有根。

```sh
set -euo pipefail
export GOMAXPROCS=0 GOWORK=off GOFLAGS=-p=2
export ROOST_MIRROR_LOCAL_HOME=/private/tmp/v147-real-deps-rerun ROOST_MIRROR_LOCAL_OFFSET=18500
real_out=/private/tmp/v147-real-rerun
mkdir -p "$real_out"
trap 'bash scripts/mirror-local.sh down' EXIT
bash scripts/mirror-local.sh up
source "$ROOST_MIRROR_LOCAL_HOME/roost-dataengine-it/env.sh"
unset ROOST_REMOTE_FAULT ROOST_REMOTE_RUN ROOST_REMOTE_TRACE ROOST_REMOTE_CPU_PROFILE ROOST_REMOTE_TIMEOUT ROOST_REMOTE_COUNT
unset ALLIANCE_E2E_RUN ALLIANCE_E2E_EXPECT SOCIAL_REAL_MUTATION ROOST_SEAMLESS_PROJECT_HOOK
export ROOST_REMOTE_LOAD=0 ALLIANCE_E2E_PERF=0 SOCIAL_REAL_MODULE_MATRIX=0
export ALLIANCE_E2E_PORT_BASE=46100 ALLIANCE_E2E_ETCD_PORT=58479
export ALLIANCE_E2E_LOGS="$real_out/alliance-logs"
export ROOST_SEAMLESS_ETCD_PORT=58379 ROOST_SEAMLESS_ROBOT_ONLY=0 ROOST_SEAMLESS_CAPACITY=0 ROOST_SEAMLESS_ROBOTS=0
bash scripts/test-demo-generated.sh
bash scripts/test-remote-generated.sh
bash scripts/test-alliance-e2e.sh
ROOST_SEAMLESS_OUT="$real_out/seamless-default" ROOST_SEAMLESS_TERRAIN=0 bash scripts/test-seamless-demo.sh
ROOST_SEAMLESS_OUT="$real_out/seamless-terrain" ROOST_SEAMLESS_TERRAIN=1 bash scripts/test-seamless-demo.sh
```

统计仅依据本轮实际 JSON、日志和脚本退出状态；索引证据沿用 Verify/Tier2 项目 `Users-whb-roost-worktrees-v147`、generation `2026-11-14T02:55:37Z`。旧代际未覆盖的新夹具/脚本以当前源码补读，没有为了本报告重新索引或运行任何验收。本报告不修改 KNOWN-LIMITS、能力规范或未回答的设计决定。
