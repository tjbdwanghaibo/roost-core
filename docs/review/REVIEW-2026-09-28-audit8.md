# 2026-09-28 第八轮独立审计（本地 main `d66eeeb`）

范围：RR-20260928-07～11，外加 `462c8d7`（DEPLOYMENT k8s 章节）。非修复方审计员在导出副本上核验（各修复父提交为修前）。图谱已过期，结论来自源码与实跑；未连外部服务、未跑 docker。

## 修复核验结论

5 条修复“修前红、修后绿”全部成立，约束符合。RR-11：`requireSync >= Strict` 只作用于 {2, 3}（更大值在 `canonicalizeRecord` 已被拒），`WAL.Append` 非测试调用方 3 处、Durability 3 进入只有 broadcast 与带 Remote 批次两条，未找到把不该等 fsync 的记录变成等 fsync 的路径，快路径 `Enqueue` 不变；
同机 A/B 复跑方向一致（broadcast pipelined 吞吐下降、strict 与快路径无显著变化）。RR-09：16 处 Durability 判定清单完整。RR-08：Durability 0 路径未变，拒绝范围表与源码一致。RR-10：临时根演练覆盖无记录 release、连续回滚、从 legacy 升级失败自动回滚，均按设计。
RR-07：新生成 game-demo 10 个服务 Secret 内嵌 config 与 prod 示例逐字节相同，手改 Secret 保留。USER_GUIDE §4 判别表 1～15 连续无重复，`replySentinels` 逐行对齐。`462c8d7` 与生成的 README / deploy.sh 一致。

门禁（审计员）：build / vet（含 integration tag）/ glsvet / gofmt，整仓非 race 120 包，nestwal / nest / remoteentity / dataengine / kit 相关包 `-race -count=20`，新回归 `-race -count=50`，codegen 定向回归，均通过。

## 新发现

- [RR-20260928-12](../bug/RR-20260928-12.md)（P4 推断，RR-10 引入）：回滚时新版本进程按旧 unit 的 TimeoutStopSec 被停。
- [RR-20260928-13](../bug/RR-20260928-13.md)（P4）：CRLF 的 Secret 示例被静默跳过；Secret 不可解析时 `add transport tcp` 与 `add mod` 处理不一致。

## 疑点（文档，随 RR-12 / 13 一并处理）

- D1：判别表第 4 行“strict / async 已持久”——自带 remoteentity 在 async 下 `Commit` 返回推测回执、不走第 4 行，async 回复时本地也未 fsync；RR-11 后带 Remote 批次的 pipelined 已持久却未列出；第 2 行“四种形态”未列 RR-08 的 Overloaded 形态（表后已写）。
- D3：`docs/DEPLOYMENT.md` §4 与生成的 `deploy/shell/README.md` 仍写“45 秒 SIGTERM 预算”（TimeoutStopSec 按 Mod 生成）。
- D4：rollback.sh 版本号校验接受 `.` / `..`（修前即有，readiness 失败后恢复原版本，基本无害）。
- D5：roost-coding 快池豁免清单只点名“strict 锁内 fsync”，建议补“以及回退到 strict 路径的 pipelined”。
