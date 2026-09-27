# 2026-09-27 第三轮修复后独立审计（本地 main `736bdb2`）

范围：RR-20260926-64～72 修复合并后，两名非修复方审计员在导出副本上独立核验修前红（基线 `2373e21` 或各修复提交的父提交）、修后绿、约束符合度与跨组交互。
复现附录：[REPRO-2026-09-26-07](../bug/REPRO-2026-09-26-07.md)。图谱索引对相关文件已过期（metadata_changed，09-26 08:36），结论全部来自 `736bdb2` 源码。

## 修复核验结论

| RR | 结论 | 备注 |
| --- | --- | --- |
| 64 | 成立 | 补验带 Remote 批次的 memory 冲突：执行 1 次、只带冲突哨兵、批次 Abort |
| 65 | 成立 | Remote 消息分支 → RR-75；判别指引 → RR-77 |
| 66（含复核） | 成立 | 升级路径（旧 60s 工程 / 手改 / 缺键）实测正确；doctor WARN 口径、减 Mod 不一次收敛 → RR-80 |
| 67 | 成立 | 疑点：`ReleaseCast` 按 ID 释放 |
| 68 | 成立 | RR-52 四条在修前修后均绿 |
| 69 | 成立 | REPRO §5 并发探针修前 3000 轮丢 17 次、修后 0 次 |
| 70 | 大体成立 | 二次撤销丢订阅 → RR-78；Direct 绑定表不清理 → RR-79 |
| 71 | 成立 | 启动期 panic 在仓内与生成工程均不触发 |
| 72 | 成立 | 三方并发探针 3×2000 轮 done 恰好一次 |

合并门禁（审计员执行）：race 52 包、整仓非 race 120 包、sync 三组 `-race -count=3`、sync-modes、game-demo 生成 build/vet/test/race/glsvet、doctor；
本地隔离集成环境（`zz4a_*` 库 / `ZZ4A_*` 流 / redis db 11，跑完已清理）机器人 3 轮 6/6、真实停机 23 个 Mod 均拿到 ≥3s、无 `mod stop budget` 告警。未跑故障矩阵与长压测。

## 新登记（均未修复，均为修前即有或 RR-66/70 引入的 P4）

RR-20260926-73（P3，生产可达）、74～75（P3 潜在）、76（P3）、77～80（P4）。维护者 2026-09-27 拍板：RR-74、RR-75 均“拒绝”（返回可 `errors.Is` 的错误，
不改外层快照 / 不引入嵌套独立批次）；RR-73 沿用 RR-64 的“返回错误，不重排”。

## 疑点（未登记）

- 嵌套派发 try-lock 分支（`nest/nest_dispatch.go:399` `GuardedCount()>0`）在 handler 内是否可达：公开 Dispatch / Request 在 handler 内一律被拒，未找到入口。
- `skill/combatcomponent` 的 `RunDetachedTransaction` 是否在 Nest handler 内调用（RR-76 的影响面）。
- RR-67：`ReleaseCast` / `ReleaseEntity` 仍按 ID 释放；Destroy 并重建后按旧指针调用会释放新实例（理论角落，无探针）。
- RR-68：调用方截止短于 `write_timeout`（5s）的推送永不关闭停读连接，只收这类推送的连接靠读侧 90s idle 发现；当前 sync 推送不带截止，RR-52 保证成立。
- RR-70：撤销记录不随会话关闭清理，上界为政策持有的 pair；会话已关而 Interest 仍持有观察者时每次 Apply 报 Retry（与 `Resubscribe` 一致），未构造 Drain 卡住场景。
- `RegisterEntityKindDefs` 批量注册中途出错不回退已写入的定义（既有）。
