# Codegen 第五轮复现材料

基线 `30085d628bc70eb89f051329b90020d652d97c52`，Windows / Go 1.27.0，源码范围及 SHA-256 见 [source-evidence.csv](source-evidence.csv)。本轮只留文档，不向生产包安装临时测试。

- [Run-Review.ps1](Run-Review.ps1)：在指定临时目录构建当前 cfggen，建立六个独立消费者，执行 tidy/编译；然后用 Go overlay 加入文档中的依赖断言。
- [dependencies_test.go.txt](dependencies_test.go.txt)：通过正式事务和既有 runner 注入点验证成功漏迁移/失败不改 root；不调用真实发布代理。
- [RESULTS.json](RESULTS.json)：八个场景原始退出状态/诊断，四个预期失败反例、四个通过控制。RR-13 的两种名称共享一个根因。
- [packages.log](packages.log)：`go test ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings -count=1` exit 0。
- [race.log](race.log)：`go test -race ./codegen/internal/cfggen ./codegen/internal/roost -run 'TestCfggen|TestFrameworkDependencyUpdate|TestUpdateFrameworkDependencies|TestConsolidat' -count=1` exit 0。

本机执行使用以下环境；已有下载缓存只作只读代理，独立缓存需要足够磁盘空间。其他机器正常可联网的 Go 环境可不设置 file proxy/GOSUMDB，并将 GoExe 改为自己的工具链。临时消费者用 local replace 引用 RepoRoot，这不是发布 tag 验证。

```powershell
$env:GOCACHE='D:/whb_s/.tmp/review-codegen-20260930/go-cache'
$env:GOTMPDIR='D:/whb_s/.tmp/review-codegen-20260930/go-tmp'
$env:GOMODCACHE='D:/whb_s/.tmp/review-codegen-20260930/gomodcache'
$env:GOPROXY='file:///C:/Users/tjbdw/go/pkg/mod/cache/download'
$env:GOSUMDB='off'
$env:GOTOOLCHAIN='local'
./docs/review/evidence/codegen-review-20260930-05/Run-Review.ps1 `
  -GoExe 'C:/Users/tjbdw/go/pkg/mod/golang.org/toolchain@v0.0.1-go1.27.0.windows-amd64/bin/go.exe' `
  -WorkRoot 'D:/whb_s/.tmp/review-codegen-20260930-05'
```

脚本 exit0 表示**预期失败被识别且控制通过**，不表示产品三项 bug 已修复。后续修复时 overlay 的期望行为应转绿，按新的验收目标运行，不能继续用“必须失败”的脚本总状态评价修后代码。脚本不自动提交、推送或删除任何项目。

| 场景 | 生成/入口结果 | 行为结果 |
| --- | --- | --- |
| RR12 index false | CLI exit0 | strconv 未使用，消费者编译 exit1 |
| RR13 RegisterConfigData bean | CLI exit0 | 与默认注册 wrapper 重声明，编译 exit1 |
| RR13 configdata bean | CLI exit0 | 与 import 名冲突，编译 exit1 |
| index true / 未声明 index | CLI exit0 | 两个消费包编译通过 |
| 嵌套 bean/切片递归/server group | CLI exit0 | 编译通过，client-only 表/字段省去；未加载真实 JSON |
| RR14 deps 自动合仓 | 函数 nil，stage 导入已迁移 | root Go/manifest 未迁移，期望行为 overlay exit1 |
| deps resolver 错误 | 返回指定错误 | root Go/manifest/go.mod 字节保留，控制通过 |

图谱记录：roost-core ready；初始 generation `2026-09-30T07:37:30Z`；最初已检查路径无 recorded gap 但 metadata_changed，完整回读当前 material 源码。cfggen 49 个函数查询及相关双向 trace 无后续页；依赖入口 trace 成功。显式 full index_repository、后续两条 snippet、含 CLI 的九路径补充 coverage 均在 300 秒超时；CLI 补充覆盖没有返回，按源码降级，不称同代全覆盖。sh/shellcheck 检查按上轮已知工具缺失跳过；真实网络代理、发布解析、进程强杀、磁盘故障和线上兼容均未验证。
