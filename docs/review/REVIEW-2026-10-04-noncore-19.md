# 非三大核心第十一批：RefHMap schema 并发与正式迁移消费

2026-10-04；main 从 `25ef4c1e` fetch/pull --ff-only 到 `e7027a65c8a3b7649837a61c50a29d999ba32636`。新增提交仅 C01 文档：另一线报告 1h 长稳通过，本机没有复跑，保留其既有资源/负载边界。开工工作树干净。仓库 roost-coding/bugfix/optimize skill 包与本机副本一致，无需同步修改；gh 未安装，未查询在线 CI。

上一批 NC-26～29 与 RR-20261004-01 已关闭，未重复修旧实现；修复旧索引表 NC-26～29 状态遗漏。接续上一停点 schema/Patch/fullSet/未知恢复，并补正式生成迁移消费；不重新 review 另一线 Nest/Sync/DataEngine 主链。

## 问题和本轮修复

新 [NC-30 P2](../bug/RR-20261004-NC-30.md)：读取清理 registry 后另一个 schema 发布新键，旧 Set/Delete 丢失 child 登记且返回 nil，TTL=0 留永久孤儿。修改前两个真实 Redis 反例失败、四控制通过，同基线 overlay 复跑一致；本轮明确修复授权下复用 registry guard/Eval 实施，12 新正式场景全部转绿。[实现/兼容](../bugfix/RR-20261004-NC-30.md)。无其他新增确认 RR。

## 本轮有界矩阵

| 入口 | 实际执行 | 结论与限制 |
| --- | --- | --- |
| registry Set/Delete | 12 正式场景；全量发布、Patch 新键、顺序清理、错误/未知、旧 registry、重复 miss | 确认并修复清理竞争；不是值 CAS/完整生命周期防 ABA |
| Patch/Set 顺序 | 两种先后、不同叶并发、同叶竞争 | 全量最后写覆盖、Patch 最后写可见；不同叶均保留，同叶最终是其中一笔 |
| 未知结果恢复 | Patch 已应用/未发送读回、未知后较新 Set、未知 Set 后显式 Delete | 原始错误不吞、不自动重放；不同状态由读取确认，测试注入不是实际弱网 |
| schema 兼容 | nil 分支清理、旧读者忽略新字段、新字段缺失默认、旧 Patch 保留新字段、Delete 后 Patch 拒绝 | 同名新增/缺字段场景通过；类型变更/字段重命名/滚动部署不是本批已验范围 |
| 既有边界实证 | 非 Pipeline adapter 读 root 后并发 Set、精确服务端过期旁支、Stale 检查后另一写 | 3 个观察夹具通过：混合版本、旁支零值、晚到旧写；不计快照/CAS/完整 TTL 正确性通过 |
| 正式 Codegen 迁移 | CLI → 独立模块 → 生成 RestorePersisted；成功、当前/较新 schema、首/次步失败、缺路径、坏输出 | 7 叶子通过；失败不发布部分 DAO/版本，原输入不变；不等于数据库 schema 已持久更新 |
| 正式 Redis/Cached 消费 | Set/Delete registry 变化，直接及 Layered 两种接入 | 4 叶子通过；错误透传、拒绝不动 L1，显式读回后 Delete 清理新键 |

新增 review overlay 合计 16 叶子；消费者 11 叶子；最终六相关包 221 叶子 pass/0 fail/0 skip、race/vet 通过，KitRedis 无测试不计 skip 叶子。上轮扩展消费者 17 环境 skip 的历史留项未因本批数字而关闭。[修复证据](../bugfix/evidence/noncore-bugfix-20261004-10/README.md) · [review 证据](evidence/noncore-review-20261004-19/README.md)。

## 图谱、源码与进度

