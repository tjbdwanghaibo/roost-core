# 方案：实体事件（framework/entity 自带 Publish，监听由生成器登记）（2026-11-16，第二版）

**状态：方案，未实施。**

本方案和 [controllers 合包、组件接口收进 view](FEATURE-2026-11-16-controllers-view-layout.md) 属于同一轮整理，两者一起实施。

## 0. 维护者定的要求（2026-11-16）

1. **事件定义集中放在 `game/event/`。**
2. **实体底层自带 `Publish`。** 生成器不再为每个事件生成发布方法。
3. **core 提供"事件名 + 回调方法"的登记方法。** 由生成代码调用它完成登记。
4. **用 `//roost:listen` 标记监听方法。** 实体的方法和组件的方法都可以标记。
5. **`//roost:listen` 可以带优先级。** 见 §2.3 和 §4.2。

和第一版相比：
- 事件不再放在 view，也不再由生成器在 view 里写 `Publish`；
- 去掉了独立的 `//roost:event` 标记和存根机制：方法标了 `//roost:listen` 就登记，没标就不登记，生成器据此判断"实现了没有"。

## 1. 要替换的东西

### 1.1 现有的钩子

- **规模**：19 个模块共有 33 个 `OnXxx` 登记点，其中 9 个模块各自手写了一份 `hooks.go`，合计约 630 行。
- **做法**：
  - 每份都是一张登记表加读写锁，在 init 时登记；
  - 执行时按名字排序，同步依次调用；
  - 任何一个返回错误，就交给 Nest 回滚整笔事务。
- **登记位置**：监听方的登记代码放在 `with/<对方模块>/` 下，只有两个模块都装了才会生成。

### 1.2 旧的 framework/event

旧包已在 10-13 的 fa3d2e4e 中删除，不恢复：

- 它让每个实例在运行时订阅；
- 订阅方没有返回值，既不能拦截，也不能让事务回滚；
- 跨实体时走一个语义未定义的异步分发。

本方案只借用"实体能发布事件"这个形态，实现重写。

### 1.3 33 个登记点的分类

| 类别 | 登记点 | 处理 |
|---|---|---|
| **通知**：事情已经发生，同一实体在同一笔事务里跟着改 | ProfileLevelUp、WalletSpent、ShopBought、StaminaSpent、CheckinClaimed、DungeonClaimed、EquipmentChanged、AttributeChanged、TeamChanged、RehomeArrived / Arrived、Submitted、Entered、FirstEnter、Create、Leaving、Region、SceneInteract | 改为实体事件 |
| **拦截**：事情发生之前询问，返回错误就拒绝 | Entering、Creating、Applying、RehomeApplying、RoomJoining、TeamJoining、MatchmakingStarting | 改为实体事件，返回错误即拒绝 |
| **扩展提供方**：登记工厂、字段表、配置修改器、取值函数 | Actor、ActorSyncFields、SceneFields、CompileEnvironment、AttributeGear、CreatePrepare、LocalChannel | 不是"发生了什么"，保留现有登记方式 |
| **连接层**：不在实体锁里 | SessionActive | 实体事件只能在实体被锁住时发布，所以保留现状 |

实施时逐个核对这张表。现在所有通知和拦截类钩子改的都是**发布事件的那个实体自己**（例如 Player 自己的任务、属性、活动），这正好落在实体事件的范围内。

## 2. 业务层写什么

### 2.1 事件定义：`game/event/`

`game/event` 成为 `game/` 下第七个固定层，和 components、controllers、entities、handler、managers、view 并列。每个模块放一个文件：

```go
// game/event/profile.go（profile 模块带来）
package event

// ProfileLevelUp 在 Player 升级成功后、同一笔事务内发布。
type ProfileLevelUp struct {
    From, To int32
}
```

