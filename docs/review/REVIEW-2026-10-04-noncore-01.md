# 非三大核心第一批修复：App / HTTP

用户本轮要求“修复，并继续 review”。main 更新前/后均为 `183b0bddc5b46f6d3b50c5edda2d08da45150a69`，fetch / ff-only 确认最新。工作树初始干净，仓库维护的三份 skill 与本机镜像字节一致。gh 未安装，没有查询远端 CI；本地回归实际结果另列。

按 roost-bugfix 完成原四项 NC-01～04；[四项修复记录](../bugfix/README.md)、[正式红/绿和可复跑入口](../bugfix/evidence/noncore-bugfix-20261004-01/README.md)。没有修改模块/依赖/生成接口、核心三大模块或外部业务数据。

| 编号 | 修复行为 | 实证 |
| --- | --- | --- |
| RR-20261003-NC-01 | 空集合外均进入已有 Mod 校验；单 Mod 也拒绝非法依赖/环 | 原 12 场景，7 个反例转绿；两个 Execute 入口不进入 Init |
| RR-20261003-NC-02 | 库拥有 client 的 Clone timeout 改副本；外部 client 不重配 | 原 deadline 300ms → 20ms；父/子链/自定义优先级/context/并发对照 |
| RR-20261003-NC-03 | 非 2xx StatusError 与读/解码错误共存 | 原 503 文本/401异型转绿；保留错误体、partial body、Close、decoder cause |
| RR-20261003-NC-04 | Unmarshal 整段单值校验先于业务 | 尾随垃圾/第二值 400 且 business_calls=0，空白/空body/超限对照保持 |

正式原 30 场景从 12 fail/18 pass 转为全通过；加入15个邻接对照后共45个新增场景。最终 11 个关联包 race/vet 全部通过，194 个 test pass 事件（含父/子/Example）、0 fail/skip。原 review overlay 原文也全绿，35 个 pass 事件包含 30 个叶子/独立场景。初次七包结果已被此有依据的扩展检查补齐，统计详见 verify-summary。

Tier 2 图谱重新定位 sortMods/Clone/DoJSON/BindJSON，双向 trace 无截断，关键片段/coverage 核对。generation `2026-09-30T11:58:14Z`，freshness metadata_changed；当前源码读取补证，新增正式测试 not_tracked。没有把 heuristic 同名边当真实调用，没有刷新或停止其他 agent 的共享索引。

设计没有引入新 HTTP 框架、依赖排序框架或重试层。兼容收紧点：非法单 Mod 和非法复合请求被拒绝；HTTP 部分错误由 Join 包装，应 errors.As/Is 分类。WithHTTPClient 的优先级保持，默认/非正数 timeout 行为不变。源码机制学习增补见 [App/HTTP](IMPLEMENTATION-APP-AND-HTTP-BOUNDARIES.md)。

未发版。后续接续 N01 的生命周期、Manager Engine、Admin/Health 和 Kit Ops，新的确认问题只登记 review，和这四项关闭结果分开管理。不是四项变绿就将整个 N01/N02 标记完成。
