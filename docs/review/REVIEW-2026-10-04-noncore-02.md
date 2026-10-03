# 非三大核心第二批：运行生命周期与 Ops

本轮在[四项 App/HTTP 修复](REVIEW-2026-10-04-noncore-01.md)之后，按 roost-review 继续 N01，新增[三个 P2、一个 P3](../bug/REVIEW-2026-10-04-noncore-02.md)，全部未实施。Manager/Admin/Ops 产品源码未修改；新反例放在 docs 的 .go.txt，通过 Go overlay 运行。源码提交 `3529a569920f08f3f42a45da9a2119e5935b45ae`；执行在同一产品源码提交前的工作树，记录哈希和日志，不因为提交操作再重复全套测试。

## 本轮范围与进度

N01 总范围为 app 9、lifecycle 2、manager 2、health 1、admin 1，合计 **15 个生产 Go 文件已完成源码读取**。上一轮已读 App 主链五文件原证据复用，本轮补齐其余十文件；App 单 Mod guard 使用新修复证据。另读 Kit ManagerMod 和 OpsMod 两个正式接入文件。源码读取完整不等于全部状态/场景验证完成。

| 当前路径 | 阅读与本批主链 | 实证 / 余项 |
| --- | --- | --- |
| app/app.go、mod.go、service.go、registry.go、runtime_failure.go | 复用上批源文；仅单 Mod 校验 guard 本批变更 | 四项修复 / 11 包回归；不重复算五个新读文件 |
| app/manager.go、config_validation.go、name.go、buildinfo/buildinfo.go | Manager 契约、配置强约束、能力名、ldflags 信息 | 源码/相关回归；全配置组合、环境变量、启动进程未另验证 |
| manager/engine.go、order.go | 注册封闭、依赖排序、启动失败/停止接管、逆序清理 | 新 5 个叶子/独立项，4 fail/1 pass；并发重复 Start 仍待验 |
| lifecycle/lifecycle.go、manager_group.go | Hook 快照/锁外回调、EmitAll、Group 操作串行/状态/回滚 | 两个新 hook 控制通过，现有 Group race 用例通过；完整新并发/阻塞矩阵未跑 |
| admin/admin.go | 命令/metadata 注册、回调隔离、输入输出复制 | 7 个叶子，6 fail/1 pass；完整权限/审计链随 N02 |
| health/health.go | checker 快照、panic/Status/Err、串行聚合 | 默认空 Status 契约观察；阻塞/大 checker 数量待验 |
| kit/manager/manager_mod.go | 正式 Registry 能力发布与 Engine 转发 | 当前源码及包回归；未单独注入完整 App 停机 panic |
| kit/ops/ops_mod.go | Init/Provide/Start、ready、token/JSON/admin、server 关闭 | 取消关闭失败 + 正常控制 + Health 观察；启动 bind 失败、并发 Start/Stop/hijack 未验 |

**新增执行 17 个叶子/独立项：11 fail + 5 pass 控制 + 1 仅观察。** JSON 原始事件为 13 个 test fail（含两个父测试）与 6 个 test pass；不能按父事件重复计场景。既有 manager/admin/lifecycle/health/kit-manager/kit-ops 六包 race 通过、54 个 test pass 事件、0 fail/skip，同包 vet 通过。故障反例集 exit=1 是断言失败，与既有回归 exit=0 分开报告。[可复跑材料](evidence/noncore-review-20261004-02/README.md)。

## 图谱使用与限制

采用 Tier 2 Verify，roost-core root 与仓库核对，index_status ready 32285 nodes/209828 edges。generation 仍 `2026-09-30T11:58:14Z`；生产路径 freshness=metadata_changed，新反例 not_tracked。没有重建或停止共享索引。

search_graph 当前 bounded scope 的 engine 13、lifecycle 23、admin 41、health 22、Kit manager 20、选定 Ops 13、App config 17/name 9/buildinfo 6，相关搜索 has_more=false；不是全仓符号盘点。stopOne 双向 depth=2，stopStarted/stopInitialized/cloneMeta/checkOne/Ops handleReady/StopWithContext 双向 depth=1 无截断，关键片段 get_code_snippet 对读。

engine 的 Start/Stop 在相关图查询中没有得到生产方法节点，同名测试/标准库 heuristic 边也不可靠；没有由此得出无调用结论，均以当前完整源码确认。cloneMeta 的 Register/Get/List 链与 ManagerMod 转发同样用源码校验。[coverage](evidence/noncore-review-20261004-02/graph-coverage.json) 包含 17 个产品路径、4 个反例附件及相关 scopes；无 recorded gap 不证明完整，metadata_changed / not_tracked 全部已回读。

## 设计、性能与接续

[机制学习与既有能力实施建议](IMPLEMENTATION-RUNTIME-MANAGER-AND-OPS-OWNERSHIP.md) 说明两种 Manager 生命周期、Ops 标准库资源所有权和 Admin 复制边界。没有引入新抽象或更换框架。Health 串行 checker、Admin 每次复制、callback 无强制终止属于需要容量/预算验证的机制观察，本批没有 benchmark 或线上 SLO 证据。

**N01：源码读取 15/15，场景验证部分完成，4 个新 RR 未修；不能标为整个单元完成。** 停点与具名待验项保留。后续按计划接 N02 security/gateway/正式 Webroute，同时补 N01 未验的 Manager 状态竞争、Group 阻塞/预算、Ops 失败/重试及认证/审计链；用户说未修时直接接新内容，不反复验收旧 bug。其他 14 单元状态不因本批包测试变化。Nest/Sync/DataEngine 主域仍由另一线处理。

仓库三份维护 skill 与本机镜像仍一致，不更新技能内容。本轮仅上批已授权修复改产品；继续 review 部分只改 docs，不发版、不打 tag、不部署。gh 缺失未查远端 CI；外部 HA/长稳未运行。原 54～92 有效小时预算保留为 10-03 初估，缺少多批速度数据不能凭文件数线性扣减或承诺完成日期。
