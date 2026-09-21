# 三仓合一仓：给 review 的交接

> 状态：2026-09-21，P0–P5 全部完成，**core v1.16.0 已发布**（当前 v1.16.1）。
> roost-kit 与 roost-codegen 已归档，最终版本 v1.14.18 / v1.15.32，旧 tag 永远可用。
> 本文是交接文档：给要审查这套代码的人（或 agent）。方案与逐阶段门禁在
> [ARCHITECTURE_V3_SINGLE_MODULE_PLAN](../ARCHITECTURE_V3_SINGLE_MODULE_PLAN.zh-CN.md)，
> 这里只写**审查时需要知道的事**：哪些东西动了、哪些没动、已经查过什么、还剩什么没查。

## 1. 发生了什么

一个仓库、一个 Go module、一个 tag。三个仓各自作为 core 根目录下的**一个顶层目录整体搬入**
（`git subtree`，带完整历史），包结构一行未动：

```
roost-core/            142 包
├── <core 包> 84       运行时
├── kit/      28       装配层 + kit/service/ 的 12 个通用服务
├── codegen/  30       生成器（CLI 在 codegen/cmd/roost）
└── demo/              game-demo 模板（198 个 .tmpl）
```

**审查时最该记住的一条**：这是一次"只移动不优化"的搬迁。除下面第 3 节列出的几处，
任何行为变化都不是搬迁的一部分——看到行为差异优先怀疑是别的单元干的。

## 2. 审查这套代码时，判据在哪里

| 想确认什么 | 用哪个工具 | 不要用什么 |
| --- | --- | --- |
| 层次依赖（core 不得依赖 kit/codegen） | `dependency_boundary_test.go`（按目录前缀判定，编译器级）或 `go list -deps` | **不要用知识图谱**：它会把同名符号连成 USAGE 边，出现过 `room_transport_sink.go`"调用"一个 `.json` 的结果 |
| 生成物是否是规范形态 | 在对应目录跑 `go generate ./...` 后看 `git status` | 肉眼比对 |
| 工作流里的 `./path` 是否存在 | `TestCIWorkflowPackagePathsExist`（覆盖所有只在本仓跑的工作流） | —— |
| 生成工程能不能编译 | `codegen/scripts/source-head-check.sh minimal\|full` | —— |

合仓后 `dependency_boundary_test.go` 是**唯一**保证层次的东西（合仓前这条由 Go 模块边界免费提供）。
它先于任何代码搬动写好并用探针验证过会红；审查时如果要改它，先确认新判据同样能拦住反向 import。

## 3. 搬迁**确实**改了行为的几处（其余都只是位置变了）

1. **生成的 go.mod 只写一条 require**：`roost-core/kit` 是包路径不是模块路径，向 `go get` 要它等于
   要一个不存在的模块。`project deps` 同理。
2. **`--skip-deps`**（`project new` / `sync` / `upgrade` 新增）：边界迁移有一段窗口——改写出来的
   import 已经正确，而没有任何 proxy 能解析它们。这个开关把"改写文件"和"解析依赖"分开。
3. **`upgrade --consolidate` 多了第二段**：两条前缀规则（`roost-kit/*` → `roost-core/kit/*`、
   `roost-codegen/*` → `roost-core/codegen/*`），**跑在第一阶段包映射表之后**。顺序是正确性本身：
   上一轮把 kit 的大部分实现搬进了 core 本体，前缀先跑会把 `roost-kit/dataengine` 送进
   `roost-core/kit/dataengine`——那里没有这个包。有专门的测试与变异验证钉住这个顺序。
4. **发布清单收敛成 schema 3**（一行 `release`），`Admit` 之外的发布链只剩 `scripts/pretag.sh` 一处门禁。
5. **生成器下限抬到 v1.16.0**，`minimumVersions.Kit/Codegen` 成为历史字段（老工程的 roost.yaml 还带它们）。

## 4. 已经查过的（不必重查）

