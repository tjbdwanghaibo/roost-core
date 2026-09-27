# 2026-09-28 第七轮独立审计（本地 main `611dab1`）

范围：RR-20260927-29～35、RR-20260928-01～06、B44 / B45 测试修正、`adfa7ac`（USER_GUIDE §4 判别表第 14、15 行）。非修复方审计员在导出副本上核验（各修复父提交为修前，必要时补无行为 shim）。RR-20260928-07 不在范围（审计期间合入）。图谱已过期，结论来自源码与实跑。

## 修复核验结论

13 条修复“修前红、修后绿”全部成立；RR-20260927-29、31 自报未验证的形态（Nest 领头方迟到步骤、嵌套形态）经探针证实修前错、修后对。RR-20260928-03 部分成立：第 4 行判据在 strict 下有反例（→ [RR-20260928-08](../bug/RR-20260928-08.md)）。
第 15 行兜底：逐一核对准入、停机、慢阶段准备、pipelined ticket、收尾失败、嵌套 detached / isolated、memory 路径，未找到“不命中 1～14 行、实际已提交或结果未知”的回复；第 14 行覆盖 `waitResult` 的全部三个出口。
RR-20260927-32：`rejectCommit` 调用点都在写入之前，三个 committer 的非 indeterminate 错误都发生在写入前，生产代码无按 `ErrCommitRejected` 的分支。
RR-20260928-01 并发探针（16 接线 × 2000 轮、一半并发 stop）gauge 始终等于份额之和；RR-20260928-02（Manager, ID）键在登记与查询两处一致。
生成器层面：基线 `a136c98` 生成的旧工程经 HEAD sync 更新 17 个文件，之后与新工程只差 prod 示例、Secret 示例、`syncbus_config_test.go` 三个脚手架文件，与修复记录的手工合并清单一致。

门禁（审计员）：新回归 `-race -count=50`、相关整包 `-race -count=20`、`codegen/internal/roost` `-race -count=1`、整仓非 race 120 包、vet（含 integration tag）、glsvet、gofmt、sync-modes、新旧生成工程 build / vet / test / doctor / diff 均通过。未连外部服务。

## 新发现

- N1（P4，RR-20260928-03 引入）：strict 下 tracker 被淘汰后重新登记报 Overloaded，回复被误标 `ErrRemotePartRejected` → RR-20260928-08。
- N2（P4 测试卫生）：`replySentinels` 未跟上改号 → 并入 RR-20260928-08。

## 疑点

- 第 14 行措辞只点 `Nest.Request`（`RequestMulti*` 同一出口）；`validateClientDispatch` 在 ctx 已取消时返回的 `ErrNestCanceled` 确定未执行，按第 14 行“结果未知”偏保守，可接受。→ 措辞并入 RR-08。
- B19 并存形态：回复同时带 `ErrRemotePartRejected + ErrNestedTransactionCommitted`、首个命中第 4 行，外层文案与“本地已提交”不符。→ 并入 RR-08。
- RR-20260928-05：install.sh 失败时自动回滚到旧 release，旧 release 无 `configs/data`；若运维此前手工在 `$APP_ROOT/configs/data` 放过数据，自动回滚会失败（推断）。→ 兼容性补写并入 RR-08。
- RR-20260927-32：带 Remote 批次、无 effect 的 memory handler 在别的消息 fence 引擎之后仍会写 Remote（修复方已声明的范围说明）。
- `glsvet -tests` 在 `nest/group_lock_test.go` 报 3 条（修前即有，`ae671a0`）。
