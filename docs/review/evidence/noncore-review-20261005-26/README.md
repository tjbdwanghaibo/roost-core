# N06 第二批证据范围

[审查](../../REVIEW-2026-10-05-noncore-26.md) · [红绿与本地矩阵](../../../bugfix/evidence/noncore-bugfix-20261005-16/README.md)。

Tier2 Verify；项目roost-core，根D:/whb_s/cube-core；ready32285 nodes/209828 edges。generation2026-09-30T11:58:14Z落后于cb11be90与本轮源码。[coverage](coverage.json)覆盖33材料路径，没有记录解析缺口，metadata_changed/not_tracked均由当前精确源码补证；[LF摘要](source-hashes.csv)只用于下一轮识别变更，不等于33文件穷尽审完。

search_graph定位StartSaga、Complete、applyCompletion、native consumer、Reserve/Bind/Replay、Mongo接口/transaction与servicemetrics；有界查询has_more=false。trace双方方向与片段确定入口，标准库同名误边不用作依赖判断。旧行号片段闭包位置偏移以当前源码校正；未刷新共享索引，不从0caller/无缺口推定不存在其他消费者。

正式Store测试后端为mongotest，JetStream为可控transport；native两个控制手工注入权威receipt。外部Mongo/NATS、未知提交/HA/容量留项。Unix codegen进程树用例不在Windows构建中，不冒认作者macOS压力结论。Service增量仅列变更，不算全部源码/场景验收。

最终整合 `47a9132c` 的 Nest U-0279，[三条新增路径 coverage](delivery-coverage.json)仍为旧 generation；新方法使用当前差异和源码补证，未以图谱缺失推断不存在。原33材料路径LF摘要未变，合并后本地 race534/根包14/build/vet/glsvet结果见[交付证据](../../../bugfix/evidence/noncore-bugfix-20261005-16/delivery-results.json)。新增 Nest 测试由最终 race 矩阵执行，上游真实生成工程压力没有本轮独立重复。