- **重复逻辑**（2026-09-21 全仓扫描，按归一化函数体指纹）：
  - 逐字节相同的文件：**0**（三份 gapmap.sh 与两份已失效的 pretag.sh 已收敛）；
  - 生产代码**跨层**重复：**2 组**——每个服务各自的 `Error() string`（Go 惯例）与一个 5 行的
    `valueOrDefault`/`stringDefault`。**搬迁没有制造重复**，这正是"整体搬目录、不打散"换来的；
  - 跨层同名导出符号 72 个（`Config`/`New`/`Mod`/`Server`…）全是每包一份的惯例名，不是重复。
- **层次依赖**：`go list -deps` 实测三条都成立（core↛kit/codegen、kit↛codegen、codegen↛运行时）。
- **全量门禁**：build / vet / `vet -tags integration` / glsvet / test（115 包）/ `test -race` 全绿；
  用**已发布的** v1.16.1 生成 game-demo 工程，无 workspace 直接 build + test 通过。

## 5. 查出来但**没有修**的，留给 review 定性

- **测试侧的重复是真的存在**，且都落在上一轮"领域半边 / 装配半边"的切口上：
  - `fakeBus`（会真的做序列化的测试替身）在 `service/mail/rpc_test.go` 与
    `kit/service/mail/rpc_test.go` 各写一份，逐字相同；`mirror/envelope_test.go` 还有第三份变体；
  - `assertWALReplayIDs` / `assertWALReplayCount` 在 `dataengine/engine/projector_test.go` 与
    `kit/dataengine/real_integration_test.go` 各一份；
  - errcode 契约测试（`TestTheCodeSegmentIsExactlyAsAllocated` 等四个）在 8 个服务包里逐字相同。
  这些**早于本轮搬迁**（是五仓合三仓拆两半时留下的），本轮只是让它们出现在同一棵树里第一次看得见。
  要不要收敛成共享 testing 包，是个取舍：共享替身会让"替身比实现更宽容"的风险集中化。
- **同层内的小工具重复**（均早于本轮）：`snakeCase`（`robot/action` 与 `configdata`）、
  `isNilInterface`（`etcd` 与 `nettransport`）、`toSnake`×4 与 `parseKV`×2（codegen 各 generator）、
  skill 的 `checkedInt64Mul` / `saturatingInt64Add` / `containsTag`（`skill/combat` 与 `skill/combatcomponent`）。
- **本仓 CI 不校验自己的生成物**（只在生成工程那侧 `generate --check`）。这次是手动重生成才发现
  合仓以来生成物一直不是规范形态（import 顺序）。加一个 job 是独立一件事。
- **故障矩阵与性能对比没做**：`framework-compat` 的 demo 格含 `dev compose` 真实启动 + 机器人，
  但 kit 的 `scripts/integration` 故障切片没在合仓后重跑；也没有迁移前后的同机基准。
  搬迁不改运行时代码路径，所以预期无差异——但**没有证据，不要说成验过**。

## 6. 搬迁过程中踩到、值得记住的三件

1. **"路径即数据"**：盲目的前缀替换会打到**断言与映射表**。上一轮 5→3 的迁移映射表
   （`consolidation_imports.yaml`）里的旧路径**是数据本身**，改了就等于废掉老工程的升级路径；
   `TestForbiddenCoreImport` 里的旧模块路径同样是断言。都已还原。反方向也有：
   不带域名前缀的 `roost-kit/service/...` 反而没被规则盖到，要手工跟上。
2. **前缀替换会造出死代码**：`roost-core/kit/` 是 `roost-core/` 的子集，glsvet 里那条判断从此永不触发。
3. **护栏要先于搬迁**：`TestCoreContractsDoNotLinkDrivers` 的注释早就写着"assembly 就是链接驱动的地方"，
   而 kit 搬进来后这个 walker 会扫到每一个 Mod——每个 Mod 都会被一条**自己注释里已经豁免它们**的
   测试判红。P1 先改好它，这条才没有在 P2 当天被误读成"搬错了"。

## 7. 与 review 线的接口

- 合仓期间登记的两条 Wanted 已分流（W-2026-09-20-05 → RR-20260921-01，W-06 → RR-20260921-02），
  两条都已修复并随 v1.16.1 发版。
- 搬迁批次**不修 Bug**：过程中看到的缺陷写进 `docs/bug/WANTED.md`，不在批次里顺手改。
  本轮按这条执行了两次（活动窗口、demo-publish），事后证明是对的——两条的根因都比第一眼看到的更深。