- **事件名就是结构体名。** 同在一个包里，天然不会重名。
- **不需要标记。** `game/event` 下导出的结构体都算事件。
- **依赖要求：** `game/event` 处在最底层，只允许 import 标准库和 `db`，不能 import game 下的其他包。事件里只放值，不放实体引用或指针。

### 2.2 发布：实体自带 `Publish`

```go
// 组件里（宿主是 view 接口）
if err := host.Publish(event.ProfileLevelUp{From: from, To: to}); err != nil {
    return 0, err
}
```

`Publish(ev any) error` 加在 `entity.IThreadSafeEntity` 上，由 `EntityBase` 实现。view 里所有实体接口都嵌入了 `IThreadSafeEntity`，所以拿到宿主就能发布。

### 2.3 监听：在方法上标 `//roost:listen`

**组件级**：方法写在组件上，组件挂在哪些实体上，就在这些实体上生效。

```go
// game/components/activity/activity_profile.go（只在装了 profile 时生成，即现在的 with/profile）

//roost:listen
func (c *Component) OnProfileLevelUp(ev event.ProfileLevelUp) error {
    return c.Reconcile("profile.level_up", 0)
}
```

**实体级**：方法写在实体上，只对这一种实体生效。

```go
// game/entities/player/player_naming.go

//roost:listen
func (e *Player) OnProfileRenaming(ev event.ProfileRenaming) error {
    return apperrors.ErrNamingRequired
}
```

规则：

- **方法签名固定**：只有一个参数，类型是 `game/event` 里的某个结构体，按值传入；返回 `error`。监听哪个事件由参数类型决定。方法名不限，约定写成 `On<事件名>`。
- **数量限制**：同一个接收者对同一个事件只能有一个监听方法。
- **优先级**：可选，写成 `//roost:listen priority=<整数>`。
  - 缺省是 0，数字大的先执行，可以是负数。
  - 取值范围 -1000～1000，超出范围或不是整数，生成时报错。
  - 优先级相同时，按 §4.2 的固定规则排序。
  - 只在同一个事件的监听方法之间比较。
  - 例：attribute 要先刷新属性，task 再按刷新后的属性算进度。attribute 的监听方法写 `priority=100`，task 用缺省值。
- **拦截类事件**：监听方法返回错误，发布方就拒绝这次操作。

## 3. core：framework/entity

### 3.1 登记方法：事件名 + 回调

```go
// RegisterEventListener 把一个回调登记到某种实体的某个事件上，只由生成代码在 init 里调用。
// eventName 用于日志和错误信息，取 E 的类型名；listener 是 "<接收者>.<方法>"，例如 "activity.Component.OnProfileLevelUp"。
// 回调按登记先后执行。重复登记同一个 listener，或在 App 启动之后登记，直接 panic。
func RegisterEventListener[T IThreadSafeEntity, E any](kind EntityKind, eventName, listener string, fn func(T, E) error)
```

内部是一张 `kind → 事件类型 → []回调` 的表。只在 init 期间写入，启动之后只读，所以不加锁。

### 3.2 `EntityBase.Publish`

**问题**：`EntityBase` 按设计不持有外层实体的接口引用，但回调需要拿到具体的实体（比如 `*Player`）。

**做法**：和现有的 `SetHooks(onClear, onDestroy)` 一样，由生成的实体工厂注入一个函数：

```go
// 生成的工厂里
e.SetEventDispatch(func(ev any) error { return entity.DispatchEvent(KindPlayer, e, ev) })
```

`Publish(ev)` 按以下步骤执行：

1. **检查锁和事务**：两项都满足才往下走，否则返回 `ErrTransactionClosed`。
   - 当前 goroutine 的 Guard 持有这个实体。entity 包自己就能判断：`CurrentGuardScope()` 加上 Guard 的持锁检查。
   - 当前在 Nest 事务里。entity 包不能 import nest（nest 已经 import 了 entity），所以由 nest 在自己的 init 里注入：`entity.SetEventTxCheck(func() bool { return CurrentRollbackTx() != nil })`。
