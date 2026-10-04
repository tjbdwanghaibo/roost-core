# N04：迁移准入修复、多 DAO、CAS 与进程恢复

2026-10-04 开始，跨午夜于 2026-10-05 完成。按用户“修复，并继续 review”执行 roost-bugfix / roost-review / roost-coding；本机 skill 三个仓库维护包逐文件 hash 与正本一致，没有另建副本或重写 skill。

## 本轮结论与基线

[NC-31](../bug/RR-20261004-NC-31.md) 已修复、声明场景验证，未发版。本轮后续六项迁移消费 review 没有新增确认缺陷。**只说明这些具名场景，不是 N04 / 整仓无 bug 证明。**

干净 main 从 `edf85be962d9faf820f2899ecd909134c883942e` fetch + pull --ff-only 到 `3127d37ce113b887038366f9289218c32729dbbc`；唯一增量为 PlayerOwner 状态机设计文档，迁移源码仍重现 NC-31。修复及验证基于 3127 + 本轮工作树；[当前源码摘要](evidence/noncore-review-20261004-21/source-hashes.csv)保存字节身份。另一线核心专项和上游已发布版本沿用原记录，本轮没有重验其完整矩阵或发版。

## 修复验收

预提交校验复用 Mongo BSON/ID 契约、目标 DAO 的 RestorePersisted 和解码 ID；仅处理尚未发布候选，不创造并行模型。手写缺能力返回 unsupported；生成的正常路径、旧 payload schema、int32 ID、错误透传和取消后恢复均有对照。[根因、兼容性、红绿](../bugfix/RR-20261004-NC-31.md) · [修复证据](../bugfix/evidence/noncore-bugfix-20261004-11/README.md)。

正式新回归 12 叶子通过；原生成消费 3 红变为 11 全绿。五包 race 421 叶子通过、1 `TestNestWALCrashChildProcess` helper skip（父用例实际执行子进程）；根包 14 通过，build/vet/glsvet 0；Kit integration-tag 仅编译通过。通过数量不能替代场景解释。

## 新增有界 review：六项全部实际执行

正式 CLI 生成 profile / inventory 两个 schema3 DAO，公开 Repository / Runner、实际文件 WAL / Projector、正式 MongoStore；后端使用 mongotest。完整消费者 17 叶子 race/vet 通过，其中下面六项为增量。

| 场景 | 预期 / 实际 | 结论 |
| --- | --- | --- |
| 两 DAO 都为 schema1 | profile v7→8 / inventory v9→10，两个提交，整聚合重读后才发布；新 Manager 无迁移器重载一致 | 通过，不把 DAO 逐笔写误称聚合迁移事务 |
| 后序 DAO 目标字段错误 | 前序有效迁移保留；坏 inventory 不提交、不发布实体；正式 CAS 修正源数据后重试，只迁移 inventory | 通过，部分持久迁移可恢复，不回滚有效前序 |
| 后序步骤返回错误 | 原步骤根因可 errors.Is；前序已提交，不发布；修正源数据重试后聚合一致 | 通过，明确错误不代表所有 DAO 都没变化 |
| 竞争者先升级到目标 schema | gate 阻住旧迁移投影，正式 CAS 写 score99/v8/schema3；旧迁移 conflict 被淘汰且 WAL 结算；重读保留99 | 通过，旧快照不能覆盖较新权威值 |
| 竞争者仍写旧 schema | 旧迁移被淘汰后全聚合重读，按 v8 新基线重新迁移到 v9，已完成 inventory 不重复迁移 | 通过，三次读取预算中的第二次可收敛；不等于任意持续竞争都会成功 |
| strict durable 后、投影前进程终止 | 等 CommitSystem 返回及投影 gate，强杀本轮启动的测试子进程；重开同一真实 WAL，检查仅一笔 strict 迁移，Flush 重放，新 Manager 无迁移器加载 score42/v8，unacked=0 | 通过，真实文件 WAL 进程恢复；后端重新建立相同未投影旧文档，不证明 Mongo 持久性 / HA |

并发测试用明确通道排序，不用概率 sleep。竞争业务写走正式 Store.Project/CAS；没有直接 Seed 偷改版本来制造冲突，也没有删坏 WAL 让测试通过。进程只终止本用例自己创建的子进程，等待退出后才开新 WAL 实例。

