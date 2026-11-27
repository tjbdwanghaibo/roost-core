# SSR 平铺 AI 配置 → decision

`Import` 是**有界、显式方言、失败即不返回程序**的迁移入口，不是通用编辑器兼容层。适用来源是 SSR 提交 `854fe4f7d8492fe7e42fb143515b9cc814d5d99f` 的 `kingdom/modules/ai_engine`。不读取服务器配置，不安装模块，不自动接管任何 NPC。

```go
program, err := ssrimport.Import(editorJSON, ssrimport.Source{
    Dialect: ssrimport.AIEngine, Name: "example", Start: "alpha",
}, bindings, registry)
```

调用方核实原资源来源、启动阶段、CID 行和 **实际 worker、参数解析器** 后提供 `Bindings`。`decision.Registry` 另外限制事件、字段和输出意图的名字。同一个 `cid` 字符串按节点类别查表，要求正的规范十进制 `uint32`。包里没有源项目业务表；`testdata` 中的数字及名称均为人工设定。BT、关卡触发器和 AI 的节点数字会重叠，**不能从字节可靠推断来源**；来源不明不能改标 AI 后导入。

## 接受什么

| SSR 输入 | 保留的运行语义 |
| --- | --- |
| `node` 数组；可选空 `parameter`；`id` 正且全局唯一；`child` 是有序 ID 数组 | 显式具名 `root:true,type:0` 构成阶段。普通根子节点在 Tick 全部按顺序运行；根的直接事件子节点独立订阅，不参加 Tick。`unique_name` 只有根名称参与执行。 |
| `1 condition` | 调用方声明的纯 `int64` 快照字段，无左参数；右侧一个 `{type:"int",value:"十进制字面量"}`；精确支持 `eq/equal, lt/less, gt/greater, lte, gte`。 |
| `4 sequence` / `2 selector` | 决策子节点原序短路，与动作子节点分组；仅组合成功后按原相对次序运行附属动作。只有附属动作时 sequence 成功、selector 失败；空组合拒绝。 |
| `3 action` | `emit` 仅一个 `int` 字面量，保留动作节点 ID；`stage` 仅一个干净的 `string` 目标名，对应遍历结束时才生效的原引擎阶段请求。每阶段最多一处切阶段节点且目标不可为本阶段。 |
| `5 event` | `enable` 决定初始启用；`mode:true` 的一次性监听在一次同类型分派后消费，即便过滤为假。同拍事件是单独的即时 `Step`，不补 Tick。仅零参数恒真事件或恰一个 `int64` 参数的启用比较；每阶段每种事件最多一个监听器。跨不同阶段同 CID 可复用，同一个目标事件名称不能别名到不同源 CID。 |

**相等的前提：** 调用方按源事件顺序和参数签名提供完整字段；所有条件仅读取本次输入中冻结的字段，worker 只产生遍历期间不可被其他节点读到的输出意图，或者只调用延后切阶段。不可把会同步改世界/黑板、嵌套发事件、读取阶段 getter、运行脚本或依赖专用解析器的 worker 声明成 `emit`。该承诺必须审查源绑定，不能由数字 CID 本身证明。本接口不接受业务回调。超出签名的输入（如漏字段/事件参数数量不符）不在对照范围；原生 `Step` 可能原子报错而不是消费监听。展示用非根名称不翻译为语义。

**明确拒绝：** 其他/缺失方言、非空参数声明、未知字段（含大小写错误）、重复 JSON 键、`null`、未知节点或未绑定 CID、重复 ID/根名、断链/环/孤儿/共享节点、非法父子、参数类型/数量/比较符不匹配、非零 `delay` 或 `time`、同阶段同种事件多个监听、非事件上非默认 `mode/enable`、自切或同阶段多处切换、任意自定义 worker 类型。原版用 map 分派多个同类监听，没有固定同类先后；不强行选一个顺序。原定时器按 100ms 单位而非原生逻辑拍，不能只复制数字。源 action 的 `time` 是执行次数上限，不是时间戳，此子集也未映射它。导入先限制 1 MiB、1024 个平铺节点、48 层；目标 `Compile` 的独立上限仍适用。

## 证据与可重跑流程

