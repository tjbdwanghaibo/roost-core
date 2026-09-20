# U-0264：声明的框架版本下限是假的（C4，CI 发现，无 RR 编号）

## 问题

codegen 的 `framework-compat` 工作流在 `generated-consumer (minimum, …)` 两格红：

```text
internal/registry/generated.go:57:19: undefined: entity.ValidateEntityRegistry
game/entities/player/player_gen_wire.go:28:21: undefined: entity.EntityCategoryOther
internal/service/platform/collaborators.go:50:25: undefined: platform.PendingOrders
```

那一格的意思是"按声明的最低框架版本生成一个工程并编译它"。它红，说明**声明的下限是假的**：
生成器今天吐出来的代码在 core v1.14.0 / kit v1.13.0 上根本编译不过。

## 根因

C4：下限写在 `minimumVersions`，而"生成物用到哪些 API"分散在各个生成器里，两者没有任何联系。
生成器一路往前走（U-0221 的注册表校验、实体类别、U-0234 的 `platform.PendingOrders`……），
下限留在原地。`minimumVersions` 还会被当成**没有显式 pin 时写进 go.mod 的默认值**，
所以这不只是文档不准，而是会生成一个编译不过的工程。

## 方案选择

- **采用**：把下限抬到**实测能编译**的版本 —— core v1.15.7 / kit v1.14.8。
  不是猜的：用这两个版本分别生成 `minimal` 与 `full` 两个场景（`full` 还跑了 CI 里那九步 `add`）
  并编译通过。工作流的 `minimum` 一格同步改成同样的版本，于是"下限失真"下次仍然在那里红。
- 放弃 **让生成器按版本条件生成**：为了让旧框架编译而给每个生成器加版本分支，代价远大于收益。
- 放弃 **直接抬到当前发布集**：下限的意义是"最老还能用的框架"，抬到最新等于放弃这个意义。
  v1.15.7 是实测过的，就用实测值。

## 改动

- `internal/roost/manifest.go`：`minimumVersions` → core v1.15.7 / kit v1.14.8（codegen 不变），
  注释写清楚它和**合并边界**（v1.14.0 / v1.13.0，两模块布局那次）是两件事。
- `.github/workflows/framework-compat.yml`：`minimum` 一格的 `version_args` 同步。

### 一条被推翻的契约

`TestConsolidationMapMatchesTheGeneratorFloor` 原本断言"合并边界 == 生成器下限"。这两个数回答的是
不同问题：边界是**布局**变化的位置（低于它要 `upgrade --consolidate` 重写 import），下限是**当前生成物**
能编译的最老版本。生成器往前走了，布局没有。按规矩把老测试改成断言新不变量——
**下限不得低于边界**——而不是删掉它。

`TestUpdateFrameworkDependenciesUsesExplicitPolicies` 的固定版本改成引用 `minimumVersions`，
这样下一次抬下限不必再改它。

## 证明

- 修前：`generated-consumer (minimum, minimal)` 与 `(minimum, full)` 两格红，错误如上。
- 修后（本机复现 CI 的两格）：
  - `minimal` @ core v1.15.7 / kit v1.14.8 → 编译通过；
  - `full` @ 同版本 + CI 的九步 `add` → 编译通过。
- codegen 全量 `go test ./...` 绿；默认生成（不指定版本）仍然写出当前发布集并编译通过。

## 未做 / 边界

- **v1.15.7 / v1.14.8 是"实测可用"，不是"二分出来的最小值"**。真正的最小值需要逐版本二分，
  收益不抵成本；重要的是这个数现在有 CI 守着，失真会红。
- `game-demo` 模板跟随当前发布集，仍然被 `minimum` 一格排除——它需要的远不止下限。
- 抬下限意味着 pin 在更老版本的既有工程会被依赖策略拒绝（`versions.core requires >= v1.15.7`）。
  这是有意的：那些工程本来就生成不出能编译的代码，`roost project upgrade` 是它们的出口。
