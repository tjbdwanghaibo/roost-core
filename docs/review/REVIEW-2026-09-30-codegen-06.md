# 2026-09-30 Codegen 第六轮：RR-22～24 修复与正式消费者

本轮按用户 bugfix 授权修复[第五轮三个 P2](../bug/REVIEW-2026-09-30-codegen-05.md)，**3/3 已修复并在具名场景验证，尚未发版**。不是新增整域 review 或 Codegen 全域完成证明。工作起点干净 Core main，fetch/快进核对为 `4784ef820cab599e0b1321a96a5843af2ab95e58`；实际源码都在 Core。对应源码提交：[RR-22 886b4223](../bugfix/RR-20260930-22.md)、[RR-23 3f83ef08](../bugfix/RR-20260930-23.md)、[RR-24 2f68aa22](../bugfix/RR-20260930-24.md)。共享索引/机制/交接随本轮最后文档提交收尾。

收尾 fetch 发现远端到 `904d6d25`，另一工作线新增 Remote marker 前缀 RR-19、集成补测和文档；没有 Codegen 源码变化。本轮正常整合其提交并保留索引/CHANGELOG，不将 RR-19 当成自己的修复或独立验收。五个本轮 Go 文件的摘要确认整合后测试对象保持；合并后的真实生成工程再次编译通过，验证新依赖源码仍能被本轮消费者使用，见 merged-consumer-test.log。

## 行为与验收

编号映射：第五轮原 Codegen RR-12/13/14 → 当前 RR-22/23/24。远端另一工作线重新使用了旧编号；原报告、红绿日志和三次源码提交消息保持历史原样，修复文档及当前导航使用新编号，避免与 Nest/Entity 的独立问题混淆。

| 问题 | 最终行为 | 正式证据 |
| --- | --- | --- |
| RR-22 | usesStrconv 与 indexSpec 的 enabled 一致；false 不导入转换包 | 八 scalar false、无 index、string true 加 numeric false、混合 bool/uint64 true 四个生成消费包编译；修前两个失败，修后都通过 |
| RR-23 | 固定 wrapper/configdata 与条件 strconv 名预占，写入前拒绝冲突 | 五个 reserved name 拒绝且旧输出保留，三个不冲突 bean 真实编译；修前三个错误接受，修后通过 |
| RR-24 | 外层暂存迁移，先冻结明确迁移字节，解析模块后与依赖一起提交 | 成功迁移并隔离 runner 任意改写、三种失败前像保留、两种 root 并发修改保留；原成功断言从红变绿 |

新正式回归 **18 个叶子场景** 是上述具名场景之和，不能当成全部 Codegen 覆盖率。既有无 consolidation 隔离、模块策略/回滚、显式 consolidation/dry-run 等邻接回归也通过；它们和新增用例都进入全包 race。修前证据保留，未把旧 review 的失败改写成成功。

## 本机执行

- `go test -race ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings -count=1` 全部通过；具名 shell 项因 sh/shellcheck 不在本机 PATH 跳过，不称未跳过全绿。
- `go vet ./codegen/...` exit0。
- `go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync` exit0；只是静态契约检查，不声称重跑这些域的完整行为矩阵。
- 正式当前 roost CLI 创建临时工程，再人为给该临时工程加旧 kit require/import。真实 `project deps` 的 Go get/tidy 成功，Go/manifest/模块对账符合迁移承诺，生成工程 `go test ./... -count=1` 通过。local replace 指向本轮源码，本机 file proxy 为缓存，不是发布 tag 验证。
- Windows 以 native Go 命令执行 `scripts/test-sync-modes-generated.sh` 的等价正式 fixture 链：复制 syncmodes 输入，当前 DAO/Entity 生成器生成，再 `go test -mod=mod -race ./... -count=1 -v`；periodic/on_change 两项通过。没有改源码模板或依赖，也没有运行该 shell 的删除 trap。
- 修改的五个 Go 文件 gofmt 检查通过；证据/链接和暂存差异检查单独进行。测试二进制、模块缓存与临时工程留在 `.tmp`，不提交。

[原始证据和复跑命令](../bugfix/evidence/codegen-bugfix-20260930-12-14/README.md)含修前/后、正式消费者和全包日志、源码摘要。第一轮 RR-24 邻接测试受本机 GOTMPDIR 混合斜杠影响，原生路径重跑通过；未修改该旧断言。消费者实验初稿重复加入已有 legacy YAML 字段、并把 main 工程的额外文件写成另一 package，均是 fixture 配置错误；修正临时输入后迁移对账/最终编译通过，不登记产品 bug，也不把这些初稿失败用于 RR 修前证明。

## 图谱与范围限制

Tier 2，roost-core index_status ready，起点 HEAD 对齐 `4784ef82`；coverage generation `2026-09-30T11:58:14Z`。搜索 16 个目标同名函数无后续页，依赖 helper 双向 trace 含测试共四个 caller，精确 snippets 和九路径 coverage 完成；没有 recorded gap，但 metadata_changed。新三个测试文件的追加 coverage 为 not_tracked。均以当前源码/正式执行补证，不把旧 generation 当修后同代穷尽图；未重建共享服务或关闭别人的进程。

没有数据库、wire、公开 API 迁移；namespace 校验有明确收紧，冲突 schema 需改名。deps 的旧布局现在确实迁移必要 Go/manifest，映射外业务符号仍需手工处理。没有自动发布、部署或操作生产数据。

## 后续停点

RR-22～24 已在上述范围关闭，不能因此把整个 Codegen 标为完成。下一次 review 继续真实配置加载/required/ref/skipempty/索引值往返，显式 upgrade 的旧版本消费者、正式 Webroute 服务启动；强杀/磁盘/rollback 失败与旧客户端兼容仍未验证。本轮同步[进度](PROGRESS.md)与[机制学习](IMPLEMENTATION-CFGGEN-NAMESPACE-AND-DEPENDENCY-MIGRATION.md)，不替这些边界补写“已验”。
