# 第八批修复：NC-21～25

2026-10-04，fetch/pull main无增量，起点`ce90e90d43cfaf7b71d3487a84ea2f3d32f97d0b`，工作树开工干净。按roost-bugfix先修五项，再按roost-review继续N04。仓库roost-coding/bugfix/optimize及本机完整包逐文件一致，本轮未改skill；无gh，未查询GitHub CI状态。不发版。

| RR | 根因与修复 | 实测范围 |
| --- | --- | --- |
| [NC-21](../bugfix/RR-20261004-NC-21.md) P2 | Eval错误透传，删除无条件DEL重放 | 真实Lua后丢回复/另一写者v3、执行前deadline/cancel/wrapped错误、已应用保留v2、正式生成DAO |
| [NC-22](../bugfix/RR-20261004-NC-22.md) P3 | 递归复制BSON可变容器，保持M/D | 嵌套abort、读输出、数组、Binary、CodeWithScope、Lookup/Documents、身份校验拒写隔离 |
| [NC-23](../bugfix/RR-20261004-NC-23.md) P3 | _id候选在规范化键上顺序去重 | Find、混合宽度重复ID、missing ID、Count/UpdateMany一次修改与计数 |
| [NC-24](../bugfix/RR-20261004-NC-24.md) P3 | 标准库big.Rat精确数值比较 | 2^53、int64极值、排序/$gt/$gte/$in、混合signed/unsigned/float、非有限值明确unsupported |
| [NC-25](../bugfix/RR-20261004-NC-25.md) P3 | 按选定/新插入物理键返回post-image | 多匹配、before/after、upsert、nil结果与错误无输出 |

修前原12叶子 **6fail/6控制**，当前正式测试确实变红后才改产品；修后旧产品overlay复跑仍相同，原始失败保留。[证据及准确计数](../bugfix/evidence/noncore-bugfix-20261004-08/README.md)。相关十包race/vet、真实Redis和两个DAO CLI生成消费者通过；扩展DataEngine/Kit与Service消费者仅回归其已有测试，不重审另一工作线核心实现。无真实Mongo/Cluster/HA/性能对照。

初版追加边界测试误把Set写前Get所用Pipeline计为写后fallback，导致四断言失败；纠正为Eval后计数并通过，保留初版日志，未将fixture失败算作产品RR。第一次沙箱Redis因MSYS命名对象权限失败，后经授权升权运行本轮独占loopback实例；两个红跑和最终绿跑退出均已记录。没有停止共享实例。

图谱roost-core ready，generation `2026-09-30T11:58:14Z`，Tier2；查符号、双向调用、片段和覆盖后，对旧代际/metadata_changed/not_tracked直接读当前源码与diff补证。图谱receiver/跨包启发式关系不能替代import和实际执行；没有强制刷新共享索引。[45路径覆盖](evidence/noncore-review-20261004-16/coverage.json)。

接续[第十六轮新审查](REVIEW-2026-10-04-noncore-16.md)；五项修复关闭，不递归修新问题。进度与预算见[PROGRESS](PROGRESS.md)和[计划](NONCORE-REVIEW-PLAN-2026-10-03.md)。
