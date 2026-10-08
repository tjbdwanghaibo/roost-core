# v1.23.1 阶段清单核对

核对范围：REMAINING-FIXES-v1.23.1-2026-10-07的B1～B8，FRAMEWORK-DOCS-FINDINGS登记行、bug/bugfix索引、WANTED/CARRYOVER与CORE-OPTIMIZATION-HANDOFF。按最新记录覆盖旧日期状态，未做全仓新一轮逐函数审计。

| 范围 | 当前判断 |
| --- | --- |
| B1～B5/B7/B8 | 前轮已提交验收，记录见v1.23.1双文档；不因历史“未修”重复改造 |
| B6 RR-22～24/26～28 | 已验收；保持正式接入、ReplicaStore与会话生命周期边界 |
| B6 RR-25 | 保留MaxDeliver方案已实施；旧无限重投候选废弃；5包race、真实NATS 5场景通过 |
| 本轮确认缺陷 | 随RR-25修复，B1～B8清单内没有仍待实施的确认缺陷；最终全仓、21格矩阵和tag下载验收均通过 |
| 已决定保留 | Windows Y14只保留编译/CLI范围；C8公开外部集成API保留，不虚构生产调用 |
| 独立产品方向 | Remote outbox唯一发布者仍待决定，当前既有修复不依赖这项重构 |
| 历史验证边界 | CARRYOVER仍列部分真实etcd/HA、迁移复杂组合、外部业务/长容量边界；本轮不能把这些全部计为通过。WANTED表的历史候选已有分流去向，不等于所有外部验证完成 |
| 未做 | 24小时长稳、真实生产部署、Windows完整正确性、客户端完整引擎矩阵 |

因此已发布阶段性v1.23.1，但不能宣称“全仓没有任何问题/所有场景已验”。此前已接受的Sync 50ms少量长尾不改变；本轮也没有新的TPS容量结论。

## 最终验收

RR-25行为红/绿、最终目标race与真实NATS日志保存 `artifacts/perf/rr25-bounded-20261008/`；旧方案日志保留在remaining-fixes目录，不混充新方案证据。

| 验收 | 实际结果 |
| --- | --- |
| 修复/冻结点 | RR-25：66692aad；合入最新main Lockstep：d367b893 |
| 干净工作树pretag | d367b893：生成无漂移、build/vet/tidy、全仓go test全部通过；根包另-count=1通过 |
| RR-25 | 目标5包race、真实NATS总次数1/2/5、预算内恢复、ACK前断线恢复均通过；原行为红日志保留 |
| 正式Remote调用方 | 真实Live订阅、重订与快照读取回归通过 |
| 私有Remote故障矩阵 | 同一d367b893运行21/21 PASS，无skip；逐格结果artifacts/perf/remote/rr25-release-20261008/results.tsv |
| tag | v1.23.1已推送，指向d367b8937eb6ceff7927b484b245a629b21213e1；main已包含该提交 |
| 实际tag生成工程 | 从远端下载codegen@v1.23.1，game-demo显式依赖core v1.23.1，无replace、GOWORK=off；tidy/build/vet/test全部通过，19个测试包 |
| 环境清理 | 本轮私有依赖已clean，确认无残留进程；不操作共享环境 |

## 新合入交接的观察项（未冒认清理）

Lockstep原Windows验收曾有5个失败包，原始日志见[记录](REVIEW-2026-10-08-client-lockstep-validation.md)。合入后本机macOS全仓通过；其中Remote按需读取原断言额外连续30次通过，但原Windows失败与偶发并发读v59/v60尚未确定性复现，不能仅凭本机通过关闭。源码的Cached读取允许在MaxStaleness内返回L1，原用例用1ns真实时钟假定每次都过期，时钟分辨率是待验证线索，并非已证实根因。既有KCP一次context canceled观察也保留；没有放宽任何断言来发布。Windows依既定规则只保证编译/CLI范围。

这次核对结论是：B1～B8清单的确认缺陷已收口；上述观察、外部验收与产品方向仍有明确边界，不等于全仓所有问题已穷尽。

## 索引与证据

发布后尝试CBM fast刷新，worker exit=1：`a pre-coordination or unverified CBM generation is active`。本会话服务对core仍返回09-30代际；另一会话的10-08索引成功记录保留，但不能冒认本服务已刷新。本轮结论按当前源码补证，未重启共享服务。

原始红/绿、pretag、root、21格结果、远端tag生成和依赖清理日志均复制到主检出artifacts/perf/rr25-bounded-20261008及artifacts/perf/remote/rr25-release-20261008。发布后仅回填文档，不移动已发布tag，不做部署。
