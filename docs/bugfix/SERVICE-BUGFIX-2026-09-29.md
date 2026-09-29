# Service 两轮 review 修复与接手（2026-09-29）

范围：RR-20260929-01～18，加旧 RR-20260909-02 的正常 Finish ABA 残留，共 19 项。基线 `f46db3e7d504bbd73581aa4119baa71171352519`，已获取最新远端。复用隔离工作树完成源码、正式回归、文档；没有发版、打 tag 或执行生产数据迁移。

本轮增加 [roost-bugfix skill](../agent-skills/roost-bugfix/SKILL.md)，仓库保存规则源，本机安装在 `C:/Users/tjbdw/.codex/skills/roost-bugfix`。明确修复请求进入此流程；普通 roost-review 的文档审查习惯保留。SKILL frontmatter 和 agents/openai.yaml 做了基本结构核对；环境没有 Python，没有运行 bundled quick_validate.py。

## 逐项状态

| 编号 | 修复范围 | 状态 |
| --- | --- | --- |
| [RR-20260929-01](RR-20260929-01.md) | platform 迟到发货与人工结算 | 已实施、回归通过 |
| [RR-20260929-02](RR-20260929-02.md) | activity 未确认进度重复计入 | 已实施、回归通过 |
| [RR-20260929-03](RR-20260929-03.md) | account 建角补偿误删已有名字 | 已实施、回归通过 |
| [RR-20260929-04](RR-20260929-04.md) | mail 过期不可见条目占满邮箱 | 已实施、回归通过 |
| [RR-20260929-05](RR-20260929-05.md) | rank 带逗号的 requestID 无法去重 | 已实施、回归通过 |
| [RR-20260929-06](RR-20260929-06.md) | rank 并发 no-op 覆盖幂等记录 | 已实施、回归通过 |
| [RR-20260929-07](RR-20260929-07.md) | chat 自定义频道键碰撞私聊 | 已实施、回归通过 |
| [RR-20260929-08](RR-20260929-08.md) | activity 最后 ACK 窗口被重试提前关闭 | 已实施、回归通过 |
| [RR-20260929-09](RR-20260929-09.md) | session 后台 sweep 无公开 owner 接线 | 已实施、回归通过 |
| [RR-20260929-10](RR-20260929-10.md) | rank Around 最大半径与页上限不一致 | 已实施、回归通过 |
| [RR-20260929-11](RR-20260929-11.md) | account 已验证身份键碰撞 | 已实施、回归通过 |
| [RR-20260929-12](RR-20260929-12.md) | account slot 写后响应丢失误补偿 | 已实施、回归通过 |
| [RR-20260929-13](RR-20260929-13.md) | global 迁移后旧路由 lease 可续期 | 已实施、回归通过 |
| [RR-20260929-14](RR-20260929-14.md) | session Attach 接受伪造释放字段 | 已实施、回归通过 |
| [RR-20260929-15](RR-20260929-15.md) | activity 非负累加回绕为负 | 已实施、回归通过 |
| [RR-20260929-16](RR-20260929-16.md) | mail 正文失败后同请求不能恢复 | 已实施、回归通过 |
| [RR-20260929-17](RR-20260929-17.md) | global Load 返回 map 污染 MemoryStore | 已实施、回归通过 |
| [RR-20260929-18](RR-20260929-18.md) | session Finish 重试成功遗留 claim | 已实施、回归通过 |
| [RR-20260909-02](RR-20260909-02.md#2026-09-29-正常-finish-残留补修) | session 正常 Finish 原子身份删除 | 已实施、Memory / 真实 Redis 回归通过 |

“已实施”指本次代码和明确反例。下面的旧数据升级、外部系统对账与未测窗口仍有效，不能把 19 项关闭写成框架再无故障窗口。

## 三个实现原则

1. **未知结果先核对。** Account slot 回复丢失不能据此删角色；Mail 把固定 ID 和完整发送意图保存在现有 ledger，重试恢复原内容；支付保留每一次尚无回包的 attempt 身份，人工结算不能忽略这些请求。
2. **版本不是跨生命周期身份。** versionstore 新增可选 ConditionalDeleter。Memory 在锁内核对逻辑身份；Redis 读出当前原始字节、校验版本和谓词，再在一段 Lua 中比较原始字节并删除值和索引。Session ClaimStore 强制要求此能力，RunID/OwnerID 在删除原子边界核对；通用 Store.Delete 的版本语义不变。
3. **有界不意味着可以忘记义务。** Activity 把可回收 recent ring 和不可丢失 pending proof 分开，容量满时背压，并用 Applies 快照拦住旧 reserved 读者。Rank 把 member 与去重 ring 一起 CAS。Mail 只回收已知过期且没有在途未结算附件的条目。

沿用 versionstore、Directory、现有服务 Sweep/生命周期和 servicerpc；没有引入新的后台扫描框架或事务存储包。

## 本轮验证

修前在当前基线重放保留的两轮反例：**18 个顶层 FAIL**，对应 17 个新动态问题和旧正常 Finish ABA；RR-09 是源码接线缺口。完成重叠只是一项 PASS 观察，不登记成 bug，也不把该观察固化成新的永久业务承诺。原始 .go.txt 与两个 review 报告均保留。

修后：

- `go test -race -p 1 -tags integration -json ./versionstore ./service/... ./kit/service/...`：**16 个有测试包、756 个测试/子测试 PASS**；servicemetrics 别名包是 [no test files]，没有测试用例 skip。
- 随后补加 activity 旧 reserved 读者恢复、rank 旧 ring 升级和损坏账本拒绝 **3 个额外顶层用例**，两个包 race 回归通过；不是把两次输出的重复用例累计。
- `go test -race ./versionstore -run '^TestConditionalDeleteProtectsRecreatedIdentity$'`：Memory/真实 Redis 身份删除通过，真实 Redis 同步清理索引成员。
- `go test ./... -run '^$'`：全仓编译通过，**不是全仓业务测试**。
- `go vet ./versionstore ./service/... ./kit/service/...`：通过。
- servicerpc 源码构建后，以各包原 go:generate 的 dir/emit/out 参数执行 **12 个 -check**：通过，生成产物无需改变。
- 隔离真实 Redis `127.0.0.1:16391`；排行实际执行 Lua，session 正常 Finish 实际重放 Redis ABA。跨服务 prefix 检查依赖“无其他包同时写入该 Redis”，因此本轮使用 `-p 1`。并行试跑的 prefix 误报已解释，不改业务代码规避断言。

Formal fixtures 位于相应包的 `rr_20260929_round{1,2}_test.go`；额外边界测试位于 `bugfix_*_test.go`、`sweep_source_test.go`、`options_test.go` 和 `conditional_delete_test.go`。Account slot、Attach、overflow 的正式断言比原始反例更强。CAS 重试使 rank 暂停点可能被多次调用，故障钩子改为仅暂停一次，保留业务结果断言。

精简执行统计与 18 个修前 FAIL 名称见 [results.json](evidence/service-bugfix-20260929/results.json)。最终 Mail 到期时间在 CAS 外固定后，补跑 `go test -race ./service/mail` 和该包 vet 均通过。大日志、缓存、二进制没有提交。

图谱项目 `roost-core` 根为主检出 `D:/whb_s/cube-core`，coverage generation `2026-09-29T00:39:36Z`。结构定位使用 search_graph、trace_path、get_code_snippet；check_index_coverage 中旧证据路径标记 metadata_changed，新增工作树文件为 missing，已直接读取/检查当前工作树源码与 diff，不把旧图谱当本次新代码。没有宣称穷尽整个 service scope。

## 升级与恢复

- **停写后统一升级相关 owner 节点。** Rank applied_v2、Activity pending proof 不能与旧写者混跑。旧支付节点/外部调用应先排空，旧订单没有 PendingAttempts 不能解释为无请求。JSON 兼容读取不等于混版本并发语义兼容。
- **Session 自定义 ClaimStore 是源码兼容变更。** 必须实现 DeleteIf 并在同一原子边界校验身份；内置 Memory/Redis 和所有仓库消费者已编译验证。不提供“先 Get 再普通 Delete”的不安全 fallback。
- **后台清理需要部署接线。** 通过 session.NewMod(releaser, reporter, session.WithSweepOwners(source)) 注入 OwnerSource；source 实现 SweepOwners(ctx, limit)，以公平轮转返回至多 limit 个 owner。无 source 时明确禁用后台 sweep，仍支持主动 Sweep/下次 Enter 的 lazy 恢复。OwnerSource 故障下次 tick 重试，不扫描全 keyspace。
- **Account 的旧复杂 channel ID** 只有验证结果完全匹配才保留；已经合并的身份、name 旧 owner 和 unknown slot 应对账后修正。slot unknown 错误带回 PlayerID；不因再次 ErrRoleLimit 就删角色。
- **旧邮箱期限为 0 / 旧 send ledger 没 Intent** 不能从正文缺失推断过期或原内容。仍需可靠原始正文/发送审计补数据；本轮没有自动迁移。新 Intent 增加 payload 体积，需实际容量测量。
- **自定义 Chat Kind 含 : 或 %** 的键变化；停止旧写者后按确切频道类型迁移，不读歧义旧键。已发生的泄露、重复计分、正数回绕不能被新代码自动撤销。
- **人工支付恢复**：先停止并核对全部外部尝试，再在 owner 进程调用 ResolvePendingAttempts(note)，随后根据真实业务结果选择 SettleOutOfBand / ReopenDelivery；两项仍是 owner-only，并无公开 bus 管理路由。错误回包是否代表外部系统停止、外部 grant 幂等与对账由 Deliverer 实现保证，本轮未重新定义供应商协议。

## 未验证边界与下一任务

未执行真实 Broker 发奖、多进程强杀、网络分区/Redis HA、生产存量迁移与大规模负载；没有补做之前其他模块性能任务。Session 外部释放成功但状态标记失败仍需 Releaser 幂等；Account 其他写入点的未知结果/崩溃恢复不由 slot 尾写回归证明。

建议下一次 roost-review 独立验收本次实现和升级边界，再查 mail 在途 claim 到期/正文缺失的恢复分类以及 Account 其他写入点；不把已完成 19 项与这些独立待查问题混成“尚未实现”。
