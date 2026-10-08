# Outbox与历史收口说明（未发版）

维护者要求“收敛成唯一”“历史的需要收敛完”，随后明确“除了压测其他的都继续实施”。本批按这些要求完成非压测实现；不打新tag、不部署。对应[实现与review入口](OUTBOX-CLOSURE-2026-10-08-IMPLEMENTATION.md)，实际验收以[收口记录](../review/OUTBOX-HISTORICAL-CLOSURE-2026-10-08.md)为准。

| 编号 | 变化与原因 | 使用/升级 |
| --- | --- | --- |
| OC1 | 唯一outbox协调者统一启动、重试和停止；独立事务有界并行，避免责任收敛后全局串行I/O | 新配置`remote_entity.outbox_publish_workers`默认8，上限64；public ApplyRemoteCommits成功只代表持久回执，发布完成等FlushRemoteTransaction |
| OC2 | 失败页继续扫描、Layered元数据有界、读兴趣异步广播，消除已确认的饥饿/容量/读预算缺陷 | 旧自定义Backend需实现游标分页，才保证跨失败页进度；正式Mongo Backend已接线 |
| OC3 | Destroy等锁可取消，已持久准入/未知删除继续收尾 | Mutex接口不新增必选方法，旧自定义实现沿用有界LockWithTimeout契约 |
| OC4 | 停机WAL只读检查与完整隔离副本，保留坏记录及后缀 | 新命令walinspect；不能用于活跃writer，不自动修复业务数据或跳过记录 |
| OC5 | Go/C# Sync接收器防增量缺口；Unity适配应用失败关闭旧连接 | 每接收器一个流，切连接Reset；应用Full先清旧状态，后续Delta可继续create，正常鉴权重连恢复 |
| OC6 | 历史A6/A7旧租约路线按静态绑定/App锁收敛，A14保留kit；补本机HA/E14证据 | 不恢复旧handBackIdle；静态Player跨机迁移以已落库数据为准，WAL只保证落库，跨机WAL热迁移无需求；真实引擎未验 |

无WAL、Mongo存储或wire格式变更，不需要本批专属数据清空。改变公开ApplyRemoteCommits成功语义的自定义调用方须按OC1迁移；框架正式batch/投影/finalizer已同步。回退须回退整个实现及调用方，禁止删除Applied记录、强置Committed或提早归还额度。

旧4分钟约60.1 TPS、4520错误的数据仍是失败；有界并发的功能反例已红转绿，但新的1h Remote/Sync负载和性能profile暂停，不能宣称性能已恢复。
