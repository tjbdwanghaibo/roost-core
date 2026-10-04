# RefHMap schema/并发/恢复及迁移消费证据

e7027a65 起点，NC-30修后本地源码；[review19_test.go.txt](review19_test.go.txt)通过 Go overlay 加入 cache，不写入产品测试。独占 Redis8.8.0，`go test -race -count=1 -timeout=120s -json -run '^TestReview19RefHMap$' ./cache`，[16叶子通过](review.jsonl)、[退出0](exits.json)、[实例退出](cleanup.json)。

两种 Patch/Set 先后、两种并发 leaf、三种 Patch 未知恢复、nil分支清理、三种 schema读取/Patch、未知后Delete、Delete后Patch共13正常/边界；三个已知边界观察为非原子读、旁支过期和Stale竞争。观察断言证明现象，并非接受业务快照/全对象TTL/CAS正确性。无任意 sleep；旁支使用实际 PEXPIREAT，未知是 Eval 前/执行后错误注入，不是弱网代理。

复跑使用 [Run-Verify.ps1](../../../bugfix/evidence/noncore-bugfix-20261004-10/Run-Verify.ps1) `-Mode Review`，提供 Go/Redis/RedisCli、RepoRoot 和新 OutputRoot。NC-30 red/12正式绿、六包221叶子回归和11正式生成消费者见 [修复证据](../../../bugfix/evidence/noncore-bugfix-20261004-10/README.md)。

[inventory.csv](inventory.csv) 41 N04产品候选，40摘要相同复用上一轮 full-source 记录，ref_hmap.go 当前全文/diff。SourceHead 为本轮起点，Blob 沿用前轮原始快照，WorkingSHA256 为当前树；ThisRoundEvidence 区分复用和新增。测试与 Codegen 消费者不扩大产品分母；不是本轮新读41文件或业务完成100%。

[coverage.json](coverage.json) project roost-core，Tier2 Verify，generation2026-09-30T11:58:14Z。全部18材料路径无记录gap但metadata_changed/not_tracked，用当前源码补证；[source-hashes.csv](source-hashes.csv)。精确文件搜索相关页已翻完，广义BM25仅定位候选。receiver合并/heuristic误边不作正式依赖证明，当前模板与实际生成编译补调用证据。模板/生成器/Tracker读取只限本轮机制，不称全量核心review。

N04场景仍部分完成；下一正式Repository/持久迁移与重载接入（复用核心另一线证据），然后N04收口/ N05路由与镜像增量。真实Mongo/Cluster/HA/弱网/长期容量未本机验证；另一线C01通过记录保留，但不据本批源码或单Redis结果推广。 [进度](../../PROGRESS.md) · [运行](../../REVIEW-2026-10-04-noncore-19.md) · [学习](../../IMPLEMENTATION-DAO-MIGRATION-HYDRATION-AND-REFHMAP-SCHEMA.md)。

提交前整合上游 c888223f 后，新增 RR-20261004-02～07 六项待修；上面的迁移接入顺序后移，先处理缓存/索引/生命周期新问题。[接手状态](../../REVIEW-2026-10-04-noncore-19.md#提交前上游审计与下一批待修)。RR-03 与本批旁支过期观察关联，不能因观察通过而判已修复。根包补验通过，不代表六项新问题的行为复现已通过。合并后格式化恢复本批 Go 文件的 LF，18 路径及41候选原始字节摘要全部复核相符；没有为了通过校验更换摘要。

**最终同步**到d49f02f1，另一线RR-02～06已实施，RR-07仍待修。RR-03自然TTL/续期/缺子记录正式回归和[当前16场景夹具](review19_merged_test.go.txt)在合并源码上通过，旧夹具及16pass是修前现象；[独立合并证据](../../../bugfix/evidence/noncore-bugfix-20261004-10/merged/README.md)。上方摘要相同结论只限c888223f阶段，现在上游改变了RefHMap/Layered等源码，原41清单与18摘要保持历史，不冒认已审的新快照。下一RR-07→正式迁移接入→N04收口。[最终状态](../../REVIEW-2026-10-04-noncore-19.md#最终同步与合并验收)。

最后同步到b9625f4f，07也已实施，追加相关生命周期114普通叶子race/vet与根包12叶子通过；19材料内容没有再次变化。[最后接手记录](../../REVIEW-2026-10-04-noncore-19.md#最后增量同步)。新Wanted W-2026-10-04-02优先真实NATS复现，分流为再观察，其次才迁移接入/N04收口。