2. **压入事件栈，检查上限**：规则见 §3.3。
3. **查表、执行**：按 `(kind, ev 的类型)` 取出回调列表，按登记顺序依次执行。遇到第一个错误就停下，错误包装成 `"<事件链> listener <listener>: <err>"` 返回，由调用方交给 Nest 回滚。
4. **弹出事件栈**：用 defer 执行，出错或 panic 时也会弹出。
5. **没有监听的事件**：直接返回 nil，但仍然计入 §3.3 的次数。

### 3.3 事件中再发布事件，以及循环

**做法：立即执行，深度优先。** 监听方法里调用 `Publish`，会马上执行新事件的全部监听，执行完才回到外层，继续执行外层剩下的监听。

不用"先排队，等外层监听都执行完再处理"的原因有两个：

- **拦截类事件必须马上拿到结果。** `Publish` 的返回值就是"拒不拒绝"。如果排队，发布方拿到的永远是 nil。
- **现在的钩子就是立即执行的。** profile 在 `AddExp` 里直接调用 `runProfileLevelUp`，钩子里触发的下一层钩子也是当场执行。保持立即执行，迁移前后行为不变。

**外层后面的监听看到的状态**：它们看到的是内层整条链执行完之后的状态。这和直接调用函数的效果一样，执行顺序由 §4.2 决定，是确定的。

**错误必须一路往外传。** 内层出错时，错误由内层 `Publish` 返回给调它的监听方法，这个监听方法必须原样返回这个错误。

- 事务只能整笔回滚，没有部分回滚（保存点）。如果把内层错误吞掉，内层在出错前已经做的修改会留下来，状态就只改了一半。
- doctor 新增 `event:publish-error` 检查：`Publish(...)` 的返回值被丢弃，或者写成 `_ =`，就警告。

**拦截类事件的监听方法只做检查，不修改状态。** 原因是发布方收到拒绝后，可能把它当成业务结果回给客户端（返回了响应，而不是返回 error），这时事务不回滚，前面已执行的拦截监听如果改过状态，修改就留下了。这条写进 compose.md 和 TAGS。现在的 `-ing` 钩子都已经满足这一条，迁移时逐个核对。

**循环怎么挡住。** 运行时有三道检查，任何一道触发都报错，整笔事务回滚。错误信息里带上完整的事件链，例如 `ProfileLevelUp → TaskCompleted → ProfileLevelUp → …`。

| 检查 | 上限 | 挡住什么 |
|---|---|---|
| 嵌套深度 | 8 层 | A→B→A→B… 这种互相触发、停不下来的递归 |
| 同一事件在栈上同时出现的次数 | 3 次 | 自己触发自己的递归。比别的链更早报错，并且直接指出是哪个事件 |
| 一次最外层 `Publish` 内的发布总次数 | 256 次 | 没超过深度、但每层发布多次造成的指数级展开 |

- **为什么不是"同一事件一出现在栈上就报错"**：有合法的重入。例如升级奖励发了经验，经验又让玩家再升一级：`ProfileLevelUp → 发奖 → AddExp → ProfileLevelUp`。这条链由数据决定，一定会停，所以允许同一事件在栈上最多出现 3 次。
- **上限可以调**：三个上限都是 core 常量，可以经 `entity.SetEventLimits` 调整，只能在 init 里调用。正常业务不应该接近这些上限。
- **事件栈的存放**：事件栈是 `EntityBase` 上的一个小切片，同时用来生成错误信息，以后也可以用来打日志和指标。事件只在同一个实体上分发，而这个实体正被这一笔事务锁住，所以不需要额外加锁。
- **跨事务不会残留**：每次最外层 `Publish` 返回时，事件栈都已经清空，不会把状态带进下一笔事务。

