# NC-32 修复证据

2026-10-05，基线 `c3aa0eddf67be234ba742188c53d18bbc1be9604`，最终修改见[源码摘要](source-hashes.csv)与交付提交。[发现](../../../bug/RR-20261005-NC-32.md) · [修复](../../RR-20261005-NC-32.md)。没有发版、删日志或接触共享外部服务。

源码摘要的SHA256按UTF-8文本、CRLF→LF归一化计算，避免Windows checkout/rebase的换行转换产生假差异；不是对测试日志或任意二进制做归一化。远端0f554aed仅文档，上述Go内容与验证时一致。

## 红 → 绿

| 证据 | 结果 | 对照 / 判别 |
| --- | --- | --- |
| [正式金样修前](golden-red.jsonl) | 9 fail | 编译成功、vet0；`loaded child level=99 but commit records=0, want 1` |
| [修前正常对照](golden-controls.jsonl) | 2 pass | 原址nested BSON通知和DAO值roundtrip正常；不能替代加载后提交承诺 |
| [正式CLI修前消费](consumer-red.jsonl) | 6 fail / 17 pass | 六个深层Nest提交反例；上轮scalar/多DAO/CAS/进程恢复控制仍正常 |
| [正式金样修后](golden-green.jsonl) | 53 pass | 新正式9与既有44；race/vet0 |
| [修后CLI消费](../../../review/evidence/noncore-review-20261005-22/consumer.jsonl) | 28 pass | 原17、新深层提交6、新嵌套迁移4、新持续CAS1；无整包timeout/panic/race |
| [正式双模式Sync消费](../../../review/evidence/noncore-review-20261005-22/sync-modes.jsonl) | 2 pass | 正式DAO+Entity生成器 → Nest → periodic/on_change；vet0 |

修前在尚未更新的金样/CLI生成物上执行同一新增运行回归，核心依赖是基线已修NC-31的代码。产品反例不是编译失败、非法Nest用法或环境失败。初期夹具曾漏排除Redis文本金样、在事务外修改、将内层BSON.D当M，以及在投影票完成后立即读取瞬时Stats；这些失败排除，仅保留修正后的承诺红。原始退出码见对应 `*-exits.json`；空 `.log` 表示命令无输出，退出0有独立记录。

## 本地验证及环境失败

从模块根 `GOWORK=off`：

```powershell
& $GoExecutable build ./...
& $GoExecutable test -race -count=1 -timeout=180s -json ./codegen/... ./dataengine/engine ./dataengine ./migration ./nestwal
& $GoExecutable test -count=1 -json .
& $GoExecutable vet ./codegen/... ./dataengine/engine ./dataengine ./migration ./nestwal
& $GoExecutable run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync
```

build/root/vet/glsvet 均0，根包14 pass。[初次矩阵](tests.jsonl) / [原退出码](exits.json)如实保留 tests=1：codegen/internal/roost 的临时文件rename被沙箱拒绝，且PATH缺少sh。设置工作区TEMP/TMP与Git sh后，[沙箱重跑](roost-environment-retry.jsonl)剩余sh失败；单独执行sh复现MSYS `NtCreateDirectoryObject` 0xC0000022，非产品错误。

不改源码，在沙箱外配置相同Go缓存、工作区TEMP/TMP、Git/bin的sh，执行 `go test -race -count=1 -parallel=4 -timeout=180s -json ./codegen/internal/roost`；[最终重跑](roost-native-retry.jsonl) / [退出码](roost-native-exits.json) 为0，273 pass / 10 skip。按包替换该包的最后结果，其余使用初次已通过包，[合并统计](combined-summary.json) **988 pass / 0 fail / 11 skip**，不重复计重跑；1个WAL子进程helper、10个工具/外部环境相关skip逐名留档，不能宣称所有环境验收。历史其他轮17个外部skip也未关闭。

执行时使用本地Go1.27工具链与已有离线依赖缓存。scratch均在 `D:/whb_s/.tmp/noncore-bugfix-review-20261005-22-*`，没有删除旧证据；重跑需要新的空输出目录。

## 复跑入口与限制

[Run-Golden.ps1](../../../review/evidence/noncore-review-20261005-22/Run-Golden.ps1) 对齐 canonical `codegen/scripts/dao-golden-runtime.sh`，只排除原脚本同样排除的Redis文本生成物，保留全部runtime测试与daoruntime标签。[Run-Consumer.ps1](../../../review/evidence/noncore-review-20261005-22/Run-Consumer.ps1) 使用当前正式CLI，不手写冒充生成物。[Run-SyncModes.ps1](../../../review/evidence/noncore-review-20261005-22/Run-SyncModes.ps1) 是正式同名sh脚本的Windows等价流程。示例：

```powershell
./docs/review/evidence/noncore-review-20261005-22/Run-Golden.ps1 -GoExecutable $GoExecutable -OutputRoot D:/whb_s/.tmp/nc32-golden-new
./docs/review/evidence/noncore-review-20261005-22/Run-Consumer.ps1 -GoExecutable $GoExecutable -OutputRoot D:/whb_s/.tmp/nc32-consumer-new
./docs/review/evidence/noncore-review-20261005-22/Run-SyncModes.ps1 -GoExecutable $GoExecutable -OutputRoot D:/whb_s/.tmp/nc32-sync-new
```

后端mongotest不是Mongo服务端认证；没有真实Mongo replica set、Redis Cluster、网络未知提交、HA或容量验证，也没有性能benchmark。不等待/查询GitHub CI，不自动发布。