## 设计与性能评估

预提交校验补上“可以迁移”与“可以持久加载”之间的契约；框架已有装载能力，复用成本和接口面小。逐 DAO 的 schema 迁移不是业务跨 DAO 原子操作：允许持久状态处于部分升级，但不发布半加载 Entity，失败修正后可从持久 schema 接续。

CAS 淘汰返回成功只表示这笔系统记录可以结算；Repository 必须重读，不能发布内存候选。现有三轮读取 / 至多两轮迁移有界，持续竞争会返回 ErrMigrationConflict，由调用者重新发起加载，不能放宽版本或无限重试。

新增解码在冷迁移路径上；每 DAO 提交/投影及完整快照重读仍有成本，多 DAO 首次加载应在升级预算里考虑。没有同机基准或真实 Mongo 延迟测量，不给性能收益数字。

## 图谱、源码与证据限制

Tier2 Verify；最近项目 roost-core、根 D:/whb_s/cube-core，ready 32285 nodes / 209828 edges，generation `2026-09-30T11:58:14Z` 早于基线。MigrationRunner 文件 / PersistedDaoLoader / Kit 集成及 nestwal 查询相关页完整；LoadEntity 双向 depth2 trace 两页已翻完。trace 含明显跨包误边（例如 NewMigrationRunner→mirror.New）及接口/receiver 漏边；调用结论以当前 import/源码/正式编译为准，不把 zero caller 当无人调用。

材料路径 coverage 显示 metadata_changed / 新路径 not_tracked，已回读当前源文或对应关键范围；旧图谱无 recorded gap 不证明最新完整。没有重建共享索引、停止他人服务或宣称索引已到 HEAD。[coverage 与源文范围](evidence/noncore-review-20261004-21/README.md)。

两次夹具更正保留说明，不登记产品 bug：最初把 int32 ID 误设为负对照，核对 documentInt64 后改为正常控制并重新跑旧 runner 得到可信 5 红/3 控制；最初用 DurableLSN 判断 strict Append，实际它是 Enqueue 票据水位，改用 CommitSystem 返回事件 + 重开后的真实日志验证。初次 crash fixture 的失败不计产品红。

## 进度、具名留项与下一入口

N04 历史 41/41 候选源文累计阅读保持；本轮不往该分母塞核心同行文件/测试。多 DAO / 单次并发 CAS / 此进程终止点的 WAL 恢复已从“未验”转为上述部分场景通过，**业务场景仍部分完成，不计 completed/15**。

- 本机可补：正式生成嵌套字段与 schema 类型变化；持续竞争达到 ErrMigrationConflict 的生成消费；读事务重试的真实 SDK 集成依赖真实 Mongo。
- 外部留项：Mongo replica set / cursor 网络失败 / 未知提交、Redis Cluster/HA、真实部署强杀/断电矩阵、长期容量、historical 17 环境 skip。
- 下一：N04 上述可执行缺口及场景清单收口，随后 N05 路由/mirror增量；W-2026-10-04-08 的最新上游归类见下方，原 N01～N03 外部/关闭留项保持。

[进度](PROGRESS.md) · [机制学习](IMPLEMENTATION-DAO-MIGRATION-HYDRATION-AND-REFHMAP-SCHEMA.md) · [复跑脚本及事件](evidence/noncore-review-20261004-21/README.md)。以本地编译及实际验证收尾，未查询/等待 GitHub CI。

## 交付前上游增量

fetch发现 `42059469` 仅改 Wanted / 两份 PlayerOwner 设计文档，所有本轮源文与测试路径无远端改动，3127 的行为验证仍适用，不重复无影响的测试。整合保留该提交，本轮没有实施其静态绑定方案。

上游依据维护者“玩家固定绑定 sid、同 sid 正常一个进程”重新将 W-2026-10-04-08 分类为“简化后不适用”，见[静态绑定方案](../feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md)。这改变后续设计审查的前提；方案明确**代码未修改**，不能把设计分类改动当成现有实现已修复或独立行为验收。后续 N05 / 生成接入按该新方案与实际落地进度继续，不继续沿动态玩家迁移假设扩造机制。本轮只核对增量性质，未审其完整设计/五项待决策。
