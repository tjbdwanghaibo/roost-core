# NC-41/42 与 RR-09 残余的本地证据

2026-10-05；Windows/amd64 Go1.27.0、GOWORK=off。基线 `1272671557c57d6abc97b99471335e3fe9c3f299`，fetch/pull 后无 main 增量。[本轮](../../../review/REVIEW-2026-10-05-noncore-27.md) · [机制](../../../review/IMPLEMENTATION-OUTBOX-CLAIM-AND-OPENING-RECOVERY.md)。

| 执行 | pass 叶子 | fail | skip | 范围 |
| --- | --- | --- | --- | --- |
| 原始反例与控制 | 2 | 7 | 0 | outbox1红2控制、Open3红、sweep3红 |
| 旧产品 overlay | 2 | 7 | 0 | 只还原两生产文件，原行为同样复现 |
| 最终新增正式回归 | 13 | 0 | 0 | outbox3、Activity10；含合法恢复/租约围栏/对账交错 |
| 相关 race 矩阵 | 420 | 0 | 2 | Saga/Kit Saga、account/chat/global/activity、metrics |
| 根包 | 14 | 0 | 0 | 含依赖边界门禁 |
| 正式 CLI 生成双同步模式 | 2 | 0 | 0 | DAO/Entity→periodic/on_change 消费者 race |
| Mail两相关包race | 137 | 0 | 0 | nil/empty意图比较增量相关回归；不累加或称新增137 |

[逐叶结果](results.json)、[原红文本](red.txt)、[overlay 红](repro-overlay-red.txt)、[生成结果](generated-sync.txt)/[退出码](generated-sync-exit.txt)。build/vet/根包与race执行均退出0；矩阵后只更换诊断控制断言为可观察日志，最终13定向race复跑已绿，不相加成覆盖率。两项 skip 是 `TestBugfix7ActivityClusterRecoversPartialCompletion`、`TestBugfix7ActivityTaggedClusterLifecycle`，本机没有对应 Cluster gate，不能算通过。

[实际执行命令/退出码](checks.json)。本轮没有改三大核心生产逻辑，未重复glsvet；全仓编译/根包边界与正式生成链仍执行。

```powershell
& ./docs/bugfix/evidence/noncore-bugfix-20261005-17/Run-Repro.ps1 -Scratch D:/whb_s/.tmp/nc41-42-red-new
& ./docs/bugfix/evidence/noncore-bugfix-20261005-17/Run-Verify.ps1 -Scratch D:/whb_s/.tmp/nc41-42-check-new
& ./docs/bugfix/evidence/noncore-bugfix-20261005-17/Run-GeneratedSync.ps1 -Scratch D:/whb_s/.tmp/nc41-42-gen-new
```

Repro 的 exit1 要核对7个行为红与2个控制，不能把构建失败当红。Verify 遇错停止；生成入口仅向新的 scratch 生成，运行输出写到该证据目录，复跑生成入口前请先复制整个证据包到自己的工作目录，并指定 RepoRoot，避免覆盖历史结果。Repro/Verify 则从仓库原位置执行以定位源码根。原始JSON日志/生成工程保存在 `D:/whb_s/.tmp/noncore-review-20261005-27`，无凭据、二进制或缓存入库。

收尾整合 `b4152f15` 两份 skill 文档更新；28材料路径 LF 摘要仍与测试时一致。canonical coding/bugfix 各3文件完整镜像已备份后同步并核对；4个证据脚本解析与Collector读取原JSON实际运行通过。产品代码未再改动，无需因 skill 更新重复整个回归矩阵。

mongotest 不是真实 Mongo；Activity 默认 Memory，不宣称 Redis/Cluster、网络未知提交、broker ACK/重连、跨进程 HA/长期容量。旧版本混跑不由本轮修复解决。未等待或查询 GitHub CI，无 tag/发版/部署。
