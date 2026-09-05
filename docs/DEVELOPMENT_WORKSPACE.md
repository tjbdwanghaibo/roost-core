# 多仓研发与发布

Roost 研发期以五仓 source-head 联调为主，正式稳定后再固定发布版本。两种模式必须隔离：
workspace 解决研发效率，module/tag 证明外部用户可安装和复现。

## 研发模式：go.work

在五个仓库的共同父目录创建不提交的 `go.work`：

```bash
go work init ./roost-core ./roost-kit ./roost-skill ./roost-service ./roost-codegen
```

每个仓库应忽略 `go.work`/`go.work.sum`。业务仓库需要联调时，用 `go work use
./your-service` 临时加入。不要用 `go mod edit -replace` 把本地路径写进可发布 module；
workspace 已经提供相同的源码替换能力，而且不会污染版本元数据。日常联调不运行
`go work sync`；该命令可能把 workspace 选出的依赖版本写回各仓 `go.mod`，只有明确执行
跨仓版本收口时才使用并审查 diff。

**模块改名过渡期的例外。** workspace 只决定"用哪份代码"，MVS 计算模块图时仍会读取各仓
`go.mod` 里 require 的那个版本的 `go.mod`。模块路径从 `cube-*` 改为 `roost-*` 后、新 tag 发布前，
旧 tag 的 `go.mod` 仍声明 `cube-core`，会被拒绝。此时在 go.work 里加**指定版本**的 replace
（Go 不允许对 workspace 模块做无版本 replace）：

```
replace github.com/tjbdwanghaibo/roost-core v1.10.0 => ./roost-core
replace github.com/tjbdwanghaibo/roost-kit v1.10.0 => ./roost-kit
```

tag 发布后删除。它只存在于不提交的 go.work 里，不会进入任何可发布 module。

研发验收在同一个 workspace 下运行：

```bash
go test ./...
go vet ./...
```

并发、WAL、Remote Entity、Saga 变更还要对对应包运行 `go test -race`。Codegen 生成的
consumer 应加入临时 workspace 后编译，证明模板与四仓 source-head 同代。

## 打 tag 前：scripts/pretag.sh

```bash
./scripts/pretag.sh v1.11.0
```

**为什么需要它，而不只是 CI。** 由 tag push 触发的 workflow 运行在 tag **已经存在于
远端、且已可被 module proxy 缓存之后**。它能报告这个 tag 不可用，但阻止不了。
roost-core 就这样出现过一个已推送的 `v2.0.0`——module 路径没有 `/v2` 后缀，因此
任何消费者都选不到它：

```
go: ...@v2.0.0: invalid version: module contains a go.mod file,
so module path must match major version (".../roost-core/v2")
```

四仓各有一份相同的脚本，在创建 tag 之前检查五件事：

1. **tag 的 major 与 module 路径后缀一致**。Go 对 v2+ 要求路径带 `/vN`；不一致的 tag
   谁都选不到。这是上面那个事故的直接成因。
2. **tag 不能已存在**（本地或远端）。重打一个已发布的版本比打错版本更糟——proxy
   已经用那个名字缓存了旧内容。
3. **`go.mod` 没有 replace**，工作区干净。
4. **`GOWORK=off` 下能 build / vet / test**。workspace 恰好会隐藏消费者会撞上的
   那类依赖错误。

版本策略：**沿 v1.x 递增，不做 v2**。真要做 major 版本应当在一次有意的大重构里做，
并且同时改 module 路径后缀与全部 import——不是为了某一处语义修正。

## 发布模式：GOWORK=off

发布验证必须完全关闭 workspace：

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

Windows PowerShell 使用 `$env:GOWORK='off'`。发布顺序固定为 core → kit → skill → service →
codegen；后一层只能引用已经存在的正式 tag。每层发布后再执行 pure-tag 生成工程 smoke。
以下内容一律阻断 tag：

- `go.mod` 含本地 `replace`、pseudo-version 或尚不存在的版本；
- 只有 source-head workspace 组合能编译，`GOWORK=off` 失败；
- codegen 的最低版本、模板 import 与实际发布 API 不一致；
- 发布 workflow 没有显式关闭 workspace。

研发期尚未发布新 tag 时，Kit/Skill 的 standalone 检查可能因旧 tag 不含新 API 而失败。
这不阻断 source-head 开发，但它始终是“尚不可发布”的明确信号，不能被改成静默通过。

## 命名迁移

Core 的并发容器路径已从含义模糊且易与 Go 关键字混淆的 `roost-core/map` 改为
`roost-core/safemap`；类型名不变，常用 alias 仍可写成 `fmap`。升级 source-head 后重新
运行 codegen，或把业务 import 改为：

```go
import fmap "github.com/tjbdwanghaibo/roost-core/safemap"
```

该路径变化必须跟随下一次正式版本发布，不提供同时维护两套实现的兼容空壳。
