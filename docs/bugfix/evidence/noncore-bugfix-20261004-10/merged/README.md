# 上游修复合并后的独立验证

父基线 d49f02f1，测试时本轮未发布提交 baf56bb7（后续只补文档）。保留外层 e7027a65 / NC-30 原红绿、c888223f 根包证据，不改写历史结果。

| 验证 | 实际结果 | 证据 |
| --- | --- | --- |
| 六相关包 race/vet | 241叶子 pass、0 fail、0 skip，含NC-30的12正式与上游RR-02/03/04回归；kit/redis无测试 | [JSONL](green.jsonl)、[退出](green-exits.json) |
| 当前review夹具 | 16叶子 pass、0 fail、0 skip | [JSONL](review.jsonl)、[退出](review-exits.json) |
| CLI正式生成消费者 | 11叶子 pass、0 fail、0 skip，race/vet通过 | [JSONL](consumer.jsonl)、[退出](consumer-exits.json) |
| 根包门禁 | 12叶子 pass、0 fail、0 skip | [JSONL](root.jsonl)、[退出](root-exits.json) |

三次Redis均为Hidden、随机loopback端口的独占实例，结束已关闭：[Green](green-cleanup.json)、[Review](review-cleanup.json)、[Consumer](consumer-cleanup.json)。[统计](summary.json)使用叶子计数；空vet日志表示实际无输出，退出0另存。生成消费者仍用外层同一组定义和测试，未改模板。

复跑使用外层[Run-Verify.ps1](../Run-Verify.ps1)各模式及新OutputRoot。Review默认改用[合并后夹具](../../../../review/evidence/noncore-review-20261004-19/review19_merged_test.go.txt)：仅把sibling_expiry_observation从“ok=true零值”改为RR-03的“整条miss”承诺，其他15场景不变。原夹具及原16pass留作修前现象，不能在已修源码上期待旧错误继续出现。RR-03自身自然TTL及无sleep控制也在Green正式包内执行。

RR-05相邻`GOWORK=off go test -race -count=1 -timeout=120s -json ./mongo/mongotest`补验115叶子通过、0fail/0skip：[结果](mongotest.jsonl)、[退出](mongotest-exits.json)。这不是实际Mongo对照验收。

最后整合b9625f4f（RR-07）没有改变19材料内容，19摘要再核对相符。追加Bus/KitNats/NatsDriver/ServiceRPC普通包race：114叶子pass/0fail/0skip，[事件](lifecycle.jsonl)/[退出](lifecycle-exits.json)，[vet退出0](lifecycle-vet-exits.json)；[最终根包12叶子](final-root.jsonl)/[退出0](final-root-exits.json)。未跑连接drain超时的真实NATS，新增W-2026-10-04-02仍待独立复现。

[source-hashes.csv](source-hashes.csv)为合并后19材料路径的实际工作树字节快照，外层18路径与41清单摘要是原测试阶段快照，不替换它们。合并覆盖报告[coverage.json](coverage.json)仍是旧generation，按当前源码/差异补证；误猜的cache/readthrough.go不存在，不引用它，不据此作穷尽结论。RR-06真实etcd验证、RR-03 Cluster与RR-05真实Mongo对照仅引用另一线记录，本机没有这些外部验收；本机mongotest检查另存结果，不冒认真实Mongo。