源仓 `ssr/conv.sh` 从独立配置库的分支 `ai/*.json` 复制到 `cfg/editor/ai/`；`kingdom/modules/ai/internal/editor_stub_manager.go` 根据配置引用取文件并用当时的模板构树。`kingdom/cfg` 中是表结构/装载及生成枚举，`kingdom/modules/ai/internal/editor_{action,condition,event}.go` 提供 worker 注册，并非部署中具体树和匹配的模板行。在本次源检出（含忽略及未跟踪路径）和脚本/文档所指的本地配置位置**没有找到可配对的真实 editor/ai JSON 与 CID 行数据**；没有访问远端，也没有部署确认。不能由此推断生产没有此类资源。

[人工树](testdata/synthetic.editor.json)、[固定输入](testdata/inputs.json)、[源侧捕获](testdata/source-trace.json) 分开保存。golden **不是**从目标 `Step` 回填的：外部源侧进程从固定提交逐个提取并校验 [21 个原始文件的 SHA-256](testdata/oracle/source-sha256.json)，构树后复制引擎，调用原 `SetNextStage/OnTick/OnEvent`，输出每步活动根、活动监听 ID 集合及有序 `(动作节点ID, 名称, 载荷)`。另一路 `TestImportedProgramMatchesSourceTrace` 用同一批输入运行 `Import → decision.Program.Step`，逐步比较三类观察量。仅活动监听集合排序，**不排序输出动作**。

复现需要 Python 3.9+、本机 Go（目标仓要求见 go.mod）、Git 和含固定提交的本地 SSR 仓库。不需要私服、数据库、网络、第三方 Go 依赖或该服务完整构建。下面在 roost-core 工作树根执行；`SSR` 为含 `ssr/go.mod` 的外层 Git 根，`WORK` 为两个仓库外的私有目录。每次使用未存在的 replay 输出子目录。

```sh
D=gameplay/ai/decision/ssrimport
python3 "$D/testdata/oracle/replay.py" "$SSR" "$WORK/replay-01"
GOWORK=off GOPROXY=off GOSUMDB=off GOTOOLCHAIN=local GOMAXPROCS=2 \
  GOCACHE="$WORK/go-cache" GOMODCACHE="$WORK/go-mod" go test -p=2 -count=1 ./gameplay/ai/decision/ssrimport
```

第一条命令只读源 Git blob 并写到指定私有目录，默认比较既有 golden，不改写它。保留 `source-sha256.json`、`trace.json` 和 `stderr.log`。如仅运行第二条，它使用已捕获 golden，不声称本次执行了原引擎。共享工作环境应把两条分别置于已有串行测试调度器下，不要嵌套执行。

**替换边界：** 原 `ai_engine` 直接离线构建因缺 `easyjson` 及全量 `cfg` 依赖不可用。隔离回放用标准库解码**原结构和 JSON 标签**代替生成的 easyjson 解码器，仅对这份合法人工输入做对照；构树和参数解析仍为原版。外围 `cfg` 仅替换三个调试显示名称函数，`reflect2` 仅替换比较器显示名的指针标识，`wlog` 只提供日志类型/遇错误即失败；原比较器实现及事件过滤不变。原 `simple_timer` 未改动，但没有用该切片验证延时语义。运行上下文和模板组由[人工驱动](testdata/oracle/main.go.in)提供，发出意图时用原 `RTLog` 的动作节点上下文记录 ID。**不是完整服务运行或真实业务 worker 验收。**

固定提交中 `rootImpl.copy()` 不拷贝 `nm`：源 `CurStageName()` 返回空串，但实际根、事件和后续动作确实切换。为避免把请求的目标当成证据，源回放用原 `RangeRunDataTriggers` 报出的**活动 root ID** 对回原配置名。`stderr.log` 显示切阶段事件中自身后续动作仍处于旧根；返回时是新根，下一次 Tick 才执行新根的普通节点。不保证 getter 文本相等，也因该源差异拒绝自切阶段（可能重新开启已消费监听器）。

| 源回放观察 | 结果 |
| --- | --- |
| Tick 10；同拍 `pulse(6)` 后马上 `pulse(7)` | 第一次过滤失败但活动监听 `8` 已移除，两次均无输出。 |
| Tick 11，嵌套组合成功 | 输出节点顺序 `6,3,7`，不是扁平数组或父节点混排顺序。 |
| 同拍 `advance`，再发一次 `advance` | 先有序输出 `11,13`，该次返回活动根 `beta`；第二次无输出。此时未执行新根 Tick。 |
| 下一拍 Tick 12，随后同拍 `reset`、两次 `pulse(7)` | Tick 输出 `15`；切回 `alpha` 重建监听，仅第一个 `pulse` 输出 `10`。 |