**静态提示（生成器，只提示、不报错）**：生成器扫描每个监听方法的函数体，以及同一个接收者上被直接调用的方法，找出其中的 `Publish(event.X{...})`，画出"事件 → 监听 → 再发布的事件"的图。图里有环就打印提示，列出这条环。

- 这只是提前提醒。经过别的包、接口或间接调用发布的事件看不到，所以运行时的检查才是最终的保证。
- 合法的重入（比如上面的升级链）也会被列出来，开发者确认后可以不管。

**panic**：事件层不 recover。panic 沿着 Nest 现有的路径处理：`callTransactionHandler` 先回滚事务，再把 panic 继续抛出去。事件栈靠第 4 步的 defer 弹出。

**还需要核实的一点**：Nest 的 memory 快路径（`invokeMemoryHandler`）里有没有 `RollbackTx`。如果没有，§3.2 第 1 步会拒绝在 memory handler 里发布事件。实施时核实，再决定是允许，还是在文档里写明不支持。

**不做的事**：
- 不做异步；
- 不跨实体：要改别的实体，仍然走 Nest 发送；
- 不跨进程：跨进程用 `nest.Emit`，它有 outbox 或 best-effort 投递语义；
- 不支持运行时订阅或退订。

## 4. 生成器

在 entity 生成器（`codegen/internal/entity`）里新增事件登记，每个实体包生成一个 `<实体>_event_gen.go`：

```go
func init() {
    entity.RegisterEventListener(KindPlayer, "ProfileLevelUp", "player.Player.OnProfileLevelUp",
        func(e *Player, ev event.ProfileLevelUp) error { return e.OnProfileLevelUp(ev) })
    entity.RegisterEventListener(KindPlayer, "ProfileLevelUp", "activity.Component.OnProfileLevelUp",
        func(e *Player, ev event.ProfileLevelUp) error { return e.ActivityComp().OnProfileLevelUp(ev) })
    entity.RegisterEventListener(KindPlayer, "ProfileLevelUp", "attribute.Component.OnProfileLevelUp",
        func(e *Player, ev event.ProfileLevelUp) error { return e.AttributeComp().OnProfileLevelUp(ev) })
}
```

### 4.1 扫描范围

- 实体包本身：找实体类型上的方法，以及定义在实体包里的组件（`roost add component` 生成的那种）上的方法；
- 实体挂载的每个组件所在的包：从 `comp:"<包>.CompTypeX"` 标签解析出包路径。

这些地方带 `//roost:listen` 的方法都会被登记。activity 这种挂在多个实体上的组件，会在每个实体的生成文件里各登记一次。

### 4.2 执行顺序（生成时固定）

1. 先按 `priority` 从大到小排；
2. 优先级相同时，实体自己的监听方法排在前面；
3. 然后是各组件，按组件初始化顺序排列（已有的确定性拓扑序，见 CHANGELOG d610a1b6）；
4. 同一个接收者有多个监听方法时，按方法名排序。

排序全部在生成时完成：生成文件里的登记顺序就是执行顺序，core 只按登记先后执行，不知道优先级是多少。生成的每条登记旁边会写上注释 `// priority=N`，方便看懂为什么是这个顺序。

### 4.3 生成时报错的情况

- 标了 `//roost:listen` 但签名不对：参数不是唯一一个 `game/event` 结构体，或者返回值不是 `error`；
- 标记写在普通函数上，而不是方法上；
- 接收者既不是实体，也不是被某个实体挂载的组件；
- 同一个接收者对同一个事件有多个监听方法；
- `priority` 不是整数、超出 -1000～1000，或者写了别的未知选项。

### 4.4 只提示、不报错的情况

- 组件有监听方法，但当前工程里没有实体挂载这个组件；
- 事件在 `game/event` 里定义了，但没有任何监听方法（发布时什么也不做，属于正常情况）。

### 4.5 doctor

