# `/skillv2` 到稳定 `/skill` 包迁移手册

本次升级只保留一个核心 Go 包：

```text
github.com/tjbdwanghaibo/roost-skill/skill
```

旧 `/skillv2` 包、转发别名和双包兼容层均不保留。这样避免同一进程同时出现两套
`Program`、`Runtime`、`Host` 和状态类型，也避免 codegen 长期生成过时 import。

## 1. 哪些内容会变化

| 范围 | 旧值 | 新值 | 是否影响持久数据 |
| --- | --- | --- | --- |
| Go 目录/导入 | `roost-skill/skillv2` | `roost-skill/skill` | 否，源码需重新编译 |
| Go package 名 | `skillv2` | `skill` | 否 |
| JSON schema | `cube.skill/v2` | `roost.skill/v2`（v1.10.0 起；当时尚无旧数据，无需迁移） | 否 |
| compiler semantics | `skillv2-compiler-2` | 保持不变 | 否 |
| checkpoint/wire/outbox | 当前格式 | 保持不变 | 不需要格式迁移 |
| Go module | `github.com/tjbdwanghaibo/roost-skill` | 保持不变 | 否 |

协议身份保留是有意设计：`v2` 描述 wire/checkpoint 语义，不再污染稳定 Go API 名称。

## 2. 升级步骤

先升级到包含稳定 `skill` 包的 roost-skill release 和对应 codegen release，然后在业务仓库执行：

```powershell
rg -n "roost-skill/skillv2|\bskillv2\." .
```

机械替换：

```text
github.com/tjbdwanghaibo/roost-skill/skillv2
=> github.com/tjbdwanghaibo/roost-skill/skill

skillv2.SomeType
=> skill.SomeType
```

若使用 roost-codegen 管理工程模板，先运行：

```powershell
make project-upgrade
make generate
```

否则直接更新 module 并整理依赖：

```powershell
$env:GOWORK = "off"
go get github.com/tjbdwanghaibo/roost-skill@latest
go mod tidy
Remove-Item Env:GOWORK
```

## 3. 验证

```powershell
gofmt -w .
go vet ./...
go test ./... -count=1
go test -race ./... -count=1
rg -n "roost-skill/skillv2|\bskillv2\." .
```

最后一条命令应无结果；`skillv2-compiler-2` 和 `roost.skill/v2` 是协议常量，不是旧 Go import。

存在嵌套 module 时（例如 integration、examples、工具仓库），需要逐个目录执行
`go mod tidy` 和测试。根目录的 `go test ./...` 不会进入嵌套 module。

## 4. 发布与回滚

- 这是源码级破坏性升级，应提高 roost-skill 的发布版本并在 release note 中列出导入映射。
- codegen 必须先切换到 `/skill`，业务项目再运行 `project-upgrade`；不要让新项目继续生成旧路径。
- 因为 wire、compiler semantics 和 checkpoint 没变，新旧二进制可以按现有网络协议灰度；
  但同一个源码构建不能同时依赖 `/skillv2` 与 `/skill`。
- 回滚二进制不需要转换 checkpoint/outbox；回滚源码时恢复旧依赖和 import 即可。

如果后续修改 `roost.skill/v2`、`skillv2-compiler-2`、checkpoint 或 skillsync schema，
必须另写数据迁移方案，不能复用本次仅针对 Go 包名的结论。
