# 独立复审核对：合并事实、修复遗漏与流程改进

日期2026-10-04；更新前7fbdc735，主体核对基线d3e69336（仅release文档/manifest变化，产品实现与7fbdc735相同），收尾整合aa35d4a1，见末节。用户确认范围为三组NC独立复审与RR-20261004-02～07、W-2026-10-04-02。只核对、记录并改进规范/使用文档，本轮没有修改产品行为或正式测试，没有发布。

**结论：另一agent登记的六项缺陷成立；上轮“已整合”作为Git合并事实正确，但不等于所有组合及真实资源场景验收完成。** 六个修复提交都是7fbdc735的祖先，[逐项提交证明](evidence/noncore-audit-followup-20261004/merge-status.json)。本轮用相同正式回归配v1.19.0历史实现overlay，四项重现28个行为失败/4控制通过；当前源码对应32场景全绿，再加etcd清理3项和Kit停止重试1项，共36叶子pass/0fail/0skip，race/vet与根包通过。[可复跑证据](evidence/noncore-audit-followup-20261004/README.md)。这不冒认本机已做真实etcd、Mongo、connected NATS或HA验收。

三个原报告覆盖10-03 NC-01～04与10-04 NC-01～29（33个旧修复编号）：[第一组](REVIEW-2026-10-04-nc-audit-1.md)、[第二组](REVIEW-2026-10-04-nc-audit-2.md)、[第三组](REVIEW-2026-10-04-nc-audit-3.md)。第一组11条修复均定向有效，不能把另外六项发现解释为全部旧修复错误；这些问题包含修复引入的回退、旧缺陷被扩大，以及原有替身语义缺口。

## 六项核对与为什么漏掉

| 项目/当前修复 | 原缺陷与成立证据 | 前轮缺口及归因 |
| --- | --- | --- |
| [RR-02](../bug/RR-20261004-02.md)，3bce736c已包含 | NC-14/15组合：过期或禁用窗口的L1旧版本永久否决L2；L2已接受写但调用方仍收到stale。历史8反例红、当前绿 | 只验证“拒绝旧回填”，未交叉L1有效/过期/ttl=0、删除后以低版本重建及远端已应用。把内存仍有值当成它仍有裁决资格 |
| [RR-03](../bug/RR-20261004-03.md)，5c1647c6已包含 | NC-18扩大旧问题：Patch续路径，旁支先过期；Get以held=true返回从未写过的零值子记录。历史真实Redis5反例+解码1反例红、当前绿 | 我把“旁支保持原TTL”记为范围边界，却没有追到整体Get的错误值。这不是用户接受的设计。原触发修好了，不代表整个记录保鲜/完整性成立 |
| [RR-04](../bug/RR-20261004-04.md)，fa885409已包含 | NC-15使Local/Redis返回拒绝后，loader路径把填充的stale/conflict当读取失败；同函数L2回填分支处理不同。历史5反例红、当前绿 | 新错误返回只检查生产者，没有穷尽L2命中与loader两条消费者分支；未检查返回值/held/error的完整组合 |
| [RR-05](../bug/RR-20261004-05.md)，e7c566ac已包含 | NC-27留项：mongotest忽略Sparse、缺唯一字段直接跳过，放过真实非sparse Mongo拒绝的数据。历史9反例红、当前绿；实际Mongo对照引用另一线 | 明知替身更宽松却只列范围说明，未登记会令业务测试假阳性的缺陷。正常唯一值/建索引验证不覆盖missing/null/sparse |
| [RR-06](../bug/RR-20261004-06.md)，c57b247b已包含 | NC-11失败分支先cancel再Close；本机依赖etcd client v3.7.1的Session.Close从session context派生Revoke，已经取消即不能删lease。当前3项SDK形态单元绿，实际服务端红绿引用另一线 | 验证caller返回、Done关闭与长期session分离，未验证候选键/lease真正消失、下一候选能接管；“未证明即时删除”掩盖了比修前退化的释放能力 |
| [RR-07](../bug/RR-20261004-07.md)，e5aea173已包含 | NC-09保留Kit引用，但Bus丢pool并把第一次超时缓存为终态；重试即使handler结束仍失败，Assembly无法关闭。当前Kit组合回归绿；旧代码缓存路径及另一线红证据一致 | 跨层“保留资源→可以重试”的假设没有核对Bus状态机，只看第一次超时返回和引用还在；没有走工作结束后再Stop的恢复闭环 |

