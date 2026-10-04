# 非三大核心第十二批：正式生成 DAO 的迁移写回与重载接入

2026-10-04。更新前`04d679b8`，干净main fetch/pull快进至`341379254a5f74bcf445c473fde161b1b5e4cf19`；新增上游是playerowner修复与文档。本轮接续[N04停点](REVIEW-2026-10-04-noncore-19.md)，只补正式消费链，不重新审另一线Nest/Sync/DataEngine全域。

**确认一个新P2，未修：[RR-20261004-NC-31](../bug/RR-20261004-NC-31.md)**。普通迁移输出缺少提交前目标类型/身份校验，坏结果先进入WAL或写成目标schema，之后才被重载拒绝。没有把这项写成正常“边界”。

## 范围与实际结果

| 范围 | 证据与结果 | 结论限度 |
| --- | --- | --- |
| 正式生成消费者 | 公开Repository→Runner→文件WAL/Projector→MongoStore→重读→生成RestorePersisted；11叶子=3fail/8pass/0skip，race无报告、vet0 | 3反例同一个缺少提交前校验根因；后端是mongotest，非真实Mongo |
| 成功与输入拒绝 | schema1经两步到3，version7到8，目标字段正确；新Manager无Runner重载成功；当前schema不提交；较新schema/步骤error不提交 | 单DAO/scalar消费链；多DAO与跨进程升级未由此关闭 |
| 原hydration对照 | 直接生成Restore对坏BSON/类型拒绝；改ID仍可解码，由Repository负责身份 | 不把生成decoder当身份校验器，也不混淆内存转换与写回 |
| 取消/恢复 | 投影被门闩阻塞时Entity未发布；取消调用者后返回Canceled；释放门闩并Flush，用新Manager加载迁移后的version8 | 真实WAL+正式MongoStore，受控投影门闩；非真实网络/进程强杀 |
| 既有局部门禁 | 34叶子定向race通过，根包14通过，相关vet通过；`GOWORK=off go build ./...`退出0 | 包绿不覆盖新反例；不等待或查询GitHub CI |
| 中文注释 | Repository.LoadEntity、Runner.Migrate、迁移全局/实例入口补职责、取消与持久边界中文说明 | 只改注释，未改接口、执行语句、生成逻辑或正式测试 |

[可复跑夹具、命令、原始事件及退出](evidence/noncore-review-20261004-20/README.md)。初版探针误用不存在的Stats字段、门闩替身嵌入MongoStore而暴露ProjectFenced绕过Project，修正后重跑；两者是夹具错误，不计产品红。最终归档版本只包含已验证的3行为失败与8对照，初版日志留本机scratch。

## 图谱、源码与进度

Tier2 Verify，project **roost-core**，generation`2026-09-30T11:58:14Z`；ready32285nodes/209828edges但早于基线。精确migration.go、entity_repository.go、migration_runner.go、template_dao.go搜索均已完成相关页；广义migration查询未翻完，只用来发现候选，不作穷尽结论。双向loadAggregate/MigrateDAO trace含heuristic跨包误边与接口漏边；当前源码和正式生成编译补证，不把零caller当无人调用。

coverage对材料路径为metadata_changed；初猜不存在的persisted_payload.go更正为mongo_load.go，golden仅被图谱定位、不计源码已读。本轮核心同行材料按[逐文件范围/摘要](evidence/noncore-review-20261004-20/source-hashes.csv)区分全文与具名范围。没有擅自重建共享索引，没有声称图谱已到最新HEAD。

N04原41产品候选源文累计阅读历史保持，不把新增消费者及核心同行文件加入该分母。本轮新增的是正式写回/重载/取消恢复场景，**N04仍场景部分完成**；缺少真实Mongo、重启恢复、多DAO/CAS组合、Cluster/HA/弱网/长容量，原17环境skip没有因此关闭。未重新计算全仓百分比或把34通过当全域完成。

## 用户要求的流程更新

共同coding已有“可补中文注释”，本机review入口原来仅允许文档。本轮把它明确成review应补必要的中文说明，按实际契约写职责、调用顺序、锁/资源所有权、失败和结果未知；不逐行翻译、不写未实现承诺、不改行为，显式只读时只记录建议。仓库coding、bugfix及共同组合清单更新，本机完整skill包同步；本机review入口保留独立目录，不另造仓库副本。

普通review/bugfix/开发收尾改为**本地编译通过满足构建交付条件，不等待或轮询GitHub CI**。保留有意义的本地行为证据，编译不能将确认缺陷写成已修。release仍须单独授权，本轮未发版。

## 停点与下一入口

先由bugfix处理NC-31（本轮未获行为修复任务）；未修继续review时接续**多DAO迁移/并发CAS淘汰与正式消费重载→N04具名缺口收口→N05路由/mirror增量**，真实资源/进程恢复受限项保留，不回退已整理的核心全域。

运行预算不靠测试数量折算。本轮未泛化验收新增playerowner修复或重开历史开放项；这些上游变化属于另一线，不能仅因pull/build通过关闭其所有边界。

## 收尾上游同步与受影响消费补验

推送前远端到`64529e3929d054fbdcbf53a8bd0b051991ec1348`，包含另一线v1.19.2发布、nocoll feature、codegen进程cwd/子进程树修复和playerowner修复。保留其提交与发布事实，本轮没有发版；共享skill与bug导航冲突按本地验收规则和双方记录整合，保留v1.19.2速查表。

nocoll修改了DAO模板/gen，但有集合的生成Migrate/Restore分支仍保留；读取相关diff后，重新编译正式DAO CLI、生成本轮有集合消费者并执行11叶子，仍3反例/8控制、vet0。最新全仓build0、根包0；[合并后消费者](evidence/noncore-review-20261004-20/synced-consumer.jsonl)、[退出/基线](evidence/noncore-review-20261004-20/synced-exits.json)、[构建/根包](evidence/noncore-review-20261004-20/synced-checks.json)。原34定向证据仍属341，未将它们改写为最新重跑。

这不是nocoll、playerowner或子进程树的再次全量审计；新W-2026-10-04-08窄交错仍待后续具名分流，05～07按上游已登记RR的状态保留，不能把本轮build通过当成所有Wanted已关闭。NC-31在新模板正式消费里仍成立且未修。保持N04多DAO/CAS接续范围，新的增量/Wanted另列，不因同步循环扩成全仓review。
