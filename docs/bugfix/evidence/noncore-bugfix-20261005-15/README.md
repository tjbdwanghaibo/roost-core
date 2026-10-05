# NC-37/38 第十五批修复证据

2026-10-05，源码基线 `af2f67fbb75e34110f59e4603e0046eedf7e358d`；Windows/PowerShell、Go1.27.0、GOWORK=off。raw日志保留在本机独占 `D:/whb_s/.tmp/noncore-review-20261005-25`，本目录保存原红文本与小型结果，不提交缓存/二进制。

| 运行 | 通过叶子 | 行为失败 | skip | 结果 |
| --- | --- | --- | --- | --- |
| health修前 | 6 | 4 | 0 | 第三消费者缺失/退出与正式消费错误ok |
| Resume修前 | 0 | 2 | 0 | 正向/补偿丢代际、复用回执、再次waiting |
| 旧产品overlay | 6 | 6 | 0 | 两个生产文件恢复到基线，再现相同旧行为，exit1 |
| 最终新增正式 | 20 | 0 | 0 | 10健康、6代际、4生命周期；race |
| 最终相关矩阵 | 498 | 0 | 1 | 546通过事件；race |
| 根包 | 14 | 0 | 0 | 普通测试，门禁通过 |
| 全仓build/相关vet/glsvet | — | 0 | — | 三检查exit0 |
| 正式DAO/Entity双模式 | 2 | 0 | 0 | periodic/on_change；race |

[原健康红](health-red.txt) · [原Resume红](resume-red.txt) · [overlay红](overlay-overlay-red.txt) · [每次正式叶子清单](formal-results.json) · [矩阵](checks.json) · [生成双模式](generated-sync.txt) / [退出码](generated-sync-exit.txt)。不同运行不累加测试数，不输出全仓覆盖率。唯一skip为RemoteAsyncFinalizerProjectionLoad（未开启负载环境），不能算通过。

复跑先使用适用Go1.27工具链与依赖缓存/网络；从仓库根，GOWORK=off。已有脚本由本批保存，不自动修改代码、提交或推送：

```powershell
& docs/bugfix/evidence/noncore-bugfix-20261005-15/Run-Repro.ps1 -GoExe go -Scratch <new-scratch-red>
& docs/bugfix/evidence/noncore-bugfix-20261005-15/Run-Verify.ps1 -GoExe go -Scratch <new-scratch-matrix>
& docs/bugfix/evidence/noncore-bugfix-20261005-15/Run-GeneratedSync.ps1 -GoExe go -Scratch <new-scratch-generated>
```

Run-Repro预期exit记录为1，需同时核对六个行为失败文本，不能将编译错误/测试超时当红。Run-Verify保存实际命令/退出码与叶子，失败会中止；普通矩阵与新正式场景的清单分列。Collect-Evidence整理本机jsonl为文本/叶子清单与源码摘要，参数可换独占scratch；它不创建新验证证据。

生成检查复用正式syncmodes fixture与DAO/Entity CLI，以Windows路径等价执行已有Bash流程；只在独占工程写本地core replace，未改根go.mod/模板。不是Saga真实Mongo/NATS消费者启动验收，也未宣称历史B35默认生成依赖问题关闭。

后端Mongo/JetStream为替身；真实Mongo副本集、NATS ACK/断线/重连、旧writer混跑、历史waiting处置、HA和容量未执行。[范围/进度](../../../review/REVIEW-2026-10-05-noncore-25.md)，本地编译及适用验证收尾，不等待GitHub CI，未发版。

最终整合上游3f29a921发布记录和生成器下限；33材料源码LF摘要一致，原矩阵仍对应本轮修复源码。最终追加本地检查保存于[delivery-checks](delivery-checks.json)，和原498矩阵分列，不重复累加。NC-37/38在Unreleased，无本轮tag动作。

生成器首跑缺PATH中的sh，两部署语法检查环境失败、其他278叶子pass/10skip：[初次摘要](delivery-codegen-initial-summary.json) / [失败原文](delivery-codegen-environment-failure.txt)。保留原结果，不计Saga产品红；加入已有`C:/Program Files/Git/bin`与当前Go目录并设独占TEMP后重新运行，不改门禁。

同一完整包复跑280叶子pass/0fail/10skip、exit0：[复跑条件](delivery-codegen-retry.json) / [最终叶子及skip](delivery-codegen-final-summary.json)；[codegen vet](delivery-codegen-vet.json)exit0。十个skip保持外部/Windows限制，不因Bash已可执行就计通过；不是全codegen树重跑。与原498相关race矩阵、20新增正式场景分别报告。