四项历史红测试使用当前正式回归与v1.19.0的5个历史实现/替身文件，关闭仅属于NC-30的新API测试文件以保持历史编译兼容，未放宽断言。不是重建整个v1.19.0发布工程，也不是所有33个修复的再次全量审计。28失败中有原故障、邻近控制及补修分支，不把28解释为28个bug。etcd SDK实际源码确认Close用session context；RPC/Bus/Kit的当前与旧代码作具名状态链补证。图谱Tier2、generation仍2026-09-30，21路径metadata_changed/not_tracked；snippet旧行号会截断新函数、receiver/heuristic误边不作调用证明，实际源码补全。

## 我应承担的判断与验证问题

1. **把局部修复当成组合契约完成。** Stale从“忽略”变为“返错”、Patch从“叶TTL”变为“祖先TTL”、Kit从“直接关闭”变为“保留引用”，都会改变调用方和下一阶段。原红测只证明旧触发消失，race只检查执行到的内存竞争，不能发现这些业务语义错误。
2. **范围说明替代了缺陷分流。** RR-03/05/06的后果不是抽象“未验证”：错误记录、宽松替身、阻止接管已能明确违反原承诺。我的文档曾把它们放在边界段，是分析不足；环境缺失可以限制独立验收，不能把源码可证的退化降格成正常边界。
3. **恢复后的结果没有成为验收对象。** 取消不等于撤销，保留引用不等于仍可重试。要断言服务端lease/键、最终closed状态及再次接管，不能只断言context error、Done或指针非nil。
4. **包测试数量代替了场景清单。** 754等通过计数对其命令范围可能正确，却没有包含根包的依赖门禁；有两个历史阻塞回归只靠整包timeout给红，失败位置不清晰。旧CI已红也不能作为新提交不查CI的理由。上轮已补根包与推后状态查看，但最近三条workflow可能不含ci本身，已进一步收紧规范。
5. **合并后的对外表述还需精确。** 祖先关系与上轮源码/12+16+11等证据支持“已整合、已执行这些场景”，不支持“全部关闭、真实资源全验或全域review完成”。后续逐项分列已合并、原触发验收、组合/外部待验；源码hash未变也不能替代上游接口/装配契约影响核对。

## 其余审计意见与当前状态

- NC-07依赖越界已在74e1ba39改为生成器直接用chi；当前根包通过。USER_GUIDE现已明确运行期ValidatePath与生成器validateChiPath是独立入口、共同chi语法，原“共用函数”意见针对旧文案成立，不能继续说当前还未更正。两边函数没有“改一处自动传递到另一处”的保证。
- 停止已发起后的callback再次Stop互等、WaitGroup停止屏障以及两个无自带等待上限的测试，另一线已在RR-07补修里处理；本轮不是再次独立全矩阵验收，也不扩张为callback任意首次同步Stop都安全。
- 指标移除的Grafana/OBSERVABILITY已处理，但README两段只划掉指标、仍说“Lua失败降级为DEL+HSET”，本轮改整句话为实际停止重放/保留未知错误。failurelog旧指标类比注释与FatalRemoteError偏窄注释仍属待同步文字，不是运行时有该指标或只在ignore模式生效的证据。
- 索引状态采用新头部覆盖历史条目容易让人混淆，后续当前状态集中在本轮摘要，历史报告只追加更正，不靠改写旧红证据消除矛盾。d3e69336是release文档/manifest提交，本轮不执行或推断新的发布动作。
- mongotest的NaN/Inf响亮失败、写冲突时点/粒度与真实Mongo不同、标量父路径更新等原报告次级限制没有因上述6项修复全部消失；不将替身绿推广为实际Mongo业务正确性。
- **主体核对基线d3时**，[W-2026-10-04-02](../bug/WANTED.md)仍未修。源码支持它的疑点链：Assembly在连接drain错误后硬关闭，Kit在任何Close错误时保留asm，Client下一次调用底层Drain可能报已关闭。**资源泄漏与“资源已关但Stop永远报错”要区分**；该阶段没有connected NATS超时复现，不冒称独立红确认/已修。收尾后续修复状态见末节。

## 已实施的流程改进与下一步

