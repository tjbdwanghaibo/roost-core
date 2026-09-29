# Service 第三轮 bugfix 与接手（2026-09-29）

用户调用 roost-bugfix，范围是上一轮 RR-20260929-19～22。基线 `23f92d735b9a9023a0330fb2b8bc0514c4cff29f`，fetch/快进后未新增 main 源码；复用 `D:/whb_s/.tmp/review-service-20260928` 完成实现，保留主工作树及其他 agent 工作。没有部署、打 tag、执行生产迁移。

| 问题 | 最终实现 | 状态 |
| --- | --- | --- |
| [RR-19](RR-20260929-19.md) | slot 持久建角计划、固定 ID 重放、名字 token admission、未知结果向前恢复、待发布角色准入保护 | 已实施；Memory/Redis 原触发、重建 Service、并发回归通过 |
| [RR-20](RR-20260929-20.md) | CancelClaim 强制 Attempts，Token+代次 CAS；RPC/样例/模板同步；计数不回绕 | 已实施；Memory/Redis、local/bus、旧 JSON 拒绝通过 |
| [RR-21](RR-20260929-21.md) | Directory DeleteIf 原子校验 Token/Owner/State，后端能力门禁 | 已实施；8 个两后端/两操作/两 owner 场景通过 |
| [RR-22](RR-20260929-22.md) | 三个公开 Role 输出克隆 Profile，保存与返回分离 | 已实施；Memory/Redis 输出所有权回归通过 |

## 验证与可复跑

修前失败材料保留在 [上一轮 evidence](../review/evidence/service-review-20260929-03/README.md)，源码时点 `83c04243`。其旧 Mail 四参数调用、未转发 DeleteIf 的 Slot wrapper 在新接口/能力门禁上需要迁移，正式回归是当前复跑入口；不覆盖历史红测结果，也不把编译/构造失败说成原 bug 复现。

最终完整回归：

```powershell
$env:REDIS_ADDR = '127.0.0.1:16393'
$env:ROOST_REVIEW_REDIS = '127.0.0.1:16393'
$env:ROOST_REDIS_TEST_ADDR = '127.0.0.1:16393'
go test -race -count 1 -p 1 -tags integration -json ./versionstore ./service/... ./kit/service/...
```

退出 0：**16 个包通过，794 个测试/子测试 pass 事件，0 个测试级失败/跳过**。servicemetrics 别名包没有测试；事件数包含父测试，不是独立业务场景或源码覆盖率。

补真实 Redis 的 Account/Mail 原触发、控制、重建与并发（Directory 的两个后端已在上面测试内）：

```powershell
$env:ROOST_REVIEW3_BACKEND = 'redis'
go test -race -count 1 ./kit/service/account ./service/mail -run 'TestReview3|TestPendingRoleRecoversAcrossServiceRestart|TestConcurrentCreationRetries'
```

退出 0。不要在下一次完整基础回归中遗留该变量；默认 Memory 模式含原有 Memory 行为证据。账号 race 定向 -count 15 通过，用于追查并解决一次 stale slot 快照/并发名字已提交的交错，未用重跑掩盖失败。

其他实际检查：
- 全仓 `go test -run '^$' ./...` 通过，包含所有包与测试编译；这是编译检查，不是全仓行为通过。
- Account、Directory、Mail、split 定向 go vet 通过。
- Mail go:generate + 在包目录执行 servicerpc `-dir . -emit transport -check` 通过。
- Account go:generate + 在包目录执行 `-dir . -check`，transport/assembly 均通过。根目录使用不同 -dir 文本曾报告 STALE，按 go:generate 的包目录参数核验并保留正确生成物；没有手改生成代码。
- `go test -count 1 ./codegen/...` 首跑仅 roost 部署 shell 语法用例因找不到 sh 失败；其余 codegen 包通过，roost 未出现其他失败，不声明每个环境依赖用例均执行。Windows 普通沙箱中补 PATH 仍无法给 Go 子进程找到 sh；使用本机已有 Git sh，在获准的进程环境内重跑该用例，两种部署配置通过。没有改测试或全局 PATH。shellcheck 未安装，该测试原设计记录日志并返回，**未声称 shellcheck 通过**。

隔离 Redis 16393、独立测试前缀；结束仅停目录匹配本批的实例，不停止共享 Redis/MCP。未做真实发奖、Broker、Redis HA/断网、多进程 kill -9、生产迁移或性能负载。

## 实施证据与兼容

图谱项目 roost-core，live Git HEAD 指向基线，coverage generation `2026-09-29T03:13:02Z`。search/trace/snippet 定位后，全部候选/修改路径调用 coverage；metadata_changed 与隔离树新增文件 missing 用当前源码补证。graph 不能表示尚未推入主工作树的新增实现，不将 ready/无记录 gap 当完备证明。

没有加持久层或必填配置 store。Account 的 Slot.Creation、Role.CreationID 是新增 JSON 状态；同名创建重试现在幂等返回原角色，不同名字仍受角色数限制。Directory state 与 Account Slots 必须支持 ConditionalDeleter，custom wrapper 要转发。Mail 为明确 Go API 变更，owner/caller 滚动顺序及旧工程业务文件迁移见 RR-20。

旧空 slot、旧孤儿角色、没有角色的 committed 名字不自动删除。新 pending 角色的名字若被别的 owner 提交，保留计划并禁止使用，等待业务对账；没有全自动的冲突处置 worker。具体恢复边界见 RR-19。这些是维护和升级边界，不抹掉四个已验证原触发的修复结果。

[进度](../review/PROGRESS.md) · [当前实现学习](../review/IMPLEMENTATION-SERVICE-COMPENSATION-AND-ATTEMPT-IDENTITY.md)。后续声明没有修复时仍按 roost-review 规则跳过旧验收，继续新范围；明确修复请求再进入本 skill。
