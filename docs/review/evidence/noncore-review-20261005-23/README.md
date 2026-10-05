# N05 接入审查证据

基线 b2232db5，Tier2 Verify，项目roost-core，generation2026-09-30T11:58:14Z。候选图谱路径22，覆盖检查见[coverage.json](coverage.json)，当前源码身份见[source-hashes.csv](source-hashes.csv)。metadata_changed/not_tracked均直接读当前源码；未重建或停止共享索引。

本轮11个完整读取生产文件与具体入口列在[review](../../REVIEW-2026-10-05-noncore-23.md)。测试文件仅作为对应场景证据，不将22路径除以N05历史24文件算覆盖率。cache泛查询70/76只作候选，其他相关定向查询完整分页；同名receiver/跨包trace误边未作实质结论证据。

[红绿与完整本地矩阵](../../../bugfix/evidence/noncore-bugfix-20261005-13/README.md)：两个确认RR闭合，新增13正式叶子，最终592相关race叶子、根包14以及build/vet/glsvet通过。真实broker/HA/跨节点删除水位/长期容量未验收。
