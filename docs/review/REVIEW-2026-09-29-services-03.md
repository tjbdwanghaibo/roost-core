# Service 第三轮运行与学习记录（2026-09-29）

本轮继续 roost-review，只修改文档及文档目录内的 overlay 复现材料。确认 [RR-20260929-19..22](../bug/REVIEW-2026-09-29-services-03.md)：3 P2、1 P3。已有服务回归全部通过；新增相邻路径仍有行为失败，因此不称 service 已无问题。

## 基线与旧修复

单仓 `D:/whb_s/cube-core`，module `github.com/tjbdwanghaibo/roost-core`，remote `https://github.com/tjbdwanghaibo/roost-core.git`。本轮 fetch 后 main/origin/main 保持 `83c042438ea9ae235cfe3a41a1a7217949139670`，工作树干净；没有新的 Core 源码增量。远端 demo-generated 分支推进不作为 main 审查基线。旧三仓分类已合并为当前 Core 内的 service/、kit/service/、codegen，未审归档仓。

复用隔离工作树 `D:/whb_s/.tmp/review-service-20260928`，同 SHA；没有修改生产 Go、正式测试或依赖。阅读上轮 bug/bugfix、ledger 及进度，检查 Account slot 尾提交处理、Session 原子身份删除、Mail 过期清理与 Intent 恢复等邻近实现。旧 19 项修复的现有回归本轮通过；这是已有行为检查的复跑，**不能据此宣称 19 项所有未知结果/并发交错都独立验收完成**。新 RR 保留旧修复有效范围。

## 图谱与源码证据

使用 codebase-memory，Tier 2 Verify，匹配项目 `roost-core`、根 `D:/whb_s/cube-core`；不是聚合 D-whb_s。session reset 后 index_status 再确认该根 ready。初始 generation 为旧代，显式 full index 请求一次后 RPC 300s 超时；后续 coverage 实际报告 full generation `2026-09-29T03:13:02Z`、recorded_at `03:13:03Z`、recording_status=complete、hash_records_complete=true。因此可以记录新 coverage 已发布，不能把超时的请求写成成功返回。

使用 search_graph 定位 CreateRole/UpdateProfile/ValidateSession 接口与生成器方法、Directory Cancel/Commit/Release、Session releasePending；trace_path 双向查看 releasePending、Directory Release，get_code_snippet 核对 Cancel 与 releasePending。相关 search 返回 has_more=false，接口/实现方法有图谱识别缺口；Directory Release trace 没有显示调用者，但源码中 Account 有调用，**不判为死代码**。releasePending 图谱把 errors.Join 映射到其他 Join 亦经源码纠正，不依赖该边。

候选路径第一批 29 个，补查 6 个（部分重复），合计 32 个不同路径，见 [coverage 摘要](evidence/service-review-20260929-03/COVERAGE.json)；范围覆盖 Account/Directory/Mail/Session 主链与 Redis/versionstore、装配、相关现有测试和 ledger。均提示 metadata_changed，不能仅凭 ready/无 recorded gap 声称完全新鲜。关键分支回读本轮 SHA 源码；ledger parse_partial 的 249/486 行直接读取。不是全 service scope 的穷尽图审计，未把 clean coverage 当完备证明。

## 实际执行

既有回归（不使用 overlay）：

```powershell
$env:REDIS_ADDR = '127.0.0.1:16392'
$env:ROOST_REDIS_TEST_ADDR = '127.0.0.1:16392'
$env:ROOST_REVIEW_REDIS = '127.0.0.1:16392'
go test -race -count 1 -p 1 -tags integration -json ./versionstore ./service/... ./kit/service/...
```

结果：退出 0，**16 个包通过；759 个测试/子测试 pass 事件，0 个测试级 skip、0 个失败**；servicemetrics 装配别名包无测试。这是 test2json 事件口径，不是 759 个独立业务场景，也不是源码覆盖率。

本轮独立 Redis 端口 16392，仅用于隔离验证；首次 Windows/Cygwin 配置路径不兼容导致 Redis 没启动、基线失败，改用 /cygdrive/d/... 配置路径，PONG 后重跑上述全量。首次环境失败不记成产品问题，不计通过。Redis 测试前缀独立，本轮结束只停自己的实例，未动共享 MCP 或其他 Redis。

新增定向证据：Go overlay 将 docs 内四个 .go.txt 映射为包内临时测试，仓库真实源码没有临时 .go 文件。运行 -race/-count 1/-timeout 2m；新增缺陷断言在当前基线预期 exit 1。账号/邮件分别跑 Memory 和 Redis；目录一次包含两个后端；Session 观察跑 Memory。

| 有界场景 | 执行数（叶子，跨后端分别计） | 结果 |
| --- | ---: | --- |
| Account 三处写后响应丢失 | 6 | 6 行为失败，RR-19 |
| Account 三处写前失败控制 | 6 | 6 通过，干净重试 |
| Mail 旧取消解除新预约 | 2 | 2 行为失败，RR-20 |
| Directory Cancel/Release 删除重建 | 4 | 4 行为失败，RR-21 |
| Account 两种 Profile 输出修改 | 4 | Memory 2 失败，Redis 2 通过，RR-22 |
| 过期未结算 mail 容量及显式处置 | 2 | 2 观察通过，保留证明 |
| Session 并发 Finish 释放回调 | 1 | 1 观察通过，2 次回调 |
| **本轮选定场景** | **25/25 已执行** | **14 缺陷反例失败，11 控制/对照/观察通过** |

25 是本轮人为声明的小场景分母，不代表 service 全域覆盖。重跑 Directory 最终 fixture 的相同 4 叶子不重复计。最初容量断言误期待自动清理，依据源码安全语义改成观察并验证显式处置恢复；最终材料与记录均采用观察结论，不把最初错误假设计为 bug。

[复跑脚本、四份源码及结果摘要](evidence/service-review-20260929-03/README.md)。未运行生产发奖/支付、Broker、断网 HA、进程强杀或负载延迟测试；现有 12 次生成检查来自同 SHA 的上轮记录，本轮无生成源码变化，未重复运行，也不把未来 API 修改说成已通过生成检查。

## 覆盖停点与下一入口

[进度表](PROGRESS.md) 保留 10/10 service 域“主链已有记录”的历史范围状态；本轮新增四域邻近恢复/并发/所有权场景，不转换为文件审完率或框架正确率。旧修复完成、新问题确认、全域尚未穷尽是不同状态。

学习文档 [提交未知、预约身份与对象所有权](IMPLEMENTATION-SERVICE-COMPENSATION-AND-ATTEMPT-IDENTITY.md) 解释当前机制并给实施边界。下一轮可从 Account 建角崩溃恢复/补偿再失败、Mail Delete/Commit 与在途发奖交错、Directory 同 Owner 换 Token、Platform poison/未知外部结果推进；真实 adapter 并发释放和冷热容量仍缺证据。用户声明没有修复时，跳过旧问题验收，直接继续这些新内容。
