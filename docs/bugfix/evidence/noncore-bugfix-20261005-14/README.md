# 第十四批：权威快照后置条件与N05恢复验证

2026-10-05，基线40d89ac68b8a2247173e94ef55c02fdf23da1d6f，Windows Go1.27.0/GOWORK=off。NC-35/36及旧RR-20260913-08残余补修已验证，未发版；[最终源码身份](../../../review/evidence/noncore-review-20261005-24/source-hashes.csv)。

## 真实红与绿

先增加正式测试、未改产品代码：

```powershell
go test -count=1 -v ./entity -run 'TestAuthoritative(LoaderRejectsForeignKeyBeforePublish|ReadChecksStoredMinimumVersion|ReadRechecksExpiryAfterL2Publish)$'
go test -count=1 -v ./remoteentity -run '^TestSnapshotReplicaAuthoritativeRepairAdmission$'
```

[red.txt](red.txt)/[exit1](red-exit.txt)保留12读取反例：异键8、最终最低版本2、发布跨期限2；[consumer-red.txt](consumer-red.txt)/[exit1](consumer-red-exit.txt)为正式接收6叶子2fail/4pass。没有把构建失败、钩子未到达或超时冒作产品红。

修后定向race：[green.txt](green.txt)/[exit0](green-exit.txt)。含第二订阅失败重试共19新正式叶子，检查最终状态及合法恢复。最终完整矩阵基于摘要所列最终测试源码（有效期恢复夹具由构造时闭包控制，无运行时更换loader）。

## 最终矩阵与复跑

[Run-Verify.ps1](Run-Verify.ps1) / [checks.json](checks.json)：

| 命令 | 结果 |
| --- | --- |
| go test -race -count=1 -json ./cache ./remoteentity ./sync/syncbus/... ./ownerroute ./kit/remoteentity ./entity ./app ./kit/nest ./kit/redis | exit0，733叶子/800通过事件，8skip |
| go test -count=1 -json . | exit0，14叶子，无skip |
| go build ./... | exit0 |
| go vet 上述受影响包 | exit0 |
| go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync | exit0 |
| 正式DAO/Entity CLI生成后go test -mod=mod -race -count=1 -v ./... | exit0，periodic/on_change两叶子 |
| go test -count=1 -json ./codegen/... | exit0，620叶子/666通过事件，10环境skip |
| go test -race -count=1 -json ./kit/service/global/... ./kit/etcd/... ./kit/mods/... | exit0，133叶子/141通过事件，2Cluster环境skip |

race八skip、codegen十skip逐项在 [race-summary](race-summary.json) / [codegen-summary](codegen-summary.json)；外部Redis、负载及Windows shell/工具缺口没有被控制测试替代。codegen为单测整合验收，无race/生成服务真实启动证明。[上游API删除补充race](upstream-api-summary.json)另记，不合并为本批新增覆盖。

正式双模式消费的[脚本](Run-GeneratedSync.ps1)、[日志](generated-sync.txt)、[exit](generated-sync-exit.txt)采用同一syncmodes fixture及正式两条CLI，仅将原Bash的 `/d/...` replace 换成Windows Go可识别的D:/绝对路径。最初误把外层Tee再写到脚本自写的generated-sync.txt导致文件占用，原取证日志保留 `D:/whb_s/.tmp/noncore-bugfix-review-20261005-24-generated/initial-outer-tee-log.txt`；移除外层Tee并在全新generated-final目录完整复跑，最终exit0。不将取证命令错误记成产品缺陷。

完整JSON日志：`D:/whb_s/.tmp/noncore-bugfix-review-20261005-24-final/`；codegen/API两份 `.log` 在同名24前缀临时目录旁；生成消费在 `D:/whb_s/.tmp/noncore-bugfix-review-20261005-24-generated-final/`。大日志/二进制不提交。

```powershell
. D:/whb_s/.tmp/bugfix-codegen-20260930-12-14/go-env.ps1
$roostGo='C:/Users/tjbdw/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.0.windows-amd64/bin/go.exe'
./docs/bugfix/evidence/noncore-bugfix-20261005-14/Run-Verify.ps1 -GoExe $roostGo -IncludeAppSingleton -Scratch <独占日志目录>
./docs/bugfix/evidence/noncore-bugfix-20261005-14/Run-GeneratedSync.ps1 -GoExe $roostGo -Scratch <新的独占生成目录>
```

codegen测试需要Git/bin和Go工具目录在PATH，TEMP/TMP为独占可访问目录；本次在自动审查允许的本机子进程权限下运行。所有检查本地完成，不查询/等待GitHub CI。只关闭自己的测试实例，不触碰共享集成环境；未证明真实broker ACK/retry/断线恢复、跨节点删除水位、HA、时钟回拨或长期容量。

## 末次赠礼生成消费

正常rebase整合另一线5bdac773/64acd782后，原15源码摘要逐一核对一致；再次全仓build、根包14、codegen/internal/roost vet exit0，见[latest-checks](latest-checks.json)。本段只验证当前生成消费，前面620 codegen普通叶子属于40d89ac6，没有声称合并后全包重跑。

