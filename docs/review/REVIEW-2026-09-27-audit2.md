# 2026-09-27 第二轮修复后独立审计（本地 main `7706419`）

范围：RR-20260926-48～63 修复合并后，三名非修复方审计员在导出副本上独立核验修前红（基线 `3e26f91` 或各修复提交的父提交）、修后绿、约束符合度与跨组交互。
复现附录：[REPRO-2026-09-26-06](../bug/REPRO-2026-09-26-06.md)。

## 修复核验结论

RR-48～63 全部“修前红、修后绿”成立，未见回退；约束大体落地。RR-48 死锁修复有效（memory 模式新增重复执行 → RR-64）；RR-51 规则正确但默认值不足（→ RR-66）；
RR-52 引入 ctx 临近到期误关健康连接（→ RR-68）；RR-59 兜底 remove 后政策不重订阅（→ RR-70）；RR-60 同类两处（→ RR-71）。

合并门禁（审计员执行）：全量 race（53 包 / 24 包 / 18 包三组）、sync-modes、game-demo 生成 build/vet/test/race、dataengine / remote 生成链路、integration（跳过故障注入）均通过；机器人 3 轮 6/6。

## 新登记（均未修复）

RR-20260926-64（P2 潜在）、65～71（P3）、72（P4）。维护者 2026-09-27 拍板：RR-64/65 返回错误不重排；RR-66 生成器按 Mod 数计算停机总时长；RR-70 通知政策重新提交订阅。

## 须随发布补的文档

- CHANGELOG“Changed（破坏性）”：RR-56 非默认 prefix 且未配 `syncbus.stream` 的旧部署升级后改用派生流名，若旧 `ROOST_SYNC` 仍占该 prefix subjects 则启动失败；迁移：同 prefix 多实例不能滚动升级（先配 `stream: ROOST_SYNC` 或全停升级），共用 NATS 时不要删 `ROOST_SYNC`、只移除本 prefix subjects，清理旧流上的孤儿 durable consumer。
- CHANGELOG：RR-52 写失败即断开连接、部分成功返回 nil、已生成工程需重新生成；kit/README 中“不同 prefix 即各有各的流”对 `roost.room` / `roost.sync` 不成立（兼容映射）。

## 疑点（未登记）

- RR-54：包装 ManagerAccess 却不转发 `BindLocalExecutor` 的自定义 Getter，领头方离开后发布在加载 goroutine 上执行（正式装配不受影响）；非 Nest 领头方等待期间发布就地执行（修前即有）——写入契约文档。
- RR-48：对称交叉创建活锁未证伪（300 次试验未达上限，持锁越长重试越多）。
- RR-51：保底 3s 与 `nest.request_timeout` 3s 无余量；accessplayertcp 可能拿到约 2.9s。
- RR-55：排队的是已关闭 state 时重载实体可能一直未登记；Close 中途 ctx 超时时排队 done 不报告。
- RR-59：无 loader 时被当作“权威没有”（kit 装配不可达）；风暴下最坏延迟远超“分钟级”（4096 实体约 43 小时上界）。
- RR-61：延迟回调快照中的 KV 为浅拷贝，与原请求回复并发时可变值可能竞争。
- RR-62：业务自身删除 Remote 实体的并发窗口也会拿到“重载中”标签。
- 基线原有：生成传输 `closeSessions` 恒返回 0；`unsubscribe` 中 defer 顺序若走到会自锁；demo scene Manager 未接 RR-59 重载链路。

## 关闭说明（2026-09-27）

来源：[OPEN-ITEMS-2026-09-27](OPEN-ITEMS-2026-09-27.md) E 节。上文原文保留。

- “须随发布补的文档”：CHANGELOG 已写 RR-56 迁移与 RR-52 行为（`CHANGELOG.md:14-18`）；kit/README 那句已在 OPEN-ITEMS A11 改为注明
  `roost.room` / `roost.sync` 兼容映射到同一个 `ROOST_SYNC`。本节关闭。
- 疑点“`unsubscribe` 中 defer 顺序若走到会自锁”：已改为解锁后再 `forget`（`sync/entitysync/subscriptions.go:423-431`，RR-20260926-69）。关闭。
- 其余疑点另有去处：RR-54 包装 Getter 契约已写入 USER_GUIDE（A13⑧）；生成传输 `closeSessions` 恒 0 → A02；RR-48 活锁 → B24 / C09；
  RR-51 保底无余量 → C05；RR-59 最坏延迟 → C14；demo scene 未接重载 → B25；RR-59 无 loader、RR-62 业务删除窗口 → D38。
