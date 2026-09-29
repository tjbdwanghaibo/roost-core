# 2026-09-29 Service 第二轮 Review

本轮接续 Core 全 service 主链审查，针对身份编码、迁移约束、管理状态输入和提交失败恢复继续验证。基线 `e10dd3f18d2da985f969cbc54cb19a87f1f287af`；已 fetch，并确认审查开始时 `main == origin/main`。只写文档，生产源码未改。

收尾再次 fetch：远端前进到 `70fb6c13`，新增其他 agent 的 B30 文档记录，未改变 service 或其他源码；本轮源码基线仍有效，提交时保留并合并这些远端文档。

新增 **8 个已复现问题（6 P2、2 P3）**，另补旧 RR-20260909-02 的正常 Finish/真实 Redis 触发，不重复编号；1 个活动完成重叠仅作为观察。[问题与实施交接](../bug/REVIEW-2026-09-29-services-02.md) · [复跑](../bug/REPRO-2026-09-29-services-02.md) · [机制文档](IMPLEMENTATION-SERVICE-STATE-AND-RECOVERY.md) · [证据](evidence/service-review-20260929b/) · [进度](PROGRESS.md)。

## 新审查范围与结果

| 服务域 | 本轮新增验证 | 结果与边界 |
| --- | --- | --- |
| account | 已验证身份键单射；slot 已写/响应丢失后的补偿 | RR-11/12；自定义冒号渠道与确定性故障注入，不是网络实测 |
| global | 完成迁移后旧 lease 续期；Load 输入/输出所有权 | RR-13/17；三种输出污染仅 MemoryStore |
| session | Attach 的服务管理字段；终态 Finish 重试；正常 Finish 与新 Enter 的交错 | RR-14/18 + 旧 ABA；ABA 使用真实 Redis |
| activity | int64 累加上界；已准入进度与完成重叠 | RR-15；重叠观察未定缺陷 |
| mail | 账本已占位/正文未写成的恢复出口 | RR-16；正文写入前单次失败 |
| match / chat | 当前存储/队列与已记录问题去重，原有回归 | 未新增编号；历史积压容量/有限去重窗口没有扩大结论 |

第一轮已完成的 **10/10 服务域主链**状态不等于全部正确性证明。本轮新增 8 个反例，说明还需继续审故障分支；不把测试数、文档数或同一文件复读算成新的“代码审完百分比”。本轮未重验前十项修复，也没有修改旧修复的历史验收结论。

## 执行证据

隔离工作树 `D:/whb_s/.tmp/review-service-20260928`，临时测试仅在该工作树注入；本轮隔离 Redis 监听 `127.0.0.1:16390`，禁止落盘。未使用生产 Redis。

```powershell
$env:GOCACHE='D:/whb_s/.gocache'
$env:ROOST_REVIEW_REDIS='127.0.0.1:16390'
go test -race ./service/session ./service/mail ./kit/service/account ./kit/service/global ./kit/service/global/activity -run '^TestReview' -v -count=1 -timeout 60s

$env:REDIS_ADDR='127.0.0.1:16390'
$env:ROOST_REDIS_TEST_ADDR='127.0.0.1:16390'
go test -race -tags integration ./service/session ./service/mail ./service/match ./kit/service/account ./kit/service/global ./kit/service/global/activity ./kit/service/chat -skip '^TestReview' -json -count=1 -timeout 120s
```

新场景：**9 个顶层 FAIL**（8 个新 RR + 1 个旧根因的新触发），其中 Load 所有权包含 3 个 FAIL 子场景；**1 个观察 PASS**。未发现 Go race 报告。不能把子测试和父测试都算不同 bug。

原有回归：7/7 包通过，425 个测试/子测试通过（432 个 pass 事件包含 7 个包终态），没有 fail 或 skip。包含真实 Redis integration 环境；不代表所有七个包都有真实 Redis测试。生成文件未改，本轮没有重复运行第一轮的 12 次生成门禁，也没有给生成调用链新增端到端通过结论。

## 图谱与源码证据限制

本轮开始确认 `roost-core` 项目存在；index_status 显示 ready，但 generation 仍为 `2026-09-20T01:02:19Z`。显示当前 HEAD 不能证明节点已覆盖最新合仓目录。

按 skill 显式提交一次 full index_repository 刷新，调用 300 秒后超时；后续 index_status/search_graph 也超时。没有重启、杀掉共享 MCP 或反复提交刷新。对本轮 34 个证据路径和 7 个 scope 发起 check_index_coverage，同样 300 秒后超时；全部本轮实质结论按源码补证，不拿旧图谱的零结果做否定结论。本轮无法确认 watcher/刷新已完成，索引状态不能标绿。

材料中的源文件位置均对应固定基线；候选函数的关键分支、存储契约和现有测试 helper 已直接回读，并通过独立反例核实。路径清单见 [source-evidence.csv](evidence/service-review-20260929b/source-evidence.csv)。这是有界审查，不是完整分页的全图审计。没有依靠图谱判定死代码或不存在恢复路径；邮件结论限定公开 Send 的同请求恢复，session 结论保留 Enter/Sweep 补救。

## 设计、性能及后续停点

身份键要单射；跨键状态要共同约束；提交未知不能直接补偿；输入不能携带由服务拥有的完成证明。学习与实施策略已追加到[机制文档](IMPLEMENTATION-SERVICE-STATE-AND-RECOVERY.md)。

本轮没有吞吐或 p95/p99 测量，不把克隆 map 的小额成本、跨记录查询代价或 match 历史对象增长写成已测性能结论。优先修一致性，再用有界故障/容量矩阵测性能。

下一轮未修复时：跳过本轮和第一轮旧触发，继续 account 名字/角色各写入点的崩溃恢复、global Acquire/迁移的并发约束、mail claim 在途到期与正文丢失的分类恢复。已修复时：先核对新的 bugfix 和红绿证据，尤其 ABA 的原子身份条件，而不是只看新增一个 Get。其他 agent 负责的非 service 模块不在本次范围。
