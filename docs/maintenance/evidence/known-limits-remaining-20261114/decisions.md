# 待维护者决定（最后处理）

## 来自遗留归类（Q1–Q17，见 limits-triage/triage.md 汇总）
Q1 事务内门控进出场景 / Q2 同步打包接 state / Q3 Lockstep 迁 Stepper / Q4 换服崩溃旧档 / Q5 删空认领入口 /
Q6 Saga 投影追平 30s / Q7 WAL 目录带服务名 / Q8 demo 进入流程换 enter 模块 / Q9 模块清单声明配置 /
Q10 模块协议号稳定 / Q11 demo 金币来源 / Q12 portal 动态区服目录 / Q13 room §5 后续与 R11 /
Q14 在途换服清扫 / Q15 换服中止信号 / Q16 account 换服方案 §6 1–4 / Q17 地形 DAO 库范围

## 整理中新增
- #154 Team 归 game 服务（B，已按推荐实施）是否确认
- #37 Saga 原型 c128 余额 false：关闭（原型已删、正式实现门槛为 0）还是保留
- CellWorld 容量 B / C / E（B：成员刷新不强制全量，破坏自包含契约，实测最大收益；C：Base 按 Cell 分订阅，改主题布局；E：Base 按批次共享展开）
- self_sync RetryLater 语义改为按 subject 作废 + 轮转（推翻 C29），分支 tmp/sync-fixes 8c86065f/a0f07234，未合并
