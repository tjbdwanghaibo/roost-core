# 2026-10-05 接续 review：N05 权威回填与失败重订阅

起点be4eb0fa，fetch/pull --ff-only到 `40d89ac68b8a2247173e94ef55c02fdf23da1d6f`；工作区原本干净。与上轮相比无新的 docs/bug、docs/bugfix 或 canonical skill 更新，没有新 Wanted 待分流；三个 skill 包与本机镜像一致。按已授权“修复，并继续review”完成本批，不等待GitHub CI，不发版。

## 问题与场景

| 范围 / 承诺 | 新证据 | 结论 |
| --- | --- | --- |
| 权威原始结果完整请求键 | Monotonic/Linearizable × tenant/entity/scope/policy；原请求键及foreign L1/L2副作用、拒绝后合法加载 | 八个读取+一个正式消费反例红→绿，P2 [NC-35](../bugfix/RR-20261005-NC-35.md)已修 |
| 最终准入结果达到minVersion | 较新epoch/version2保留，旧epoch/version4权威被准入拒绝；合法同epoch恢复 | 两个读取+一个消费反例红→绿，P2 [NC-36](../bugfix/RR-20261005-NC-36.md)已修 |
| L2发布跨越绝对期限 | 真实调用L2.Set钩子等待信封截止时刻，最终读取不命中，新版本无期限结果恢复 | 两个反例红→绿，旧 [RR-20260913-08](../bugfix/RR-20260913-08.md)追加残余补修，不新编号/T |
| 正式Sync接收回填 | BindSync/Replicator/Syncer的gap、epoch、schema三类回填；loader错误透传与合法重投递 | 共六个新增正式消费叶子（含上面两红），验证full内容/版本/schema和缓存保留 |
| Assembly订阅失败重试 | 第二Subscribe失败释放第一订阅，重试两个订阅到位，重复Start/Stop幂等且最终active=0 | 一个新增正式叶子通过，无新确认bug |
| 停止交错 | 复用既有Storage失败重试、排队取消、finalizer在途导致Stop超时后再次Stop释放全部订阅 | 没有重算新增；Replicator取消订阅不提供在途handler drain，未把它宣称为已解决 |

合计 **19新增正式叶子**：12公开读取反例、6正式消费（2反例/4原本通过控制）、1重订阅控制；错误后的副作用与恢复也在这些场景内，不拆成额外覆盖计数。没有修改核心执行池、持久化协议、另一线App feature或生成器实现。

[学习/实现机制](IMPLEMENTATION-AUTHORITATIVE-SNAPSHOT-POSTCONDITIONS.md)解释入口身份、最终L1后置条件、时间敏感I/O与生命周期。修复只增加常数次比较和当前时间读取，无新锁/缓存/重试；未跑性能基准。

## 本地验证与证据

[红绿/矩阵](../bugfix/evidence/noncore-bugfix-20261005-14/README.md)：相关包含App/Kit的race **733叶子 / 800通过事件，8 skip**；根包14；全仓build、相关vet、glsvet通过。正式DAO/Entity生成的periodic/on_change两个消费叶子race通过。上游生成器整合另独立跑 `go test -count=1 -json ./codegen/...`，620叶子/666通过事件，10环境skip；不是race或真实生成服务启动验收。具名skip保留，不能将不同运行数相加折算全仓覆盖。

首次生成消费调用误在脚本外再次Tee到脚本自写的日志，引发文件占用；这是本轮取证命令错误，随后移除外层Tee，使用新的独占scratch完整复跑exit0。没有修改产品门禁或把环境失败当产品红。脚本本身仍是原Bash双模式生成检查的Windows路径等价运行，差异与入口留档。

## 图谱与范围

Tier2 Verify，最近项目roost-core / D:/whb_s/cube-core；ready 32285 nodes / 209828 edges，generation `2026-09-30T11:58:14Z`，未刷新共享索引。图谱先定位Snapshot/Assembly及其测试，再检查调用方向和精确LoadAuthoritative片段。15材料路径及三个相关scope的 [coverage](evidence/noncore-review-20261005-24/coverage.json)无记录缺口，但生产metadata_changed、新测试not_tracked均用当前精确源码补证；[最终LF源码摘要](evidence/noncore-review-20261005-24/source-hashes.csv)。