使用[Run-GeneratedDemo.ps1](Run-GeneratedDemo.ps1)正式CLI独占生成game-demo，GOWORK=off、require仍为生成的v1.18.0引导值，replace指向本地core。首次CLI未用skip-deps，文件已生成但file GOPROXY缓存缺v1.18.0.mod，依赖解析失败，原日志在 `D:/whb_s/.tmp/noncore-bugfix-review-20261005-24-generated-demo/generate.log`。随后用已有skip-deps选项，全新目录完整生成，不冒认公开tag兼容验证。

原始生成依赖下，玩家/赠礼消费race89叶子/101事件通过、2真实Mongo门控skip，但完整build出现genproto旧单体2019与拆分api/rpc重复包，见[初次结果](generated-demo-initial-checks.json)、[真实构建失败](generated-demo-initial-build.txt)。它是[历史B35](../../../review/OPEN-ITEMS-2026-09-27.md)已经留档的依赖整理问题，本次Windows GOWORK=off + local replace仍能触发，不重复造RR，也不把这次原始build写成通过。

只在独占生成工程内显式 `go get google.golang.org/genproto@v0.0.0-20251202230838-ff82c1b0f217`（本机已有缓存，采用已拆分模块），[依赖变更日志](generated-demo-deps-recovery.txt)。没有修改框架go.mod、生成go.mod模板或产品门禁。整理后重新执行消费race/build/vet，见[结果](generated-demo-checks.json)。这证明的是**显式整理依赖的本地HEAD生成工程**，不能证明未整理依赖、默认网络解析流程或公开tag的编译全部正常。

最终整理依赖后的消费 **89叶子/101事件通过，2真实Mongo门控skip**，生成工程全仓build/vet均exit0。中间一次重试遗漏已批准的socket子进程权限，四个真实loopback连接被sandbox拒绝，85叶子通过/4失败；[中间摘要](generated-demo-sandbox-checks.json)、[具体connectex权限错误](generated-demo-sandbox-errors.txt)保留。恢复与首次相同的本机socket权限、测试/配置不变，才得到最终89通过。没有将权限失败当产品红，也没删这四个样本让结果变绿。

复跑可在新目录显式选择同一依赖整理，参数未提供时脚本保留原始依赖行为：

```powershell
./docs/bugfix/evidence/noncore-bugfix-20261005-14/Run-GeneratedDemo.ps1 -GoExe $roostGo -Scratch <新独占目录> -GenprotoVersion v0.0.0-20251202230838-ff82c1b0f217
```

最新消费原始/整理后两次不是两份新增覆盖；2个Mongo skip不关闭，未启动真实玩家进程。完整生成文件与原始日志在 `D:/whb_s/.tmp/noncore-bugfix-review-20261005-24-generated-demo-final/`。额外CLI flag字符串及实际分支按当前源码核对，见[补充coverage](../../../review/evidence/noncore-review-20261005-24/cli-coverage.json)，旧图谱用源文补证，不把它加进N05产品分母。

## 推送重试后的最终基线

又整合c493a791/364b763c/10e2e0ea，关键源文增量与兼容边界见[review](../../../review/REVIEW-2026-10-05-noncore-24.md#推送重试同步10e2e0ea)。原15N05摘要再次核对一致，最新core build、根包14、codegen vet、正式etcd前缀测试exit0：[delivery-checks](delivery-checks.json)。

按上面脚本带显式GenprotoVersion，在全新 `D:/whb_s/.tmp/noncore-bugfix-review-20261005-24-generated-demo-latest/` 生成并验最新game-demo：[最终generated-demo-checks](generated-demo-checks.json) 为91消费race叶子/104事件/2Mongo skip、build/vet exit0；[64基线结果](generated-demo-64-checks.json)仍为89/101，不将两次累加。作者新phase/topic两叶子独立执行通过，不是本批新增19项。最新完整生成文件/原始日志保留该latest目录。

作者第5笔真实演练只是接手记录，本机未重跑；原始默认生成依赖B35失败仍单列，不能用显式整理依赖后的通过替代。未修改模板依赖或生产数据，不等待CI、不发版。

最后又整合fdcd8605的etcd停机补修，新增[etcd-checks](etcd-checks.json)：App/etcd-driver普通race162叶子/173事件通过，核心build/根包14/vet通过。真实etcd文件有integration tag，未编入该命令，0skip不等于真实etcd通过；新四种LeaseNotFound/其他错误后重试/停止中重注册及App普通Stop错误释放控制独立执行，作者红与真实资源未重做。原15N05源码身份与赠礼模板不变，91生成消费沿用前次，不累加覆盖。

最终正常整合f8bb0261的新登录claim/赠礼预算/卸载测试，15N05源码摘要不变；[last-checks](last-checks.json)保留当前全仓build、根包14、codegen vet均exit0。新模板已整合但未本轮逐项独立审查/生成消费重跑，91消费属于10e2e0ea，不把它写成最终f8bb0261全模板验收。详细范围见[交付记录](../../../review/REVIEW-2026-10-05-noncore-24.md#交付范围冻结与最后整合f8bb0261)。
