# 非三大核心第二批修复：Manager / Admin / Ops

本轮用户要求“修复，并继续 review”。初始工作树干净，main 更新前/后为 `7e0d6ee29a134e75db142fda94ef93332b8e9841`，fetch / ff-only 确认无新提交。仓库维护 roost-bugfix/coding/optimize 完整资源仍与本机同名镜像一致；没有修改技能规范。

按 roost-bugfix 完成 RR-20261004-NC-01～04 **4/4 修复、声明场景验证**。只改 manager.Engine、admin schema helpers、OpsMod 与必要 Kit 错误别名；不动另一线的 Nest/Sync/DataEngine，不改依赖、持久/wire 或生成模板。[逐项记录](../bugfix/README.md) · [正式红绿与复跑](../bugfix/evidence/noncore-bugfix-20261004-02/README.md)。

| 问题 | 实际修复 | 具名补证 |
| --- | --- | --- |
| NC-01 Manager 停止 panic | 单对象 recover；逆序清理继续，错误 Join 和 Start cause 保留 | plain/context、rollback、最后 Start 接管、error panic cause |
| NC-02 重复 Start | 锁内一次启动权、ErrStartState；Kit alias；缺 Provide 不取得权 | 16 并发重入、Order/Start 失败、Stop 前后、补 Provide |
| NC-03 schema 引用 | 递归 JSON map/array/[]string，非 nil 空 map 复制，nilness 保持 | 原输入/Get/List 六反例；8 nil/空、嵌套数组、并发副本修改 |
| NC-04 Ops 取消关闭 | 错误保留 server，短锁状态，捕获监听实例、同实例成功才释放 | 实际取消/期限、再次排空、新旧 Start、各 caller context、race |

正式原 14 项由 11 fail/3 pass 全转绿；增加 21 邻接项后 **35 个新增叶子/独立项通过**。最终 11 包 race / 233 test pass 事件（含父/子/Example）、0 fail/skip；同包 vet 通过。原上轮 overlay 原文单独通过，Health 空 Status 仍未定契约。原始证据与精确命令在附件。

兼容收紧：Engine 开始后不再自动重复尝试，失败后用新 Engine；未 Provide 时仍允许补装配后启动。Ops active/draining 时拒绝 Start，成功排空后可启动新 server。schema 的非 JSON 自定义值须不可变。没有用新的生命周期框架/强关连接/自动重试掩盖问题。实际实现学习已补 [所有权机制](IMPLEMENTATION-RUNTIME-MANAGER-AND-OPS-OWNERSHIP.md)。

Tier 2：engine/cloneMeta/Ops 搜索和双向 depth=1 trace、片段对读；同名误连不作调用事实。coverage generation `2026-09-30T11:58:14Z`，生产 metadata_changed / 新正式测试 not_tracked 均源码补证；未重建或停止共享服务。永久阻塞回调、完整 App 故障进程、hijack/bind 与外部 HA/长稳未宣称。gh 不可用未查远端 CI，未发版/打 tag/部署。

继续 review 进入 N02 security/gateway/正式 Webroute，新的问题只记录，另列运行/进度，不能把本轮修复全绿写成 N01/N02 全场景完成。
