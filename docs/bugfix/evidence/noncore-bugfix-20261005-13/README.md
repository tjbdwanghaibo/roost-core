# NC-33 / NC-34 红绿与最终验证

基线 b2232db528de9947a2cf1cccaed79e7a9e29d2dc，2026-10-05，Windows Go1.27.0，GOWORK=off。没有真实外部服务验收或生产数据修改。

## 红与绿

先加入两份正式 promises 回归，在未修改产品代码的基线上运行：

```powershell
go test -count=1 ./cache ./remoteentity -run 'Test(ReplicaPayloadIdentityBeforeStoreMutation|ReplicaOptionalVersionAndDeleteRemainUsable|InterestPayloadIdentityBeforeRegistryMutation)$'
```

[red.txt](red.txt)、[red-exit.txt](red-exit.txt)：exit1；缓存2叶子、interest8叶子实际副作用失败，无VersionOf控制通过。正式 Replicator/BindSync 与进程内同步传输夹具组合，不把传输夹具当真实broker。修复相同命令exit0，见[green.txt](green.txt)。之后补null和generation兼容两项控制，最终13新增正式叶子全部通过。

## 最终矩阵

执行 [Run-Verify.ps1](Run-Verify.ps1)，源码最终身份见[摘要](../../../review/evidence/noncore-review-20261005-23/source-hashes.csv)。结果：[checks.json](checks.json)。

| 命令 | 结果 |
| --- | --- |
| go test -race -count=1 -json ./cache ./remoteentity ./sync/syncbus/... ./ownerroute ./kit/remoteentity ./entity | exit0；644通过事件，592通过叶子，8 skip，无失败 |
| go test -count=1 -json . | exit0；14叶子通过，无skip |
| go build ./... | exit0 |
| go vet ./cache ./remoteentity ./sync/syncbus/... ./ownerroute ./kit/remoteentity ./entity | exit0 |
| go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync | exit0 |
| 正式DAO+Entity生成双模式Sync，Run-GeneratedSync.ps1 | exit0；periodic/on_change两个叶子race通过 |

八个skip逐项保存在[race-summary.json](race-summary.json)：七项真实Redis门控、Remote finalizer投影负载一项；没有替换配置或删样本让它们变绿。统计区分父测试事件与叶子，不能按644推导新增覆盖或完整review比例。

正式生成消费见[日志](generated-sync.txt)、[退出码](generated-sync-exit.txt)、[复跑脚本](Run-GeneratedSync.ps1)。初次直接跑scripts/test-sync-modes-generated.sh失败：Git Bash把repo根写成`/d/whb_s/cube-core`，Windows Go无法解析go.mod中的replace；见[初次失败](native-bash-initial.txt)、[exit1](native-bash-initial-exit.txt)。随后使用PowerShell逐字保持同一fixture、正式DAO/Entity CLI与go test参数，仅将replace写成Go可识别的D:/路径。没有修改产品脚本或把初次失败算成产品红/验收通过；双模式生成消费不等于真实Remote外部资源集成。

本机 Go 路径 `C:/Users/tjbdw/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.0.windows-amd64/bin/go.exe`；依赖环境入口 `D:/whb_s/.tmp/bugfix-codegen-20260930-12-14/go-env.ps1`。完整JSON原始日志保留在 `D:/whb_s/.tmp/noncore-bugfix-review-20261005-23-final/`，仓库保存退出码、计数与具名skip，避免提交大日志。复跑在模块根执行：

```powershell
./docs/bugfix/evidence/noncore-bugfix-20261005-13/Run-Verify.ps1 -GoExe <Go1.27.0可执行文件> -Scratch <独占日志目录>
```

只复用本机依赖缓存，不要求重建共享图谱。不等待GitHub CI；未发版。源码摘要按UTF8并将CRLF规范为LF计算SHA256，哈希代表内容身份，不代表该文件的所有场景都审完。

## 整合最新App首批实现后的复核

最终基线包含c9b934ae/d4ac9853，原22证据路径哈希不变。在同一Run-Verify脚本增加`-IncludeAppSingleton -ResultPrefix upstream-`，日志保留于`D:/whb_s/.tmp/noncore-bugfix-review-20261005-23-upstream/`；[结果](upstream-checks.json)：race扩大包含app/kit-nest/kit-redis，708通过叶子/769通过事件/8skip，根包14与全仓build/vet/glsvet exit0。592是整合前原包矩阵，708是整合后扩大矩阵，不能将两者相加计算独立覆盖。

没有在本机跑singleton真实Redis/Cluster/进程接管；新feature只是第一阶段，完整独立验收边界见[review最后同步](../../../review/REVIEW-2026-10-05-noncore-23.md#最后同步app单实例锁第1阶段)。本轮修复T-214/215，保留上游T-212/213。

末次再整合e3810ef1的续期预算修正，使用同一脚本加`-IncludeAppSingleton -ResultPrefix upstream-final-`，完整日志在`D:/whb_s/.tmp/noncore-bugfix-review-20261005-23-upstream-final/`。[末次结果](upstream-final-checks.json)：709 race通过叶子/770事件/8skip，根包14与build/vet/glsvet exit0；新增作者的时间预算单测通过。592/708/709分别对应三次具名矩阵，不能累加。