Tier 2 Verify；project roost-core，ready 32285 nodes/209828 edges，coverage generation `2026-09-30T11:58:14Z`，早于 HEAD。精确 ref_hmap.go、migration.go、gen.go、两个模板的搜索已处理相关页（均无 has_more）；广义 BM25 只作候选发现，不据其未翻完的排名做否定结论。两个方向 trace 存在 receiver 合并及 heuristic 跨包误边；实际模板、当前源码和正式生成编译是调用证据。初始猜测的三个不存在文件不作证据。coverage 对全部 18 材料路径无记录 gap，但 metadata_changed/not_tracked，按当前源码全文或具名范围补证；不声称索引已刷新。

N04 当前候选仍 **41**：40 相同 hash 复用第18轮，ref_hmap.go 全文和当前 diff 补证；新测试不扩大产品分母。Codegen 模板读取只限 redis 装配/Patch、schema/hydration/Marshal 及生成入口，Tracker 只限版本/清 dirty；这些不宣称 Codegen/核心重审完毕。[41 清单/18路径 hash/coverage](evidence/noncore-review-20261004-19/README.md)。

N01～N04 仍场景部分完成，不计 completed/15；跨域50～90有效小时组织估计未重算。下一优先 **正式迁移的 Repository/持久确认/重载与失败边界（复用另一线核心证据，仅查接入）→ N04 缺口收口 → N05 remote 路由/mirror 接入增量**。真实 Mongo cursor/partial bulk/unknown commit、Cluster/HA/弱网/长期容量仍留项。[进度](PROGRESS.md) · [学习](IMPLEMENTATION-DAO-MIGRATION-HYDRATION-AND-REFHMAP-SCHEMA.md)。

## 提交前上游审计与下一批待修

收尾fetch发现七个新提交，到origin/main `c888223f`：v1.19.0发布、grpc直接依赖归类（版本未变）、根包门禁/webroute补修、文档/skill与三组独立审计。已整合，保留审查/红绿原基线e7027a65；18材料路径和N04产品hash无上游变化，不冒称原回归覆盖新增webroute实现或发布tag。CHANGELOG中NC-30保持Unreleased，v1.19.0由另一线发布，不含NC-30。

| 上游新增待修 | 范围 | 本轮接手状态 |
| --- | --- | --- |
| [RR-20261004-02](../bug/RR-20261004-02.md) P2 | 过期L1否决L2、远端成功却报stale | 原审计有红与修复约束，本轮只接手，不冒称独立复测 |
| [RR-20261004-03](../bug/RR-20261004-03.md) P2 | Patch只续路径、Get返回缺旁支的部分记录 | 本轮观察实际看到相同零值/ok=true现象，追加关联；不把“已记录边界”当用户接受而忽略 |
| [RR-20261004-04](../bug/RR-20261004-04.md) P3 | loader回填stale使读取失败 | 接手原审计约束，未复测/修复 |
| [RR-20261004-05](../bug/RR-20261004-05.md) P3 | mongotest非sparse缺字段唯一索引过宽 | 接手，原真实Mongo证据未在本机复跑 |
| [RR-20261004-06](../bug/RR-20261004-06.md) P2 | etcd失败Campaign先cancel再Revoke | 接手原真实etcd证据，本机未跑/修复 |
| [RR-20261004-07](../bug/RR-20261004-07.md) P3 | Bus/NatsMod取消后重试无法最终关闭 | 接手，原callback重入疑点一并作为约束 |

这是本批修复/矩阵完成后才到达的另一线新范围，六项未在本批实施，不把当前scope“无其他新RR”外推为仓库无待修。下轮顺序改为 **02/03/04缓存语义→05索引→06/07关闭与lease→正式迁移接入/N04收口**，不直接跳到N05。NC-30与RR-03根因不同（清理清单 vs TTL/完整记录），不能用NC-30绿色关闭03。

仓库新roost-bugfix包已备份并同步本机（备份在workspace .tmp/skill-backups/20261004-noncore19）；增加“每批跑根包”的规则，整合后实际`GOWORK=off go test -count=1 -timeout=120s -json .`已通过，[结果](../bugfix/evidence/noncore-bugfix-20261004-10/root.jsonl)/[退出](../bugfix/evidence/noncore-bugfix-20261004-10/root-exits.json)。根包不代替此时尚未执行的六项行为复现。首次接手六项的历史状态保留，以下最终同步为准。

