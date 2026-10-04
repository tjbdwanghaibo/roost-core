# N04 第21轮：可复跑生成消费

2026-10-04 开始 / 10-05 完成；源码基线 3127d37c + 本轮 NC-31 修改，字节身份见[source-hashes.csv](source-hashes.csv)。[运行结论](../../REVIEW-2026-10-05-noncore-21.md)。

## 本机复跑

使用仓库 Go 1.27.0 工具链；模块缓存按本机环境准备。脚本每次要求新的输出目录，正式编译 DAO CLI、生成两个 DAO、独立 module replace 当前仓库，再 race/vet；任一行为失败脚本返回失败。无需真实 Mongo / Redis，不启动共享服务。

```powershell
& ./docs/review/evidence/noncore-review-20261004-21/Run-Consumer.ps1 `
  -GoExecutable '<Go1.27/bin/go.exe>' `
  -RepoRoot 'D:/whb_s/cube-core' `
  -OutputRoot '<新的绝对scratch目录>'
```

[定义](consumer-definition.go.txt)、[原11项与通用夹具](consumer-test.go.txt)、[多DAO/CAS/真实子进程恢复](aggregate-test.go.txt)、[脚本](Run-Consumer.ps1)。测试进程只强杀自己创建的子进程；临时 WAL 位于该测试 TempDir。生产状态/未知锁不参与。

[最终事件](consumer.jsonl)：17 叶子通过，0 fail/skip/race/panic/整包timeout；[退出](exits.json) test=0/vet=0，[汇总](summary.json)。其中 6 项为本轮新场景，不把 17 都算新增。

WAL 是真实文件，进程确实在 CommitSystem strict 返回、投影受控阻塞后被终止；重开同一目录检查一笔原日志，再正式投影/新 Manager 装载。后端是框架 mongotest，父进程重建相同未投影旧文档；不能推定真实 Mongo 事务、跨进程数据库持久性、网络错误或 HA 已验证。

[本地检查](checks.json)对应build/vet/glsvet/根包，完整包/根事件归档于[修复证据](../../../bugfix/evidence/noncore-bugfix-20261004-11/README.md)。本轮不等待或查询 GitHub CI。

## 源码补证范围

[coverage](coverage.json)：project roost-core，generation 2026-09-30T11:58:14Z。16 源码材料 hash + 新夹具 coverage；已有路径 metadata_changed，新文件 not_tracked，均不是“最新图谱已证明完整”。

本轮全文：Runner、两个 Runner 测试、EntityRepository、dataengine/migration、mongo_load；Repository 测试读 DAO/注册/迁移/重试范围，Kit 集成读真实迁移消费/装载方法；template_dao 读 Migrate/Unmarshal/RestorePersisted；MongoStore 读 applyPut/documentInt64/classifyNoMatch，mongo_projection 读 ProjectFenced 单笔 migration conflict；Projector 读 New/CommitSystem/Flush/Close，projector_replay 读失败/完成/ack；nestwal 读 Open/Append/Replay/openActive/recoverTail。其余原机制只作为前轮证据复用，不计本轮全文新阅读或穷尽审计。

LoadEntity 双向 depth2 trace 的两页已翻完；图谱中 NewMigrationRunner 跨包错误调用边以及动态接口漏边未作为事实。当前源码与正式消费者编译/执行补证，不使用旧图谱零caller证明无人调用。

## 夹具更正

1. BSON int32 ID 最初误列拒绝，核对 documentInt64 后保留兼容正常对照。可信红基线另跑，见修复证据的5红/3对照；初次错误断言未计 bug。
2. 初次 crash fixture 以 DurableLSN 判 strict Append 就绪失败；该水位只对应 Enqueue 票据。最终改为 CommitSystem 返回事件和重开日志，才实际执行强杀/恢复。初次夹具失败不是产品回归证据。
