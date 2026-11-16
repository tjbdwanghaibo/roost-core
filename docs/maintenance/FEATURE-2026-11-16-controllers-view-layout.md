# 方案：controllers 合成一个包，组件接口统一收进 view（2026-11-16）

状态：**方案，未实施。** 方向由维护者在 2026-11-16 提出并确认（见第 1 节）。第 6 节的验证点在实施时先做。

适用范围：生成工程，包括模块模板、生成器、game-demo 中由模块和生成器带来的部分、genre 夹具。线上未部署，旧生成工程不做兼容，照 CHANGELOG 手动迁移。

## 1. 维护者定的方向

1. **controllers 合成一个包。** `game/controllers/` 下只有一个公共的 `controller.go`，不再按模块分子目录；每个模块的业务都写在一个文件里，不再一条协议一个文件。
2. **窄接口返回 view 里的接口，不返回具体组件结构体**（比如不再返回 `*GameClockComponent`）。view 是最底层的包，这样就没有循环依赖。
3. **activity 统一用一个组件类型号。**
4. **实体为组件提供的方法（下称"宿主方法"）也在 view 里抽成接口。** 做法是：
   - 宿主方法加进组件的窄接口；
   - 实体总接口嵌入这些窄接口；
   - 实体负责实现。

组件里的钩子改成实体事件（新增第七个固定层 `game/event`），单独写在 [FEATURE-2026-11-16-tx-event](FEATURE-2026-11-16-tx-event.md)，两份方案一起实施。

## 2. 现状

### controllers

- 30 个模块各带 `game/controllers/<模块>/controller.go`：
  - 约一半只是同一段样板：持有 Registry 和 Nest 客户端，加 `NestClient()`、`Registry()` 两个方法，各份只差错误信息里的模块名；
  - 另一半在样板之外多持有自己的依赖，比如 mailbox 的 mail 客户端、chat 的在线名单和推送面。
- 协议生成器（`codegen/internal/protocol/controllers.go`）：
  - 包里没有 `Controller` 时，补写同样的样板；
  - 每条协议单独写一个存根文件（`bag_discard.go`）。
- **为什么现在每个包都要有自己的 controller.go**：`HandleXxx` 是 `*Controller` 的方法，Go 要求方法和类型在同一个包里，所以只要还分子目录，就得各声明一次。
- 一个模块拆成多个文件的例子：chat 有 5 个，room 有 7 个，combat、alliance 各 4～5 个。

### entities/<实体>/<x>_component.go

一共 39 个，每个文件放了：

1. `//roost:component type=N` 标记和 `CompTypeX` 常量；
2. 类型别名 `XComponent = x.Component`；
3. 窄接口 `IXEntity`，返回具体组件类型，例如 `ProfileComp() *ProfileComponent`；
4. `init()` 里的 `entity.RegisterComponentFactory`；
5. 其中 11 个还有宿主方法，见第 4 节。

另外两点现状：
- view（v1.47.0 起）已经有每个组件的 `XHolder{ X() view.X }`，以及生成的实体总接口 `<E>Entity`，后者嵌入实体挂载的所有组件的 Holder；
- handler 目前仍通过 `IXEntity.XComp()` 拿具体类型。

## 3. controllers 的目标形态

```
game/controllers/
  controller.go   公共部分（生成器写入，一次）
  bag.go          bag 的全部 Handle* 方法和 bag 自己的依赖取用
  chat.go         由现在的 chat_history / chat_push / chat_send / local 合成
  alliance.go
  bag_test.go     测试同样每个模块一个文件
```

- **`controller.go`** 包含：
  - `Controller{registry, nestClient}`、`FromRegistry`、`NestClient()`、`Registry()`；
  - 推送辅助，取代现在 chat、servertime、rules、team、enter 各抄一份的 `playerTCPCapability` / `pushTransport` / `toPush` / `PushNow`；
  - 按模块名懒建状态的辅助函数。
- **模块自己的依赖**：
  - 无状态的依赖（比如 mail 客户端）由模块文件里的私有方法每次从 Registry 取；
  - 有状态的依赖（比如 chat 的在线名单、推送面）用公共的懒建辅助函数，在一个 Controller 上只建一次。
- **文件命名规则**：
  - 模块放在 controllers 下的文件只能是 `<模块>.go` 和 `<模块>_test.go`，安装器检查这条；
  - 包级名字带模块前缀，避免重名。现在只有 scene、terrain 的 `refusal` 会冲突，推送辅助那几个会收进公共文件。
- **协议生成器**：
  - 缺少的 `Handle*` 存根追加到 `<分组>.go` 末尾，文件不存在就新建。只追加，不改已有内容；
  - 分发绑定只构造一次 `controllers.FromRegistry`；
  - "每个分组都要有 Controller"的检查改成全包只检查一次；
  - `roost add endpoint` 用同一套规则。
- **合并后的规模**：alliance 约 600 行，team、enter、chat 各约 300 行，其余不超过 260 行。
- **代价**：所有模块编译进同一个包，一个模块编译失败会连带整个包；模块之间不再有包级隔离。`game/handler` 现在就是这样，可以接受。

## 4. 组件与 view 的目标形态

```go
// view/component_bag.go（模块带来，手写）
type Bag interface { Add(...) error; ... }            // 组件对外的方法
type BagHolder interface {
    Bag() Bag                                         // 访问器（生成器实现）
    BagState() *db.BagState                           // 宿主方法：组件要求实体提供
}

// view/entity_player.go（生成）
type PlayerEntity interface {
    entity.IThreadSafeEntity
    BagHolder
    ...
}

// view/<x>_entities_gen.go（生成）：每个 Holder 一个窄接口，给 Nest handler 当参数类型
type BagEntity interface { entity.IThreadSafeEntity; BagHolder }
```