## 最终同步与合并验收

后续远端到d49f02f1，另一线已修复并推送RR-02/03/04/05/06。保留他们的代码、红测与文档，NC-30只增加registry Set/Delete guard；RefHMap生产源码与替身自动合并，手工检查两种脚本的参数位置、Patch全部声明键续期及Get缺子hash判miss。CHANGELOG保留双方Unreleased条目，RR-03原报告仅追加本轮关联；T-208已被etcd修复占用，本轮NC-30顺延T-209。

合并源码上重新执行：六相关包241叶子pass/0fail/0skip（含NC-30的12正式、RR-02/03/04正式回归），race/vet通过；16 review、11正式生成消费者、根包12叶子全部通过。原旁支过期夹具用于保存修前现象，另增当前夹具明确断言RR-03修后整条miss，Run-Verify默认使用当前版，其余15场景不变。[独立合并证据](../bugfix/evidence/noncore-bugfix-20261004-10/merged/README.md)。旧221/原观察/18摘要/41清单是早期快照，不能继续称所有摘要与当前树相同；合并后19材料摘要另存，不改写原证据。

RR-05补跑本机`GOWORK=off go test -race -count=1 -timeout=120s -json ./mongo/mongotest`，115叶子pass/0fail/0skip，[结果](../bugfix/evidence/noncore-bugfix-20261004-10/merged/mongotest.jsonl)/[退出](../bugfix/evidence/noncore-bugfix-20261004-10/merged/mongotest-exits.json)，不是实际Mongo对照验收；RR-06仅接手上游修复和真实etcd记录，本机没有etcd资源，未独立验收外部效果。图谱仍旧generation，新增TTL测试及当前RefHMap/Layered按源码/差异补证，错误猜测的readthrough.go不存在且未作材料；不声称全域重审。

截至最终同步的待修是RR-07，先其关闭/重试/callback约束，再正式迁移Repository/持久确认/重载接入和N04收口，随后N05。N01～N04仍场景部分完成，不以回归通过冒认全部review完成。GitHub公开API读到d49f02f1最近三个CI为pending/in_progress，未见失败结论、不等待CI，也不称在线CI已绿；推送后再读一次快照。

## 最后增量同步

远端随后到b9625f4f，RR-07也已由另一线e5aea173实施，以上“07待修”保留为同步时点。最后两提交没有改变本批19材料路径；格式化恢复本批Go源码LF后摘要再次相符，241相关/16 review/11生成消费结果仍对应同一内容。追加`GOWORK=off go test -race -count=1 -timeout=120s -json ./bus ./kit/nats ./nats/driver ./servicerpc`，114叶子pass/0fail/0skip；对应vet通过，最后根包12叶子pass/0fail/0skip。[生命周期事件](../bugfix/evidence/noncore-bugfix-20261004-10/merged/lifecycle.jsonl)、[退出](../bugfix/evidence/noncore-bugfix-20261004-10/merged/lifecycle-exits.json)、[vet退出](../bugfix/evidence/noncore-bugfix-20261004-10/merged/lifecycle-vet-exits.json)、[最终根包](../bugfix/evidence/noncore-bugfix-20261004-10/merged/final-root.jsonl)。这些是普通包回归，不包含真实connected NATS的drain超时矩阵。

另一线新增[W-2026-10-04-02](../bug/WANTED.md#w-2026-10-04-02natsmod-在连接-drain-超时后保留已硬关闭的-assembly重试永远拿到-errconnectionclosed)，报告连接硬关闭后NatsMod仍保留Assembly，重试返回ErrConnectionClosed。本轮分流为**再观察、待独立真实NATS红证据**：实现侧给出了可复現链与修复候选，但本轮无connected NATS超时复现，包内/无连接回归绿不足以判非问题或已修；不创建未获独立证据的新RR，不改其实现。下一优先复现和分流此Wanted，再迁移接入/N04收口/N05。RR-02～07已实施不等于每项真实资源边界均被本机验收，更不等于非核心15单元完成。
