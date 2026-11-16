# 方案：事务内同步事件（framework/event 重写，监听由生成器接线）（2026-11-16）

状态：**方案，未实施。** 维护者 2026-11-16 定下三条：

- 重写 `framework/event`，做成事务内同步事件；
- 业务层只做两件事：定义事件结构体、声明要监听的事件名；
- 监听方有没有实现，由代码生成判断。

本方案和 [controllers 合包、组件接口收进 view](FEATURE-2026-11-16-controllers-view-layout.md) 是同一轮整理，事件类型也放在 view。

## 1. 要替换的东西

### 1.1 现在的钩子

19 个模块共有 33 个 `OnXxx` 登记点。其中 9 个模块各写了一份 `hooks.go`，合计约 630 行。每份都是同一套做法：

- 用一张登记表加读写锁，在 `init` 里登记；
- 执行时按名字排序，依次调用；
- 任何一个返回错误，就把错误交还 Nest，整笔事务回滚。

监听方的登记代码放在 `with/<对方模块>/` 下，只有两个模块都装了才会生成。

### 1.2 旧的 `framework/event`（10-13 删除，fa3d2e4e）

不捡回来，原因有四点：

- 它是每个实体实例在运行时订阅、销毁时退订，靠 eventgen 生成 `InitSub`/`SyncHandleEvent` 里的 switch 分发；
- 事件编号是 `int32`，需要全局分配；
- 订阅方没有返回值，既不能拦截，也不能让事务回滚；
- 发给别的实体时走一个语义没有定义的 `AsyncDispatcher`，和 Nest 的事务模型冲突。

这次只沿用包名，从头重写。

### 1.3 33 个登记点的分类

实施时先按下表逐个确认：

| 类别 | 登记点 | 处理 |
|---|---|---|
| **通知**：事情已经发生，监听方在同一笔事务里跟着改 | ProfileLevelUp、WalletSpent、ShopBought、StaminaSpent、CheckinClaimed、DungeonClaimed、EquipmentChanged、AttributeChanged、TeamChanged、RehomeArrived / Arrived、Submitted、Entered、FirstEnter、Create、Leaving、Region、SceneInteract | 改成事件 |
| **拦截**：事情发生之前问一遍，返回错误就拒绝 | Entering、Creating、Applying、RehomeApplying、RoomJoining、TeamJoining、MatchmakingStarting | 改成事件。监听方返回错误就是拒绝 |
| **扩展提供方**：登记工厂、字段表、配置修改器或取值函数，不是"发生了什么" | Actor（钩子工厂）、ActorSyncFields、SceneFields、CompileEnvironment、AttributeGear、CreatePrepare、LocalChannel | 不改成事件，保留现在的登记方式 |
| **不在实体事务里** | SessionActive（连接层） | 用事件的 `tx=none` 选项（见 §2.1）；如果确认不需要，就保留现状 |

## 2. 业务层要写的东西

只有两样：

### 2.1 事件结构体（放在 view）

```go
// game/view/event_profile.go（profile 模块带来）

//roost:event host=Player
type ProfileLevelUp struct {
    From, To int32
}
```

- `host`：事件发生在哪种实体上，写实体名，对应 view 里生成的 `<实体>Entity`。监听方拿到的就是这个实体，并且已经被这笔事务锁住。
- `tx`：可选。缺省是 `required`，只能在 Nest 事务内发布；`none` 表示可以在事务外发布，这时监听方的错误交还给发布方，但没有回滚可言。
- 事件结构体只放值，不放指针或实体引用。这和现在钩子的参数一致。

### 2.2 监听声明（放在监听方自己的包里）

```go
// game/modules/activity/activity_profile.go（只在装了 profile 时生成，即现在的 with/profile）

//roost:listen ProfileLevelUp
func OnProfileLevelUp(host view.PlayerEntity, e view.ProfileLevelUp) error {
    return host.Activity().Reconcile("profile.level_up", 0)
}
```

- 监听名就是事件名。处理函数的名字固定为 `On<事件名>`，放在声明所在的包里。
- 参数里的 host 可以写实体总接口，也可以写更窄的 view 接口，只要实体总接口满足它。这一点在编译时就会检查，见 §3.2。
- 一个包可以监听多个事件，每个事件写一个 `//roost:listen`。

### 2.3 发布

```go
// game/components/profile/levelup.go
if err := (view.ProfileLevelUp{From: from, To: to}).Publish(host); err != nil {
    return 0, err
}
```

`Publish` 方法由生成器写在 view 里（见 §3.1），参数类型就是事件声明的 host 接口。传错实体类型会编译不过。

## 3. 生成器做的事

在 `roost generate` 里新增 event 一步，放在 entity（view 接口）之后、nest 之前。`generate --check` 也覆盖这一步。

### 3.1 view 里：`game/view/events_gen.go`

为每个 `//roost:event` 生成三样东西：

- 一个 `Publish` 方法：`func (e ProfileLevelUp) Publish(host PlayerEntity) error { return event.Publish(host, e) }`；
- 一个 `Listener` 类型：`type ProfileLevelUpListener func(PlayerEntity, ProfileLevelUp) error`；
- 一份事件清单，包括事件名、host 和 tx，供运行时核对。

### 3.2 接线：`game/handler/events_gen.go`

handler 包本来就 import 实体包、组件包和功能包，是 Nest 处理函数的登记层；事件监听也是事务内的处理函数，所以接线放在这里。