**组件包（`game/components/<x>`）**
- 放 `//roost:component type=N` 和 `CompTypeX` 常量。组件类型号属于组件本身，实体只是使用它。
- `init()` 里注册工厂。
- 实体结构体的标签写成 `comp:"bag.CompTypeBag"`，不再需要 `XComponent` 类型别名。
- 一个组件包挂在多个实体上时，只用一个类型号。activity 现在挂在 player、world、alliance 三种实体上，用了 3 个类型号，改成 1 个。

**handler**
- 参数类型改成 view 的窄接口，从 `target.ProfileComp().Rename()` 改成 `target.Profile().Rename()`。
- handler 用到、但 view 接口里还没有的方法，补进 view 接口。
- 生成的 `XComp()` 具体类型 getter 只留给生成的接线内部使用。

**宿主方法的实现**

- **纯 DAO 交接，由生成器实现**：view 方法返回实体 DAO 里的某个字段时，生成器在 `<实体>_gen_wire.go` 里直接写出实现。先按名字匹配，名字对不上再按类型找唯一的字段；有歧义就报错，要求手写。
  - 覆盖的方法：`BagState`、`WalletState`、`WalletLedger`、`EnterState`、`AttributeBase/Gear/Final`、各实体的 `ActivityBook`。
- **其余手写**：在 `entities/<实体>/<模块>.go` 里实现，每个模块一个小文件。

  | 类别 | 现有例子 |
  |---|---|
  | 每种实体自己的值 | `ActivityScope()` 返回 "player" / "world" / "alliance"，`ActivityCreatedAt()`，Scene 的 `Capacity()`、`MemberLoaded()` 等读实体私有字段的方法 |
  | 转给别的组件 | `MapBasic()`、`MoveComponent()` 转给 actor；`AttributeLevel()` 转给 profile |
  | 业务钩子 | `GrantFirstEnter`、`RunCreateHooks`、`CollectAttributeGear`、`WalletSpent` |
  | 可选的拦截 | naming 的 `CheckProfileRenameRequest`。现在 handler 里用 `target.(interface{...})` 在运行时判断，改成 view 里定义的可选接口 |

  不能合成每种实体一个文件，是因为模块安装器按整个文件安装，多个模块不能往同一个文件里写。

  预计手写文件剩约 9 个，其余约 30 个 `*_component.go` 都会删掉。
- **缺实现会在编译时报出来**：实体漏实现宿主方法时，会在"实体实现总接口"的编译期断言处报错，不用等到运行时。

## 5. 生成器与其他改动

| 位置 | 改动 |
|---|---|
| `codegen/internal/protocol` | 存根追加到 `<分组>.go`；只构造一次 Controller；去掉每个分组的 controller.go |
| `codegen/internal/entity`（`view.go` 等） | 生成每个 Holder 的窄接口；生成 DAO 交接方法；解析 `comp:"<包>.CompTypeX"` 标签（`deriveAccessors` 现在对带包名的标签另有规则，要改）；生成编译期断言 |
| `codegen/internal/nest` | 确认参数类型改成 view 的接口后仍能找到目标实体，见第 6 节 |
| `codegen/internal/roost` | 安装器的 controllers 文件规则；ID 分配改成按组件分配，不再按实体挂载分配；`roost add component` 的模板（组件定义在实体包里的写法仍保留）；doctor 中 `game/entities/*/*_component.go` 的检查规则 |
| 模块模板 | 约 40 个模块：controllers 合文件；删除或改写 entities 下的组件文件；view 补上宿主方法；handler 改参数类型 |
| 跟着改 | game-demo 中由模块和生成器带来的部分；genre 夹具和 compose.md 的目录约定；`codegen/docs/TAGS`；framework 的 view 说明；CHANGELOG 和迁移步骤 |

## 6. 实施前先验证的点

1. **Nest 生成器怎么找目标实体**：handler 参数从 `player.IProfileEntity` 改成 `view.ProfileEntity` 后，生成器还能不能找到目标实体？组件挂在多个实体上时（activity），要求显式写 `target=`，还是报错？
2. **组件类型号只按组件分配**：确认框架不要求类型号在不同实体之间唯一，类型号也不进存档或同步数据。线上未部署，换号本身不需要兼容。
3. **单个包的测试**：各模块的测试辅助函数合进同一个包后会不会重名。需要的话统一加模块前缀。

## 7. 实施顺序

1. 先用 bag、profile、activity 三个模块走通整条路，覆盖三种情况：纯 DAO 交接、一般组件、多实体共用一个组件：
   - 协议生成器的追加规则；
   - view 窄接口和 DAO 交接方法的生成；
   - 组件类型号挪进组件包。

   新生成工程装这三个模块，要能编译、通过测试、`generate --check` 无差异。
2. 再批量改其余模块。每改一批，跑对应模块的门禁。
3. 最后改 game-demo、genre 夹具和文档，跑一次全量：codegen 全量、全部模块门禁、genre 脚本、demo 重新生成。

## 8. 工作量

约 2～3 天的 agent 工作量。

- 生成器（protocol、entity、nest、installer）约 1 天；
- 模块模板约 1 天；
- demo、夹具、文档和全量约半天。
