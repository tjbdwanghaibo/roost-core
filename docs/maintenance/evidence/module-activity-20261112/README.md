# R9 活动生命周期模块（2026-11-12）

基线 `75039d64`，`feat/business-modules`，macOS。生成工程 replace 到当前工作树；本轮不合 main、不发版。工具链：`go version go1.27.3 darwin/arm64`。当前使用与边界以 [activity](../../../../modules/activity/README.md) 为准。

## 实现与取舍

个人、全服/服务器组、联盟三种宿主共用 ActivityBook 与 activity.Host：初始化→预热→开启→结算→结束；固定锚点、间隔/日历循环、可信条件解锁、自定义 Flow、异步 pending 完成、同事务回滚、有界恢复。三个宿主直接挂同一个 activity.Component Go 类型，经 activity.Owner 接口取得 DAO 与归属；实体包只留类型别名和注册。Player 提供同事务定时唤醒，Remote 保存期限由权威服务扫描。配置表默认空，不创建示例活动。

个人复用正式 entitytimer。Remote 生成契约不允许本地持久定时器，本轮保留限制：全服/联盟服务扫描当前 ownership 清单，路由核对后交回正式 Nest 推进，不改三大核心执行契约。离线个人恢复默认惰性；Remote 扫描不是准点执行保证。排行榜/邮件是可组合身份/发件适配，业务自行冻结奖单并幂等履行；CoordinatorKey 接到现有 service/activity，不自动部署另一套协调服务。

安装器增加 existing 服务扩展、configdata 代办与首个有状态定时模块的 DAO 前置计划；配置和服务所有权保持清晰。开发/生产示例/Secret 示例一起纳入失败回滚。全服和联盟 RPC Mod 明确依赖 configdata，不能只编译通过却缺少规则加载器。

## 生成工程验收

[原始输出](acceptance.log)，路径已脱敏。命令：

```sh
GOWORK=off go test ./codegen/internal/roost \
  -run '^TestModuleActivity|^TestModuleExistingServiceExtension|^TestModuleCatalog|^TestModuleWith' \
  -count=1 -v -timeout 20m
```

110.046s，通过（统一组件后的最终复验；先前 95.800s 一轮同样通过）；每个 VerifyModules 内含场景 `-race -count=1`、build / vet / ID 校验 / generate --check。

| 组合 | 结果 |
| --- | --- |
| 单装 activity | 21 PASS / 0 SKIP |
| activity_world + activity_alliance 自动装依赖 | activity 21、全服 4、联盟 4，合计 29 PASS / 0 SKIP |
| 加 profile、leaderboard、mailbox（带 enter） | 上述 29 + 四组活动联动各 1，合计 33 PASS / 0 SKIP |
| 重复安装 | 改变输入顺序后再次安装，工程摘要不变 |
| 服务扩展失败 | 故意令生成失败，服务、配置、DAO、manifest 摘要与安装前一致 |
| 权限与隔离 | 已有服务扩展不能接管 Entity；计划不修改原 manifest；重复 RPC 拒绝 |
| 定向 race | game/activity、game/handler、两个活动 RPC 包，筛选活动场景 `-race -count=2` |

组合中的 **33 仅统计本次活动基础/宿主/联动**，不是整个工程所有已安装模块的总场景。联盟 Remote 与提交器为内存替身；正式 Nest 与生成 DAO/定时器用于验证事务、回滚与旧属主拒绝。BSON 序列化恢复不是磁盘 WAL 或 Mongo 验收。

## 9 个变异

均在实际生成工程临时修改后复跑对应场景，必须断言失败，不接受编译失败；每项后恢复源码。

| 变异 | 被检出的事实 |
| --- | --- |
| 去掉 CheckOpen 的关闭期限判断 | 到期消息尚在队列时过期业务也必须拒绝 |
| 忽略 pending | 初始化未完成不能进入开放阶段 |
| 绕过 Token 匹配 | 旧流程/旧期回执不得更新当前实例 |
| 取消单次 32 次动作上限 | 停机跨期恢复不能在一次 handler 无限补跑 |
| 重复解锁重建记录 | 两个玩家锚点独立，重复解锁不改开始时间 |
| 定时推进错误吞掉后 return 0 | 初始化失败必须回滚业务和定时节点删除 |
| 日历开期错误地加一小时 | DST 业务日不是固定 24 小时，锚点不漂移 |
| 邮件键遗漏 Cycle | 不同期不能被同一封奖励去重吞掉 |
| 忽略 Remote 路由拒绝 | 旧属主恢复扫描不得越权推进 |

开发中已修正：生成 RPC 的本地类型别名和 run 骨架、个人定时入口的测试注册方式、首次 DAO 计划、配置 Mod 缺失、联动规则表隔离及安装配置回滚。没有为通过验证放宽期限、吞掉异步错误或扩大恢复预算。

## 其余检查与剩余项

[根包检查](root.log)通过（10.400s）；[文档锚点](docs.log)检查 327 个文件，0 断链；git diff --check 通过。全模块正反序、完整 TestModule/TestDemo、全仓 go test、真实 Mongo/Redis/NATS/WAL、TCP/Robot、跨进程迁移与 kill -9 恢复均未在本批运行，依交接 §5/§6 留到最终批次。没有新增性能或准点结算承诺。

CBM 使用 Tier 2；模板多数仅有 File 节点，调用关系通过当前源码核对。局部解析缺口主要是模板占位符；逐项补读原文件，不将图谱无边视为无调用。最终代际 `2026-11-12T08:48:16Z`；本批模块及共用组件等 61 个路径再次核对，无 metadata_changed。9 个模板局部缺口逐段补读；此外安装器、组件生成器及调用方逐项核对 coverage。docs/scripts 为设计排除，直接读取并运行对应检查。
