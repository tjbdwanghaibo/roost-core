# 单仓研发与发布

> **2026-09-21 重写。** 本页原来讲"三仓 source-head 联调"：core / kit / codegen 各是一个仓库，
> 开发期用 `go work init ./roost-core ./roost-kit ./roost-codegen` 把它们挂在一起，发布期再逐个对版本。
> [三仓合一仓](ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md)（core v1.16.0）之后这些都没有了：
> 一个仓库、一个 Go module、一个 tag。旧形态的记录留在
> [五仓合三仓方案](ARCHITECTURE_V2_CONSOLIDATION_PLAN.zh-CN.md) 与 `docs/history/`。

## 研发：不需要 workspace

框架的四层都在同一个 module 里，改哪一层都是同一次 `go build`：

```text
roost-core/
├── <core 包>   运行时：entity、nest、dataengine、room、saga、skill …
├── kit/        装配层：mods、app 生命周期与运维、service/ 的 12 个通用服务
├── codegen/    生成器与升级器（CLI 在 codegen/cmd/roost）
└── demo/       game-demo 模板
```

```bash
go build ./...        # 全部四层
go test ./...         # 115 个包
go vet ./... && go run ./cmd/glsvet ./...
```

**依赖方向由测试钉住**（`dependency_boundary_test.go`）：core 的包不得 import `kit/`、`codegen/`、`demo/`；
kit 可以用 core；codegen 独立于它生成的那个运行时。合仓之前这条规则由 Go 模块边界免费保证，
现在由目录前缀判定——所以它是一条**会红的测试**，不是一句约定。

## 唯一还需要 go.work 的场景：验证还没发布的改动

生成工程依赖的是**已发布**的 core。当你改了运行时或模板、想在发版之前看生成物能不能编译时，
用一个临时 workspace 把生成工程指到本地 checkout：

```bash
roost project new planet -module example.com/planet -out /tmp/planet -skip-deps
cd /tmp/planet
go work init . /path/to/roost-core
go work edit -go=1.27.0
go mod edit -droprequire=github.com/tjbdwanghaibo/roost-core   # 它指着一个还没发布的版本
go build ./...
```

`-skip-deps` 是必需的：生成物的 import 可能只存在于你本地这棵树里，让 `project new` 去 proxy 解析必然失败。
这套流程已经脚本化：

```bash
codegen/scripts/source-head-check.sh minimal   # 或 full
```

**go.work 永远不提交**（`.gitignore` 里有它）：提交一个 workspace 会让每个消费者被悄悄重定向到本地源码。

## 发布：一个 tag

```bash
./scripts/pretag.sh v1.16.2      # 门禁：主版本、tag 未存在、无 replace、工作树干净、
                                 # GOWORK=off 下 build/vet/tidy/test、清单版本 == 要打的 tag
git tag -a v1.16.2 -m "…" && git push origin v1.16.2
```

发布清单是 `codegen/ci/framework-release.yaml`，只有一行 `release`。它必须等于要打的 tag：
`pretag.sh` 在打 tag 之前比对，`release.yml` 在 tag 之后再比对一次并产出 `framework-lock.json`。
这条检查是有来历的——那个字段曾经漂了十个版本，把受保护的发布闸一起带红而没人发现（U-0270）。

发布之后 `go install github.com/tjbdwanghaibo/roost-core/codegen/cmd/roost@latest` 就是新的 CLI；
业务工程 `roost project deps` 升到新版本。

## 兼容承诺

一个仓库只有一个版本号，所以**只有 core 包的改动进兼容承诺**；`demo/` 与 `codegen/` 的改动
不构成框架行为变化，CHANGELOG 分节标明。判断要不要升级看 CHANGELOG 的分节，不要只看版本号跳了几位。
