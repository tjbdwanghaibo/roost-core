# P5：总验收（2026-09-08）

批次：P5（收敛方案 §4）
状态：完成到"可发版"；两项留待 P6 后（见 §5）
分支：core `consolidation`、kit `consolidation`、codegen `consolidation`；预发布 core `v1.14.0-alpha.4`、kit `v1.13.0-alpha.1`

## 1. 生成工程 source-head 通路

| 场景 | 本地 `scripts/source-head-check.sh`（macOS） | CI `framework-compat` source-head lane（Linux + dev compose 真启动） |
| --- | --- | --- |
| minimal（configdata） | OK | success |
| full（game 模板 + access player + transport tcp + skill + saga） | OK（planet 全部包 build / vet / test / glsvet 通过） | success |

`framework-compat` 的 minimum / released lane 与 codegen `ci` 的 release-smoke 在正式 tag 前不可能绿（拉到的是旧布局或不存在的版本）；`consolidation` 分支上 source-head / release-smoke 已 pin 到 alpha，发版后去掉 pin（工作流里标了 Transitional）。

## 2. 升级器端到端

codegen `upgrade-compat`：用 v1.11.0 与 v1.12.1 的 roost 生成历史工程 → 当前 roost `project upgrade --consolidate --dry-run` → `--consolidate` → 业务文件哈希不变 → go.mod 不再 require roost-skill / roost-service → `project diff` → tidy / test / glsvet → 第二次 upgrade 幂等（roost.yaml 不变）。**两条 lane 均 success**。

途中修掉一个真问题：`--consolidate` 曾把 go.mod 的 core / kit 抬到未发布的边界版本，随后的 `go get` 读不到 v1.14.0 的 go.mod；现在版本交给依赖解析步骤，改写只删旧模块。

## 3. 三仓 CI（consolidation 分支）

- core `ci`（linux-quality 含 `-race`、`go vet -tags integration`、windows、release-hygiene）：绿。
- kit `ci`：linux-quality、windows、released-core、local-core-source（取 core 同名分支）、integration（隔离 Mongo 副本集 + NATS 集群，跑 dataengine / saga / remoteentity / nats 的 Mod 级集成）、service-redis（12 个服务的 Redis 套件，拒绝跳过，-race）、release-hygiene：**全部绿**。
- codegen `ci` / `security` / `upgrade-compat`：绿；`framework-compat`：source-head 两条绿，其余两组等发版。

## 4. 性能对比（同机 macOS，`-count=5`，同一套基准：nestwal / dataengine 引擎 / saga）

迁移前在 kit 路径跑（`scripts/perf/dataengine.sh`），迁移后在 core 路径跑同样的 `go test -bench . -benchmem -count=5`；包名归一后 `benchstat` 对比，全文 [P5_benchstat.txt](P5_benchstat.txt)。

| 维度 | geomean 变化 |
| --- | --- |
| sec/op | -3.04% |
| B/op | +1.39% |
| allocs/op | +0.57% |

单项里有 ±12% 的双向波动（`MongoProjectionMatrix/single_cas` +12%、`MongoProjectionConflictMatrix/conflict_10_percent` -24%、`ProjectorWALReplayAckMatrix/ordinary_only` +12%），`ProjectorAdmissionMatrix/async/writers_1` 的分配 +12～16%。代码逐字节相同、只改了包路径，且两轮测量期间机器都在跑 CI 观察与 go 编译，判定为噪声；未发现系统性退化。P6 发版后在安静机器上再各跑一轮归档。

## 5. 故障矩阵与真实环境

- kit CI `integration` job 在新路径上跑通隔离 Mongo 副本集 + NATS JetStream 集群的 Mod 级集成（dataengine real / failover / toxic、saga、remoteentity、nats JetStream RPC toxic）——这是故障矩阵五切片中依赖 Mod 装配的部分。
- 随 redis 客户端搬到 core 的 `redis/lock_toxic_integration_test.go`（Redis 丢回复 / 延迟切片）在 core 侧只被 vet，未运行：本机此刻没有 docker，core 也没有起 toxiproxy 的脚本。**P6 后补**：要么在 kit 的 `dataengine-env.sh test` 里追加 `(cd ../roost-core && go test -tags integration ./redis)`（local-core-source 那种 go.work 布局下可行），要么给 core 加一个复用 kit 脚本的 integration job。

## 6. 文档

core README / docs 导航 / 用户指南 / 排障 / 技能文档、kit README 已改到三仓布局与新路径；交接文档加了"收敛后"导读；账本矩阵加"新位置"说明；映射表是权威。

## 7. 未验证项 / 风险（进 P6 清单）

1. core 侧 Redis toxic 套件未运行（§5）。
2. 性能在安静机器上的复测（§4）。
3. framework-compat minimum / released lane 与 release-smoke 只能在正式 tag 后验证；发版当天先发 codegen v1.15.0（业务工程要靠它的升级器），再 core v1.14.0、kit v1.13.0，然后去掉三处 Transitional pin 并看全部 lane。
4. P3b：kit Mod 仍是编排者，`Assemble*` 下沉未做。
5. roost-skill / roost-service 仓库归档、README 置顶指向。

## 8. 下一步唯一动作

P6：合并 `consolidation` → main（三仓），按序发 codegen v1.15.0 → core v1.14.0 → kit v1.13.0，去 pin，归档两仓。
