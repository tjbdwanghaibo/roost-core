# RR-20260930-12～14 修前与修后证据

起点 Core `4784ef820cab599e0b1321a96a5843af2ab95e58`；五个最终 Go 文件见 [source-evidence.csv](source-evidence.csv)，源码收口 `2f68aa22`。Go 1.27.0 / Windows；仅临时消费者有 local replace，未改 Core go.mod/toolchain 或 go.work。

| 文件 | 实际结果 |
| --- | --- |
| [RR12-red.log](RR12-red.log) | 修前真实消费包两个失败（strconv unused）、两个控制通过 |
| [RR12-green.log](RR12-green.log) | 单项 race/四个真实消费包全部通过 |
| [RR13-red.log](RR13-red.log) | 修前三个 reserved 名未拒绝，另外两个固定名控制已拒绝 |
| [cfggen-green.log](cfggen-green.log) | 全 cfggen 包 race 通过，含四消费场景、五拒绝、三正常 bean 消费场景 |
| [RR14-red.log](RR14-red.log) | 原成功迁移 root 对账失败，五个失败/并发控制通过 |
| [RR14-green.log](RR14-green.log) | 六个新增迁移/失败/并发场景和既有邻接回归 race 通过 |
| [RR14-temp-path-mismatch.log](RR14-temp-path-mismatch.log) | 临时目录斜杠格式造成已有路径比较失败；仅改环境路径后重跑通过，不是产品 RR |
| [codegen-race.log](codegen-race.log) | 全 Codegen race 通过，明确跳过一个 shell 环境项 |
| [vet.log](vet.log) / [glsvet.log](glsvet.log) | 两条静态检查 exit0，无输出日志为空是正常结果 |
| [consumer-deps.log](consumer-deps.log) / [consumer-test.log](consumer-test.log) | 正式 project deps 真实 Go get/tidy、迁移回写与生成工程编译通过 |
| [merged-consumer-test.log](merged-consumer-test.log) | 整合另一工作线最新 Remote 源码后，真实生成工程再次编译通过；不冒称 RR-19 独立验收 |
| [sync-dao.log](sync-dao.log) / [sync-entity.log](sync-entity.log) / [sync-consumer.log](sync-consumer.log) | 正式 DAO/Entity fixture 生成及 periodic/on_change 两种 Sync race 通过 |

修前运行的是本次新建的正式测试，在生产修复前执行，不靠 stash 或修改旧失败。18 个新叶子只代表具名场景；全包通过仍不能证明所有 schema、升级版本或故障模式。

## 正式回归复跑

```powershell
$env:GOWORK='off'
go test -race ./codegen/internal/cfggen -count=1 -v
go test -race ./codegen/internal/roost -run 'TestFrameworkDependency|TestUpdateFrameworkDependencies|TestTidyProjectDependencies|TestConsolidat' -count=1 -v
go test -race ./codegen/... -skip TestDeployScriptsCarryNoKnownShellcheckFindings -count=1
go vet ./codegen/...
go run ./cmd/glsvet ./nest ./entity ./dataengine/engine ./sync/entitysync
```

本机额外设置独立 GOCACHE/GOTMPDIR/GOMODCACHE、GOPROXY 到已有下载缓存的 file URL、GOSUMDB=off、GOTOOLCHAIN=local，实际用 Go 1.27 二进制。Windows GOTMPDIR 用原生 `D:\whb_s\...`，避免已有路径比较用例混合斜杠。其他机器按实际网络/工具环境配置；有 sh/shellcheck 的 Linux CI 应不跳过 shell 用例。不要照搬本机 file proxy 地址到无该缓存的机器。

## 正式 deps 消费者步骤

选择新的独立临时目录；以下路径占位符只用于复跑说明。go mod edit 的 replace 只改消费者。generated manifest 本身已有合法 legacy policies，不要重复插入。

```powershell
go build -o <scratch>/roost.exe ./codegen/cmd/roost
<scratch>/roost.exe project new depsconsumer -module example.com/depsconsumer -out <consumer> -skip-deps -roost-core-version v1.18.0
# 切到 consumer：
go mod edit '-replace=github.com/tjbdwanghaibo/roost-core=<current-core-checkout>' '-require=github.com/tjbdwanghaibo/roost-kit@v1.12.6'
# consumer/business.go 必须与根 main.go 同属 package main：
# package main
# import "github.com/tjbdwanghaibo/roost-kit/mods"
# var _ = mods.ModBus
<scratch>/roost.exe project deps -root <consumer>
# 核对 business.go 是 roost-core/kit/mods、manifest 无 skill/service policy、go.mod 无 roost-kit require。
go test ./... -count=1
```

CLI 子进程通过 PATH 找 go，需要与所选 Go 1.27 工具链一致。Go get/tidy 和实际模块缓存使用真实工具，没有 resolver mock；local replace 限定本轮源码验证，不验证已发布 v1.18.0 包含这三项新修复。

Sync 在本机直接按 `scripts/test-sync-modes-generated.sh` 的输入与命令运行 native Go 生成/消费，不执行 shell 的删除 trap。fixture 来源 `codegen/internal/entity/testdata/syncmodes`；只复制其顶层 .go 到自己的临时模块，DAO 输入仍引用该目录下 def，生成顺序 dao → entity → Go race test，源码摘要/日志独立保留。

图谱：roost-core generation `2026-09-30T11:58:14Z`，九个既有路径 metadata_changed、三个新测试 not_tracked，均无 recorded gap、都回读当前源码；不作同代全覆盖声明。未测生产数据、发布 tag、真实配置全量组合、强杀/磁盘故障。
