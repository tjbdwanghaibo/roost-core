# v1.24 维护手册

如果这是第一次维护Roost，先读[入门手册](../GETTING-STARTED.md)。本篇按日常修改、升级、故障和发布组织；先找到你的场景，再执行对应步骤。

运行时代码冻结于 v1.24.0；v1.24.1 为文档及发布元数据补丁。支持保证范围为 macOS/Linux。本地 Windows 编译/检查的结果仅作当前验证证据，不扩大支持平台。

## 1. 版本与兼容

单模块发布，kit、codegen、demo 随 core 同一 tag。新业务固定 core 与工具版本，重新生成并编译工程。v1.24.0 最低生成物运行依赖是 core v1.24.0。文档补丁不改变最低版本和业务 API。

当前没有旧 WAL/schema/interest/客户端包的透明兼容。WAL只支持codec7；DAO只接受当前schema；Remote interest要求非零代际及完整身份；客户端RS v2区分PB、Sync、Lockstep。不要把单模块版本号与这些各自的线/存储格式号混为一谈。

## 2. 升级与回退

1. 记录当前 tag、配置版本、业务 schema、WAL 目录和依赖资源；备份数据库与需要保留的 WAL。不要复制正在被修改的 WAL 来冒充一致快照。
2. 停止新业务准入，等已接纳操作排空。停机超时继续保留底层依赖，不能把 context 取消当成真实完成。
3. 跨旧格式升级时，由能读旧 WAL 的旧程序先完成所需数据落库。新程序使用新 WAL 目录，schema 不同的数据由业务明确离线转换；框架不自动清库。
4. 重新生成消费者，执行 build/vet/test、配置检查及声明场景冒烟，再启动新版本。静态 Player 跨机迁移只依赖已落库数据，不搬本地 WAL 热状态。
5. 回退前检查新进程是否写入旧程序不认识的数据/格式。无法读取时恢复匹配备份并按业务方案处理，不能只把二进制降级。

v1.24.0 → v1.24.1 的运行代码相同，不要求清空/转换任何数据，不要求因这次文档清理重新生成业务代码。

## 3. 日常修改与验证

先同步最新代码并确认工作树状态，在独立 worktree 实施。业务修改遵循三大块局部聚集，写明拥有者、锁、准入点、结果未知、资源释放和中文契约注释；不要为方便接入复制另一套持久/并发状态机。

修复先留下能在修前失败的回归，后记录根因、变化、红绿结果及限制。必要文档直接维护到对应设计/实现篇及本目录，不再累积按轮次的交接流水账。源码历史注释里的 RR 和旧 docs 路径，可用 [文档清单](DOCUMENTS.md) 的固定提交追溯。

~~~sh
GOWORK=off go build ./...
GOWORK=off go vet ./...
GOWORK=off go test -count=1 .
# 改动包的定向测试；跨包行为修改再跑全仓
GOWORK=off go test ./...
# 并发相关改动在支持 race 的环境运行受影响包
GOWORK=off go test -race ./framework/nest ./framework/sync/entitysync
# 改生成器或模板：重生成，确认无漂移，再编译真实消费工程
GOWORK=off go generate ./...
git diff --exit-code
~~~

真实资源测试需要独立 Mongo/Redis/NATS/etcd 和唯一命名空间。不要打印/提交 env 凭据，不用共享业务库进行故障注入。测试文件数不是测试通过数，跳过、未运行、仅编译均分别记录。

## 4. 故障处理

| 现象 | 先确认 | 正确处理边界 |
| --- | --- | --- |
| Nest fence/提交未知 | 首个失败原因、WAL/存储结论、进程 incarnation | 停止安全写入并走恢复；不盲重发扣款/发奖 |
| strict 成功但 Mongo 尚旧 | 投影队列、held、版本冲突、outbox | strict仅承诺WAL fsync；需要落库时用投影等待 |
| WAL 格式拒绝 | 程序版本和codec、目录备份 | 用匹配旧程序完成落库；不删坏记录/推进checkpoint |
| Entity schema 不匹配 | 全部DAO descriptor及实际数据版本 | 校验发生在水合前；明确离线迁移计划，不自动“补默认值” |
| Sync 不发/延迟 | held/ready、订阅来源、profile、冻结预算、水位、背压 | setter只标脏；不绕过提交水位来降低延迟 |
| 客户端增量缺口 | epoch/base/对象存在、重连代际 | 恢复全量；不继续应用不可信增量 |
| Remote 已提交但快照旧 | outbox进度、确认、L2水位和读取策略 | 区分数据权威与快照水位；不把旧L1写回覆盖 |
| 总线停止/重订失败 | consumer关闭、在途回调、ErrSubscriptionBusy | 等真实排空；有限MaxDeliver不保证永不丢 |
| 停机超时 | Service与各Mod预算、仍在途操作 | 保留依赖并继续等待，禁止先释放连接/锁 |
| readiness Degraded | 哪个checker降级、是否仍满足准入 | Degraded仍可ready；Fail才阻断 |
| activity目标长期失联 | 持久投递状态、静态sid及人工管理操作 | 观察重试不自动耗尽，按业务决定收口 |

## 5. 发版

保持已有tag不可变。更新 release manifest 与两篇发布说明；新增API时才提高生成器最低版本和compat minimum lane。干净worktree执行 scripts/pretag.sh，包含生成、build/vet/tidy/test与tag存在性检查。运行时变化重验相关真实资源矩阵与消费工程；文档补丁清楚记录沿用哪个运行时验收，不能声称重新跑过。

提交和推送后创建 annotated tag、推送tag；不等待GitHub CI。随后下载真实tag验证模块解析与消费者，记录实际结果。发布不等于部署，不能顺便改线上资源。性能只能来自同环境同参数实际测量。

## 6. 维护文档的规则

接口/配置/格式改变时，同时更新设计篇、实现篇、源码定位、升级要求与已知限制。当前文档不保留相互矛盾的“旧段落+末尾更正”；过程讨论进Git历史，未解决问题留在KNOWN-LIMITS。历史证据链接固定commit，当前规范用仓内相对链接。