涉及生产函数位于entity快照/record、remoteentity syncer/assembly/assemble/Manager读取、AtomicLocal与通用mirror共8文件；测试路径仅证明声明场景。没有将15材料路径计为完整产品文件覆盖，没有从同名/跨包误边作权限或无调用结论。旧generation/无缺口不证明穷尽；不重开Nest/Sync/DataEngine全域审核。

## 首次feature整合边界（40d89ac6）

当前拉到的66文件大批变化包括App锁、codegen装配/预算、静态PlayerOwners与global租约API删除。不能沿用上轮“只有第1笔、静态绑定未实施”。[App方案](../feature/APP-SINGLETON-LOCK-2026-10-05.md)§13已记录1/2/2b/3/**3b**实施；首页仍将3b列未实施，阅读时以后续记录/40d89ac6实际删除为准。本轮不改另一线规则源或方案历史。

[静态绑定方案](../feature/PLAYEROWNER-STATIC-BINDING-2026-10-05.md)记录PlayerOwners静态绑定已实施f051e24a，赠礼/match的后续路由步骤仍待实施；App方案4/5及真实进程演练没有因本次合并而完成。W-2026-10-04-08仍按静态绑定新前提归“简化后不适用”，不是本批修复的bug。

本轮相关App/Kit race、全仓build和codegen单测是独立运行；global/etcd/mods API删除的定向race133叶子/141事件通过、2Cluster环境skip，见 [补充矩阵](../bugfix/evidence/noncore-bugfix-20261005-14/upstream-api-summary.json)。作者真实Redis/Cluster/生成game-demo运行记录只是接手证据，本轮未重跑，不将整合状态写成feature全链独立验收。

## 进度与下一停点

N05 **场景部分完成**：本批关闭两个新增P2和旧RR08残余，补齐具名gap/epoch/schema、失败后重订阅/最终资源收口控制；不计completed/15、不重算全仓百分比。19场景不是19个全新产品入口，历史已测项不双计。

下一优先新Wanted/变更，再明确真实transport停机/旧handler在途交错的具名契约与可验证入口，补N05剩余场景清单后按原非核心计划继续N06。repair miss/过期时nil仅表示loadErr为空，不证明目标视图存在，仍作观察，不凭缺少业务ACK前提改协议。跨节点L2删除水位、真实broker ACK/retry/断线恢复、时钟回拨、HA及长期容量保持留项；Mirror DTO方案仍未实现，无新的全部review完成日期承诺。

## 末次同步：赠礼静态sid路由（64acd782）

提交前再次fetch到5bdac773/64acd782，15文件增量已正常rebase整合。两份方案现已将赠礼路由第4笔标已实施；前节“第4笔待实施”保留首次40d89ac6时点，第5笔真实进程演练仍未独立验收。原15证据源码按LF哈希逐一核对未变化，因此19新场景与相关733 race矩阵保持有效；没有把赠礼实现算成本轮修复或N05新增覆盖。

合并后再次跑全仓build、根包14与codegen/internal/roost vet均exit0，[结果](../bugfix/evidence/noncore-bugfix-20261005-14/latest-checks.json)。原codegen620普通叶子对应40d89ac6，不能当成64acd782全包重跑。最新赠礼生成消费通过正式CLI独占生成game-demo并替换本地core检验，运行入口与结果见[末次生成验证](../bugfix/evidence/noncore-bugfix-20261005-14/README.md#末次赠礼生成消费)。不等待GitHub CI，不发版。

本机原始生成依赖的完整build仍触发历史B35 genproto重复包；89消费race叶子/2真实Mongoskip通过不掩盖该失败。仅独占夹具显式整理到缓存中的已拆分genproto后另验build/vet；结果与原始失败分列，不改框架依赖/模板，不新造重复RR。首次CLI默认依赖解析因离线缓存缺发布版mod失败，随后用已有skip-deps+本地replace；不算公开tag/默认网络依赖解析验收。

最终整理依赖的生成工程89消费race叶子/2Mongo skip、全仓build/vet通过；中间缺socket权限的四项loopback失败及相同权限恢复后全绿留在证据，不改样本/门禁。没有从消费测试通过推导跨服赠礼全部状态/真实HA已审完。

## 推送重试同步（10e2e0ea）

首次push遇远端前进，未强推；正常fetch/rebase整合c493a791/364b763c/10e2e0ea。CHANGELOG追加冲突手工保留双方条目，bug/bugfix/交接索引和T-216/217核对保留。最新包含赠礼接收端phase→Topic对应关系检查、生成etcd前缀结尾斜杠与第5笔演练文档；关键增量按当前diff/源文补证，[补充coverage](evidence/noncore-review-20261005-24/delivery-coverage.json)。模板图谱Handoff查询0节点不作缺实现结论，新测试not_tracked/旧metadata用源码和正式生成消费补证。

原15 N05证据源码LF哈希再次一致。本轮没有重做无行为变化的733矩阵，最新核心全仓build/根包14/codegen vet与etcd前缀正式新测试exit0：[delivery-checks](../bugfix/evidence/noncore-bugfix-20261005-14/delivery-checks.json)。

又在新独占目录生成最新game-demo，显式沿用已留档genproto整理；**91消费race叶子/104事件通过，2Mongo skip，生成全仓build/vet exit0**。原89是64acd782消费，91为10e2e0ea增量，多两项是作者新phase/topic回归，不计本批19新增正式叶子。[最新消费](../bugfix/evidence/noncore-bugfix-20261005-14/generated-demo-checks.json)、[前一消费](../bugfix/evidence/noncore-bugfix-20261005-14/generated-demo-64-checks.json)。原始依赖失败不因此关闭。

作者现将App方案第1～5笔标已实施并报告真实进程演练，覆盖sid双进程、失锁、暂停/恢复等；这取代前段“第5笔未实施”的旧时点，但本机仅独立验证上述具名编译/消费，没有重跑作者真实进程/Redis演练，更不等于HA全验收。N05仍部分完成，Mirror DTO和既有外部留项不受影响。最终本地验证后提交推送，不查询/等待GitHub CI，不发版。

### 最后etcd停机增量（fdcd8605）

再次正常整合b67d5945/fdcd8605，新增etcd注销将LeaseNotFound视为已达成、取消停止过程中才完成重注册的新keepalive，以及App普通Mod Stop错误仍释放单实例锁的控制测试；按当前diff与具体错误/重试/重注册测试核对，[coverage](evidence/noncore-review-20261005-24/etcd-coverage.json)。本轮未修改这些实现、未复做作者红测试，不将整合当完整外部验收。

独立 `go test -race -count=1 -json ./app ./etcd/driver` **162叶子/173事件通过**，核心全仓build、根包14、两包vet通过：[etcd-checks](../bugfix/evidence/noncore-bugfix-20261005-14/etcd-checks.json)。真实etcd测试有integration build tag，本次普通命令不包含它，0skip不代表真实etcd已验证。赠礼/生成模板未变，前节91消费矩阵复用，不重跑不相加。原15N05源码身份不变、两个新P2和旧过期残余的状态不变；不等待CI，不发版。

### 交付范围冻结与最后整合（f8bb0261）

其他模块仍持续提交，最后正常rebase整合021454d5/c441fbdd/ac5acfbe/4bea90d0/f8bb0261：包含静态PlayerOwners卸载测试、认证与登录共用server_id claim、赠礼退款重启预算及注释/观察交接。原15N05证据源码哈希仍一致；本轮继续以NC-35/36/RR08及具名N05场景为交付范围，不临时接管另一线整个新模板专项。

最终全仓build、根包14、codegen vet exit0：[last-checks](../bugfix/evidence/noncore-bugfix-20261005-14/last-checks.json)。这些新模板**已整合，未在本轮逐项独立审查/生成消费重跑**，前面的91消费明确对应10e2e0ea；不宣称是f8bb0261全部生成行为验收。另一线未改观察按App方案§13接手，后续review按新Wanted/变更和既有N05→N06优先级推进。既有原始依赖B35与真实外部留项继续开放，不等待CI、不发版。
