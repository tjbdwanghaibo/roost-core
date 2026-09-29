# Service 第七批：Activity 配置与首份购买奖励

2026-09-29；源基线 `bcebb80568559dcf895db0683cb85eee75b9ef53`，实际隔离树 `D:/whb_s/.tmp/review-service-20260928`；主树/隔离树开始干净，三仓 fetch/快进均无变化。当前实现均在 Core，本轮选定 RR-31/32，**2/2 已修复、声明场景验证**。随后 review 新发现 RR-33，只交接未修复。

| 问题 | 最终行为 / 兼容 | 正式行为证据 |
| --- | --- | --- |
| [RR-31](RR-20260929-31.md) | Activity 调用现有 Cluster tag 验证；RPC/key/存储格式不变，拒绝旧错误配置，迁移另行计划 | 7 叶子/8 事件，真实 Cluster 配置/生命周期/多 game 部分完成恢复，race |
| [RR-32](RR-20260929-32.md) | producer 先读已有 grant，再单键原子首写、核验实际 durable winner；有字段不重查 catalog，未知错误保留未知分类 | 新生成 consumer 两后端 26 叶子/29 事件，写后丢回复、重建、并发、改版/下架与坏证据保护，race |

没有改通用 Redis API、订单/奖励格式、RPC、DAO、ledger 或后端依赖；没有发版/部署/生产数据迁移。新增正式生成测试已由 demoScaffoldSteps 接线，不只是一个无人消费的 tmpl 文件。

## 修前、修后与实际运行

[修前结果](evidence/service-bugfix-20260929-07/BEFORE.json)：原第八轮 overlay 8 叶子，3 pass/5 fail/0 skip/build-fail。测试时生产树仍干净 bcebb805；旧 runner 的 working_changes=true 是硬编码描述，不能据它认定修前已有本轮改码，实际源码基线以 HEAD 与该时点干净状态为准。

[原断言修后](evidence/service-bugfix-20260929-07/ORIGINAL-AFTER.json)：**8/8 pass**。Activity 原反例未改；购买 seam 只从 HSet 改为真实 Eval 成功后返回 DeadlineExceeded，保留独立 seed/control/catalog10→3 新二进制与必须仍读 Count10 的原断言。完整历史复现源码/红结果保留在第八轮，不让接口改名使故障失效后假绿。

新增正式定向共 **33 叶子/37 测试及子测试事件**通过。最终 `go test -tags integration -race -count=1 -timeout=180s -json ./versionstore ./kit/mods ./kit/service/... ./service/...`：**17 测试包、909 pass 事件、841 pass 叶子、0 test skip/fail/build-fail**；servicemetrics 无测试 package skip=1。初次默认运行 816 pass 事件、11 test skip 因 ROOST_REDIS_TEST_ADDR 未设置，已保留在结果文件；补齐此环境并开启 integration 后以上实际场景全部执行，不能把首轮跳过写成通过。

新消费工程来自实际本轮 CLI project new（449 文件）；最初依赖解析被 sandbox 网络阻止，**project new 整体当时 exit1**。消费模块使用本轮 Core 本地 replace 后，实际 CLI generate exit0；消费者 Platform/handler/game 回归 **107 pass 事件/100 叶子、0 test skip**，purchase 无测试 package skip1。整个新消费者与整个 Core 只编译通过；定向 vet 通过。未改框架 go.mod/toolchain 或声称公网依赖解析成功。

demo embed/步骤/解析门禁 5/5 通过；12 次 service RPC 只读检查按原 go:generate 的 cwd/参数执行，全部通过。先从根目录传不同 -dir 的比较曾因“Regenerate 命令注释”不同报 STALE；按源码声明复跑后消除，不登记成产品 bug，也未改生成文件。Go 模块缓存权限/VCS safe.directory 环境失败分别处理，使用已知 worktree 的临时进程配置，不改全局 Git。

[计数与日志摘要](evidence/service-bugfix-20260929-07/RESULTS.json) · [复跑](evidence/service-bugfix-20260929-07/README.md) · [源码内容](../review/evidence/service-review-20260929-09/SOURCE.json)。独占 Redis8.8.0 单机16429、三 master16426/16427/16428、16384 slots/state ok，无 replica/持久化；只使用测试 prefix。测试前/后身份用 baseline+working tree 内容摘要，不把测试时 HEAD 误称包含未提交修复。

## 升级和剩余工作

Activity 错 prefix 不自动加 tag；已有数据要迁移完整相关 keyspace/token/index，当前回归不等于旧跨 slot 数据已迁移。购买 collaborators 是用户业务 write-once 文件，旧生成项目须合并补丁；混合旧 producer 可以继续覆盖字段，必须升级全部投递 owner。已经覆盖的历史 goods 仅能依原 catalog/资产证明对账恢复。

永久 ClaimPurchase 与 HGetAll 保留；没有 fulfilled 拒写握手就不能安全按时间删 ledger。新 Mail RR-33 的候选复用现有 Pipeline，真实跨槽 proof 已补，但生产 Mail 未修改，见[第九轮交接](../bug/REVIEW-2026-09-29-services-09.md)。实际渠道/资产/allocator、HA、跨进程强杀、历史迁移与长稳容量未执行。
