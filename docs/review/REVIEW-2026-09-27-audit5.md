# 2026-09-27 第五轮独立审计（v1.17.1 `ffcf902`）

范围：清单 [OPEN-ITEMS-2026-09-27](OPEN-ITEMS-2026-09-27.md) B36——v1.17.1 中未经独立审计的 RR-20260926-81～85、RR-73 / RR-80 复核补修。非修复方审计员在导出副本上核验（`aud6-head`=ffcf902，`aud6-base`=9009268，
RR-81 基线 `36745f7`，RR-82 基线 `2af6687`）。图谱已过期，结论来自源码与实跑。

## 修复核验结论

| RR | 结论 | 备注 |
| --- | --- | --- |
| 81 | 成立 | RR-48 交叉创建用例修前 3 轮 `-count=300` 各失败 2/4/5 次，修后 3 轮 0 失败；同一 Guard 自我撤销形状 → [RR-20260927-21](../bug/RR-20260927-21.md)（本批引入，P4） |
| 82 | 成立 | 修前 `-count=2` 失败，修后整包 `-race -count=3/5` 通过 |
| 83 | 成立 | 修前第二轮 9 个用例失败并 panic；修后 `-race -count=20`、`-shuffle=on` 4 次通过 |
| 84 | 成立 | 修前 9 例失败 1 例保持性通过；广播 3 实体 `txInFlight=[true true true]` 不误判；“消息事务开始前调用”在替身环境实证成立（真实 remoteentity + Mongo 未测） |
| 85 | 成立 | 只加原子计数、不取新锁；RR-70/78/79 回归通过 |
| 73 补修 | 成立 | 错误文本带目标 ID |
| 80 补修 | 成立 | 解析失败只列文件 / 键 / 错误；dev 非法时长仍 FAIL |

门禁（审计员）：race 58 包、整仓非 race 120 包、glsvet、vet、sync-modes、game-demo 生成 build/vet/test/glsvet/doctor（无 WARN/FAIL，game 107s / 112s）通过。未连外部服务，未跑故障矩阵。

## 新发现

- **N1（P4，RR-81 引入）**：登记为 [RR-20260927-21](../bug/RR-20260927-21.md)。
- **N2（P4 文档，`f1d7591` 引入）**：CHANGELOG v1.17.1 称“`RunDetachedTransaction` 在无事务的 memory handler 内调用时同样不再认领消息，`skill/combatcomponent` 受此影响”不实——memory 快路径进入 handler 前已置 `txInFlight`，handler 内调用修前就按嵌套处理（探针 P84-D/E 基线与 HEAD 回复相同、都 fence）；真正变化的只有收尾阶段。已在 CHANGELOG 更正。

## 疑点（推断，未证实）

- RR-85：`Unregister` 先无锁取 `subj` 再加 `subj.mu`、途中不查 `forgotten`；若旧 subject 期间被 forget、同 ID 重新登记并 Bind，旧 subject 上取的释放戳晚于新绑定，`dropRetractedSubject` 按 ID 可能删掉新登记的记录，Direct 按戳删掉活绑定。根因修前即有、窗口极窄 → 清单 B39。
- RR-85：重新提交已取出、Subscribe 落下前同 ID 关闭又重开，重开连接未 Bind 却订阅（修前修后相同，未放大）——即清单 C20，已接受。
- USER_GUIDE §4 第 1 行“含收尾阶段调用的独立事务”：收尾阶段业务无法把错误带进回复，回复里看不到 `ErrCommitIndeterminate`（只是引擎已 fence），按“回复满足 errors.Is”一列读会误导 → 随 RR-20260927-21 一并改表述。
