# Service 第四批修复与接手（2026-09-29）

基线 `66b58d90856b3c31f82b415a271f1450df1769f3`，fetch 无新增提交。本次按 roost-bugfix 实施 **RR-20260929-23 P1、RR-20260929-24 P3、旧 RR-20260910-02 未领取删除残余 P2**；未发版、未部署、未操作生产数据。问题原记录见 [第四轮 review](../bug/REVIEW-2026-09-29-services-04.md)，历史失败材料保留不改。

## 行为与理由

| 问题 | 最终实现 | 正式回归 |
| --- | --- | --- |
| RR-23 | 新增 collaborator sentinel `ErrDeliveryNotApplied`。仅明确证明没有外部效果时移除当前 pending，并按原预算重试。其他错误保留 PendingAttempts，进入 exhausted、停止自动重试；ResolvePendingAttempts 对账前不能 Reopen/Settle。退避到期但还有在途证明时，同样转入人工恢复，防止慢调用被第二次发货追上；迟到成功仍能完成原 generation | platform/bugfix_external_outcome_test.go：写后超时不得退款/重发、服务重建仍受围栏、明确未应用控制、收据隔离；race_test.go：退避到期只调用一次，迟到权威成功可完成；原 reconciliation 回归保留 |
| RR-24 | 重复 HandleCallback 构造 receipt 时 clone Order，包括错误返回分支；修改 PendingAttempts 不再绕过 MemoryStore CAS | 修改返回切片后存储内容、版本不变，原 callback 能正常完成；Memory 和 Redis |
| 旧 RR-10 残余 | 复用 SettledClaims 墓碑，添加 `deleted,omitempty`，只标记从未领取的删除。淘汰 Entry 时保留删除身份与 envelope expiry；Deliver 不复活，Reserve/Commit 维持 Missing。新删除证明的未知期限不能按旧 legacy 计数丢弃；容量不足拒绝新投递，已到期证明可回收 | mail/bugfix_deleted_identity_test.go：真实 Send→Delete→200 条容量淘汰→重复投递→拒绝领取，两个后端；未知期限证明不被计数遗忘、到期回收 |

没有新增包或存储系统，保持现有 CAS、pending 索引和 owner-only Admin。模板的本地未绑定/未知商品/编码失败发生在 HSet 前，显式标记未应用；Redis HSet 返回错误仍视为未知。不能用超时、错误文本或 ctx 是否取消来推断发货未生效。

原 Platform recordingDeliverer 的失败确实发生在 grant 计数前，故更新为类型化“未应用”，保留原预算/重试测试。旧 race 测试曾期待慢调用后第二次也发奖，现在替换为不重复调用的行为契约。Mail 的旧测试曾明确期待“删除但未领取的邮件可以复活”，已纠正为保留删除身份，不通过 skip 隐藏错误。

## 兼容与恢复

- RPC 方法、错误码、Order 持久格式不变。ErrDeliveryNotApplied 为 Go collaborator 标记，外部 RPC 仍返回既有 Failed/Expired 分类；旧 collaborator 的普通失败将进入人工对账，需要接入方逐个确认哪些分支确实未生效再 wrap。不要把整个 Deliver 函数的所有错误统一标记。
- DeliveryExhausted 现在也代表结果待对账，Attempts 可能小于 MaxAttempts；监控与客服不能仅凭 exhausted 判断可退款。对账须先停止/围栏旧外部调用并查询权威发奖，再用现有 ResolvePendingAttempts 和 Settle/Reopen，记录真实结果。外部 OrderID 持久幂等仍是要求，不宣称对账能证明 exactly once。
- Mail JSON 新增可选 bool；旧数据默认 false，原 claimed/in-flight token 墓碑语义保留。所有 mailbox owner 应一起升级，避免旧节点按未知期限 legacy 策略丢掉新删除证明。回退旧版本会重新暴露删除遗忘风险。
- 过去已被删除的未知 pending、已忘记的 deletion 无法从现在的 store 自动重建；需要业务审计/备份对账。本次不擅自扫描或迁移。
- 在途已 Reserve 后再 Delete 的 Commit 在淘汰前后仍有既有分类差异，此轮不决定“删除是否取消已经可能发奖”的业务语义。不能将该观察写成附件已安全取消。

## 实际验证

Go 1.27.0 Windows amd64，race 可用；隔离 Redis loopback 16395，无持久化。

- 修前 Memory 复跑原 Run-Repro：5 pass / 4 fail 测试及子测试事件；原历史两后端证据不改。本次新控制改为显式 ErrDeliveryNotApplied，所以旧通用错误控制不再是升级后的合法“可自动退款”控制。
- 修后两个正式包 race 全量通过；新定向行为分别以 Memory、真实 Redis 执行通过。服务重建是同持久 store 重建 Service，不是强杀进程。
- `go test -race -count=1 -p=1 -tags integration -json ./versionstore ./service/... ./kit/service/...`：**16 个测试包通过，801 个测试/子测试 pass，0 测试级 fail/skip**。servicemetrics 无测试包的 skip 事件不计为行为测试。
- `go test -run '^$' ./...` 全仓编译通过；`go vet ./kit/service/platform ./service/mail` 通过。
- `TestDemoTemplateGeneratesABuildableWritePath` 通过，但该用例主要做生成/解析和 doctor 校验，不能当真实消费链编译。本次另用正式 CLI 生成 game-demo，生成448个文件后依赖解析被沙箱网络阻止；将仅该临时工程的 core replace 到本工作树，再执行 `go test -mod=mod -race -count=1 ./internal/service/Platform ./game/purchase ./game/handler ./internal/service/game` 通过（前两个包无测试，后两包有测试）。未启动生产服务或真实支付。

图谱 roost-core generation `2026-09-29T05:34:58Z`，search/双向 trace/snippet/coverage 使用完成；路径 metadata_changed，新工作树测试 missing，全部读当前源码补证。未重启共享 MCP，未把无记录 gap 当完整性证明。原始日志本地 .tmp；正式回归已进包，复跑不依赖临时 overlay。

下一步：仅做 service 新内容 review，记录 10 域完成矩阵与外部接入边界；本次不自动修新 review 的问题。