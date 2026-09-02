# 多仓研发与发布

Roost 研发期以四仓 source-head 联调为主，正式稳定后再固定发布版本。两种模式必须隔离：
workspace 解决研发效率，module/tag 证明外部用户可安装和复现。

## 研发模式：go.work

在四个仓库的共同父目录创建不提交的 `go.work`：

```bash
go work init ./cube-core ./cube-kit ./cube-skill ./cube-codegen
```

每个仓库应忽略 `go.work`/`go.work.sum`。业务仓库需要联调时，用 `go work use
./your-service` 临时加入。不要用 `go mod edit -replace` 把本地路径写进可发布 module；
workspace 已经提供相同的源码替换能力，而且不会污染版本元数据。日常联调不运行
`go work sync`；该命令可能把 workspace 选出的依赖版本写回各仓 `go.mod`，只有明确执行
跨仓版本收口时才使用并审查 diff。

研发验收在同一个 workspace 下运行：

```bash
go test ./...
go vet ./...
```

并发、WAL、Remote Entity、Saga 变更还要对对应包运行 `go test -race`。Codegen 生成的
consumer 应加入临时 workspace 后编译，证明模板与四仓 source-head 同代。

## 发布模式：GOWORK=off

发布验证必须完全关闭 workspace：

```bash
GOWORK=off go test ./...
GOWORK=off go vet ./...
```

Windows PowerShell 使用 `$env:GOWORK='off'`。发布顺序固定为 core → kit → skill →
codegen；后一层只能引用已经存在的正式 tag。每层发布后再执行 pure-tag 生成工程 smoke。
以下内容一律阻断 tag：

- `go.mod` 含本地 `replace`、pseudo-version 或尚不存在的版本；
- 只有 source-head workspace 组合能编译，`GOWORK=off` 失败；
- codegen 的最低版本、模板 import 与实际发布 API 不一致；
- 发布 workflow 没有显式关闭 workspace。

研发期尚未发布新 tag 时，Kit/Skill 的 standalone 检查可能因旧 tag 不含新 API 而失败。
这不阻断 source-head 开发，但它始终是“尚不可发布”的明确信号，不能被改成静默通过。

## 命名迁移

Core 的并发容器路径已从含义模糊且易与 Go 关键字混淆的 `cube-core/map` 改为
`cube-core/safemap`；类型名不变，常用 alias 仍可写成 `fmap`。升级 source-head 后重新
运行 codegen，或把业务 import 改为：

```go
import fmap "github.com/tjbdwanghaibo/cube-core/safemap"
```

该路径变化必须跟随下一次正式版本发布，不提供同时维护两套实现的兼容空壳。