- `event:publish-error`：`Publish` 的返回值被丢弃时警告，见 §3.3。
- `event:cycle`：把 §3.3 的静态环提示也放进 doctor 的输出。

新增 `event:publish` 检查：扫描 `.Publish(` 的调用，发现参数不是 `game/event` 里的类型时给出警告。这是静态兜底，防止把普通结构体当事件发布，结果没人收到。

## 5. 安装器和目录规则

- `game/event/` 加入固定层：
  - 模块只能往里放 `game/event/<模块>.go`；
  - 安装器要检查这个文件只 import 标准库和 `db`。
- 监听方法照旧通过 `with/<对方模块>/` 文件带进来，安装条件不变：对方模块没装，事件类型就不存在，监听文件也不会生成。
- 本方案和 controllers/view 方案里的组件类型号调整（`comp:"<包>.CompTypeX"`）一起实施，扫描组件包时依赖这个新标签。

## 6. 迁移

1. 把 §1.3 中"通知"和"拦截"两类钩子改成 `game/event` 里的结构体。原来调 `runXxx` 的地方改成 `host.Publish(...)`。删掉 9 份 `hooks.go` 和其他模块里的同类登记表。
2. 把 `with/<模块>/` 下 init 里的登记，改成组件上带 `//roost:listen` 的方法。
   - 原来写在功能包里的登记（例如 `game/modules/activity` 里的那个），挪成对应组件的方法。
   - 少数不属于任何组件的，写成实体上的方法。
3. naming 现在在 profile 的改名 handler 里做类型断言拦截。改成：profile 发布一个拦截事件 `ProfileRenaming`，naming 在 Player 上监听它。
4. 文档要改：
   - `codegen/docs/TAGS` 新增 `//roost:listen`；
   - framework 文档新增"实体事件"一节，写清和 `nest.Emit`、Nest 发送的分工；
   - compose.md：新玩法要联动时，先找现成的事件；
   - 目录约定加上 `game/event`；
   - CHANGELOG 和迁移步骤。
5. 线上还没部署，不做兼容。

## 7. 验证

- **core 单测**：
  - 事务外发布被拒绝；
  - 多个回调按登记顺序执行，遇到第一个错误就停下，错误信息带上事件名和监听方；
  - 用真实 Nest 事务验证：监听出错时整笔回滚，包括发布方自己的改动；
  - 内层出错时错误一路传到最外层，并且整笔回滚；外层后面的监听能看到内层链执行完之后的状态；
  - 三道上限各有触发用例，错误信息里带完整事件链；同一事件合法重入两次可以通过，第三次报错；
  - 监听方法 panic 后，事件栈被清空，下一笔事务不受影响；
  - 不在事务里，或当前 Guard 没持有这个实体时，发布被拒绝；
  - 启动后再登记会 panic；
  - 没有监听的事件返回 nil。
- **生成器单测**：
  - §4.3 的每种报错情况各一个用例；
  - 组件挂在多个实体上时，每个实体都登记了；
  - 按优先级排序：大的先执行、负数排在缺省值之后；优先级相同时退回固定规则；跨实体方法和组件方法也按优先级比较；
  - 多次生成，顺序和内容不变。
- **先失败后通过**：把 `DispatchEvent` 改成"出错也继续执行"，profile 的"升级联动出错则整笔回滚"场景必须失败。
- **模块门禁**：现有联动场景（task 进度、attribute 刷新、activity 资格、mailbox 发奖、各类拦截）数量不变，全部通过。

## 8. 工作量

| 项 | 估计 |
|---|---|
| core：登记表、Publish、nest 注入事务检查、单测 | 约半天 |
| 生成器：扫描实体和组件方法、生成登记代码、报错规则、doctor、安装器规则、单测 | 约 1 天 |
| 迁移 26 个登记点，删除 hooks.go，改 naming 拦截 | 约 1 天 |

合计约 2.5 天。和 controllers/view 方案一起做，最后共用一次全量测试。