```go
func init() {
    event.Register("ProfileLevelUp", event.TxRequired, func(host view.PlayerEntity, e view.ProfileLevelUp) error {
        if err := activity.OnProfileLevelUp(host, e); err != nil { return fmt.Errorf("activity: %w", err) }
        if err := attribute.OnProfileLevelUp(host, e); err != nil { return fmt.Errorf("attribute: %w", err) }
        if err := mailbox.OnProfileLevelUp(host, e); err != nil { return fmt.Errorf("mailbox: %w", err) }
        return nil
    })
}
```

- **顺序**：按监听方包的 import 路径排序，生成时就固定下来。运行时没有登记表、没有锁，也不用排序。
- **没有监听方的事件**：也会登记，执行体为空。这样发布一个没声明过的类型时，运行时可以明确报错。
- **类型检查**：监听函数参数里的 host 是窄接口时，生成代码直接把 `view.PlayerEntity` 传进去，接口不满足就编译失败。

### 3.3 "实现不实现"的判断

生成器扫描每个 `//roost:listen <事件>`，按下表处理：

| 情况 | 处理 |
|---|---|
| 同包里有签名正确的 `On<事件>` | 接线 |
| 没有 `On<事件>` | 在声明所在文件末尾**追加存根**，只追加、不改已有内容，和协议存根同一个规则。存根返回 nil，第一行带存根标记；生成器打印"未实现"提示 |
| 有 `On<事件>` 但签名不对 | 生成失败，说明期望的签名 |
| 监听了不存在的事件，比如对方模块没装 | 生成失败 |
| 同一个包对同一个事件写了两次 `//roost:listen` | 生成失败 |
| 写了 `On<事件>` 但没有 `//roost:listen` | 不接线；生成器提示"有处理函数但没声明监听" |

**存根为什么返回 nil，而不是返回错误**：返回错误会让源操作整笔回滚，比如升级本身失败。所以存根什么都不做，改由检查来拦住：

- `roost project doctor` 新增 `event:stub`，列出还带存根标记的监听方；
- 仓库自带模块的门禁（`module verify`）发现存根就判失败。

## 4. core 运行时：`framework/event`

约一百行，只做这几件事：

```go
type TxMode int // TxRequired / TxNone

// Register 只由生成的 init 调用；同名重复登记，或 App 启动后登记，直接 panic。
func Register[H any, E any](name string, mode TxMode, dispatch func(H, E) error)

// Publish 由 view 生成的 <E>.Publish 调用。
func Publish[H any, E any](host H, e E) error
```

`Publish` 按以下顺序处理：

1. 用 E 的类型查登记。没登记过就报错。这张表只在 init 期间写，启动后只读，所以不加锁。
2. `TxRequired` 时检查 `nest.CurrentRollbackTx()`，不在事务内就返回 `ErrTransactionClosed`。判断方式和 `nest.Emit` 相同。
3. 嵌套深度加一。监听方可以再发布事件，嵌套超过 8 层就报错，事务随之回滚，用来挡住互相触发的死循环。深度记在当前事务上，需要 nest 的 `RollbackTx` 增加一个计数字段。
4. 调用生成的 dispatch，然后原样返回它的错误。

不做的事：

- 不做异步；
- 不跨实体：要改别的实体，仍然走 Nest 发送；
- 不跨进程：跨进程仍然用 `nest.Emit`，它有 outbox 或 best-effort 的投递语义；
- 不在运行时订阅或退订。

## 5. 迁移

1. 把 §1.3 的"通知"和"拦截"两类，改写成 view 里的事件结构体加 `Publish` 调用，删掉 9 份 `hooks.go` 和其他模块里的同类登记表。
2. 把 `with/<模块>/` 下的 `init()` 登记，改成 `//roost:listen` 加 `On<事件>`。安装条件不变，仍是两个模块都装了才生成。
3. "扩展提供方"一类不动。
4. 文档：
   - `codegen/docs/TAGS` 新增 `//roost:event`、`//roost:listen`；
   - framework 文档新增事件一节，写清和 `nest.Emit` 的分工；
   - 写进 compose.md 的复用规则：新玩法联动先找现成的事件；
   - CHANGELOG 和迁移步骤。
5. 线上未部署，不做兼容。

## 6. 验证

- **core 单测**：
  - 未登记的类型报错；
  - 事务外发布：`TxRequired` 被拒绝、`TxNone` 放行；
  - 监听出错时，整笔事务回滚，用真实的 Nest 事务测；
  - 嵌套超过上限报错；
  - 启动后登记会 panic。
- **生成器单测**：§3.3 表里的每种情况都有用例；存根追加后重跑，不再改动文件；生成结果排序确定。
- **先失败后通过**：去掉 dispatch 里"出错就返回"这一步，profile 的"升级钩子出错则整笔回滚"场景必须失败。
- **模块门禁**：现有联动场景（task 进度、attribute 刷新、activity 资格、mailbox 发奖、各拦截）的数量不变，并且全部通过。

## 7. 工作量

| 项 | 估计 |
|---|---|
| core `framework/event`、nest 深度计数、单测 | 约 2 小时 |
| 生成器（扫描、view 生成、handler 接线、存根、doctor）和单测 | 约 1 天 |
| 26 个登记点迁到事件，删掉 hooks.go | 约 1 天 |

合计约 2.5 天。和 controllers/view 那份方案一起做时，可以共用最后一次全量。
