# 生成工程完整链路的 strict 写入吞吐（2026-10-20）

**结论**：新用户试用第八轮第 5 项报告的“生成工程整条链路写入约 125 TPS”在当前 main 上不能复现。同一类工程（CLI 生成，Player + 一个 strict handler + 嵌入 TCP 接入，生成的 wiring Mod 与生成配置）在本机 50 连接、各自一个玩家时为**压测端 1253 TPS**，WAL 每批约 9.9 条；只有所有请求落在**同一个实体**时才是一百多 TPS。试用报告里的数字与“全部请求落在同一实体（或被串行化）”的形态完全吻合，推断问题在试用者的夹具；夹具源码未入库，这一点尚未确认。

这是功能性定位的单样本，宿主机非独占（共享隔离环境同机，见 `env.txt` 的负载与 swap），只作数量级参考，不是容量基线。

## 1. 场景与口径

| 项 | 内容 |
| --- | --- |
| 工程 | `codegen/internal/roost` 的 saga_native 新手工程（`roost add access / entity / component / dao / handler / saga -native / effect`）再加 `roost add handler Tap`、`add protocol Tap`、`add endpoint Tap`、`add lifecycle Player`、`add transport tcp`、`config enable player-tcp`，生成后 build |
| handler | `//roost:nest rollback=undo durability=strict`，Player 金币 +1，**不经 Saga、不发 effect**；进程里装着 saga Mod，但 Tap 不经过它 |
| 进程 | 生成的 `main` + 生成配置（`dataengine.wal.group_commit_interval: 2ms`，`nest.short.workers: 0` 即 GOMAXPROCS = 10），只改连接地址、库名、流名、端口与流大小；鉴权换成“票据 = 玩家 ID”（`auth.go.txt`，仅本地压测） |
| 压测端 | `tapload`（`tapload-main.go.txt`）：N 条 TCP 连接各自闭环发 Tap、读回复，10 秒；吞吐与延迟按**压测端**计 |
| 依赖 | 共享隔离环境（Mongo 副本集、NATS、Redis），唯一库名 / 流名、用后删除，持有 `remote-acceptance.lock` |

## 2. 结果

| 场景 | 压测端吞吐 | P50 / P99 | WAL 条 / 批 | 平均持锁（服务端） |
| --- | --- | --- | --- | --- |
| 嵌入 TCP，50 连接各自一个玩家 | **1253 TPS** | 38.2 / 75.2 ms | 12561 / 1275 ≈ 9.9 | 7.9 ms（含所在批 fsync，平均 5.6 ms） |
| 嵌入 TCP，2 连接同一个玩家 | 108 TPS | 17.7 / 32.4 ms | 1078 / 1078 = 1 | 9.2 ms |
| 进程内（同工程的 wiring Mod，直接调生成的 syncsender，无 TCP），50 并发各自一个玩家，`group_commit_interval` 缺省 10ms | 894 TPS | 54.8 / 89.8 ms | ≈ 10 | — |
| 同上，`group_commit_interval: 2ms` | 1257 TPS | 39.0 / 59.4 ms | ≈ 10 | — |
| 同上，handler 改 `durability=async` | 1083 TPS | 47.0 / 66.9 ms | ≈ 11.6 | — |

原始输出：`benchmarks/r8-generated-tcp-strict-20261020/`（`tcp50.txt`、`tcpsame2.txt`、`inprocess-*.txt`、`env.txt`）。

## 3. 解读

- **吞吐上限 ≈ short 池 worker 数 / 每笔持锁时间**：strict 在实体锁内等所在批的 WAL fsync（[NEST_TRANSACTION_WAL](../framework/NEST_TRANSACTION_WAL.md)），持锁约 8 ms，short 池 10 个 worker，所以约 1250 TPS；50 连接时 P50 ≈ 50 / 1250 s = 40 ms，是排队。要更高的单进程 strict 吞吐，看 pipelined（提前放锁）或更多 worker，不在本次范围。
- **同一实体串行**：同一实体的事务按锁串行，吞吐 ≈ 1 / 持锁时间 ≈ 108 TPS，WAL 每批 1 条。
- **试用报告的形态**：50 连接 115 TPS、WAL 每批 1 条、持锁 8.6 ms、P50 430 ms ≈ 50 × 8.6 ms，与“50 个连接的请求落在同一实体或被串行化”一致，与 short 池 10 个 worker 并行提交不一致。框架夹具（`scripts/perf/dataengine.sh` single strict，32 writer × 128 实体，进程内调用方计）报告的 857 TPS 与本次进程内 894 TPS（缺省 group commit）同量级。
- **async 不比 strict 快**（1083 对 1257）：原因未查，尚未确认。async 的契约是“不等本条 fsync”，不承诺负载下更快；WAL 只有一个 writer，写盘与 fsync 串行，推测 async 的追加仍要等前一批 fsync 结束，未逐次计时证实。

## 4. 复跑

**2026-10-22 已在 v1.31.0 发版候选上复跑**（[NIGHT-20261022 §13](NIGHT-20261022.md#13-第-14-项生成工程完整链路持久写入嵌入-tcp--strict)）：strict 50 连接 1241～1314 TPS（4 次）、同一实体 113 TPS，与本页一致；另做了 pipelined / async 对照，self_sync 对照未做。命令用的就是下面的步骤（第 2 步的具体命令写在那一节）。

1. 在本仓 `codegen/internal/roost` 放入 `prepare_test.go.txt`（改名为 `_test.go`），`R8_OUT=<目录> GOWORK=off go test -run '^TestR8PrepareTapProject$' ./codegen/internal/roost/` 生成工程（指向本检出）。
2. 在生成工程里按 §1 依次运行 `roost add protocol / endpoint / lifecycle / transport tcp`、`config enable player-tcp`，替换 `internal/access/player/tcp/auth.go`，放入 `cmd/tapload/main.go`，`roost generate`，build 出 game 与 tapload。
3. `run_tcp.sh.txt`（嵌入 TCP）与 `run_tap.sh.txt` + `tap_perf_test.go.txt`（进程内）在持锁、source 隔离环境后运行；参数见脚本头。