仓库roost-coding增加[组合契约复核](../agent-skills/roost-coding/references/fix-contract-review.md)，roost-bugfix及roost-optimize接入；同步本机三包，并给本机roost-review接入同一清单。核心新增约束是错误传播查消费者、超时后重试至终态、多hash完整读和替身拒绝语义、明确退化登记、场景而非数量验收、合并与验收分列及根包/main ci核验。按风险选场景，不扩张到无关模块或等待CI。

本轮没有新增功能域完成计数：只复核既有缺陷与流程。下一顺序仍Wanted真实NATS→迁移Repository/持久确认/重载接入→N04收口→N05，不因这次复盘重开另一线Nest/Sync/DataEngine全量review。不能保证以后没有遗漏；这些改进针对本轮六项实际根因，可用独立反例检验是否执行。

## 收尾新增修复与最终状态

收尾fetch收到a1c1af3e发布文档、e0591f79/aa35d4a1修复及索引：Wanted已由另一线分流[RR-08](../bug/RR-20261004-08.md)，并修复为Assembly硬关闭后返回可识别终态ErrClosedUndrained（保留原错误），Kit按终态释放引用，只有RPC回调尚未排空的ctx错误保留重试。已整合，保留另一线的发布/真实NATS证据；本轮未发版。上面“Wanted未修/下一先复现”是收到这次修复之前的判断，不再作为当前状态。

独立补验普通`./kit/nats ./nats/driver` race：45叶子pass/0fail/0skip，含两个实际Client连接协议桩的drain/已关闭终态用例，vet及最终根包通过。[成功事件](evidence/noncore-audit-followup-20261004/late-nats-verified.jsonl)、[退出](evidence/noncore-audit-followup-20261004/late-nats-verified-exits.json)、[根包/vet退出](evidence/noncore-audit-followup-20261004/late-checks.json)。首次默认沙箱连接loopback被拒的2失败属于环境错误，保留[原事件](evidence/noncore-audit-followup-20261004/late-nats.jsonl)，授权放行隔离连接后转绿，不计产品红。没有在本机跑真实broker的integration或HA；协议桩不冒认完整NATS。RR-08的健康检查并发可见性等原修复留项未由这两个测试关闭。

所以三组原六项修复已包含，新Wanted也已修并通过本机协议桩/普通包验收；实际资源验收仍分机器与场景记录。下一转正式迁移接入/N04收口，RR-08真实broker/组合验收作为具名余项保留。原d3的32+4定向源码和场景不因两条NATS实现变化冒称已重跑36项；新影响面单独补验并记录。

skill三包在最终上游整合后再次逐文件hash同步、本机review保留新引用，frontmatter与备份相同、相对引用及PowerShell脚本结构检查通过。系统Python仅有不可运行的WindowsApps别名，skill-creator的quick_validate.py未执行成功；改用frontmatter不变对照和实际引用/镜像检查，不宣称该Python检查通过。[main ci快照](evidence/noncore-audit-followup-20261004/ci-snapshot.json)包含d3/a1 success和aa35 queued，不把后者称绿，也不等待。

### 推送前最后同步：NC-30 / RR-09

远端又收到d638f312/00bffd2a/0bd8a9f3/c9aab877；保留[NC-30独立复审](REVIEW-2026-10-04-nc30-audit.md)、[历史开放项分流](REVIEW-2026-10-04-open-triage.md)与另一线[RR-09修复](../bugfix/RR-20261004-09.md)。NC-30的新guard用注册表字节相等代替“本次清理清单覆盖当前键”，拒绝了原本合法的同布局创建/删除/过期；同时暴露Layered.Delete远端失败后保留旧L1。这再次是我此前只测危险竞争被拒、未测正常并发成功的缺口，不是合并丢失修复。该条根因和真实Redis红绿引用另一线证据，本轮不冒称独立完成RR-09全矩阵验收。组合契约清单已补应拒绝/应成功与正式消费者对照。

在最终产品基线c9aab877重新执行本轮已有36项定向race，36pass/0fail/0skip，vet与根包通过；独占Redis进程已退出。[最终事件](evidence/noncore-audit-followup-20261004/final-green-tests.jsonl)、[退出](evidence/noncore-audit-followup-20261004/final-green-exits.json)、[根包/vet](evidence/noncore-audit-followup-20261004/final-green-checks.json)。这些选择器不含RR-09的新8个真实Redis竞争场景；其已修/验证状态按另一线bugfix记录归属，不把本轮36绿替代它们。
