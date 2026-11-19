# PhysX 物理房间 + LevelComponent 联合玩法：实施证据（2026-11-19 起）

方案：[FEATURE-2026-11-19-physx-level-actions](../../FEATURE-2026-11-19-physx-level-actions.md)（D1～D11 全部按推荐 A）。在分支 `feat/physx-level-actions` 上实施，分支基线 main `2d15a1e5`，每批一个提交。本文按批记录：做了什么、怎么验的、哪些没有验到。

环境（各批相同，除非另写）：Apple M5（10 核）、macOS 26.5.2（25F84）、Go 1.27.3、Apple clang 21.0.0。原生库用本机已编好的 PhysX 4.1.2 静态库（`/Users/whb/roost/.physics-deps/build`，源码修订 `a2c0428a`）与分支里的 `bridge.cpp` 以 clang++ 链出 `infra/physics/physx/lib/libroost_physx.dylib`（gitignore，不入库）。**只在 macOS ARM64 上跑过；Linux 没有跑。**

变异验证的做法：绿之后逐条撤掉或改掉一处关键代码，跑对应用例，要求变红并记下失败文本，然后原样恢复。每批的原始记录在 `mutations-batchN.txt`。

## 批 1：native `rp_set_pose`、ABI 3、`World.SetPose`

### 改动

| 文件 | 内容 |
| --- | --- |
| `infra/physics/physx/native/bridge.h`、`bridge.cpp` | 新增 `rp_set_pose`：只接受动态刚体，写入位姿、线速度、角速度，清力与力矩的两组累加器，最后 `wakeUp()`；静态、运动学、未知 id 返回 0。`rp_abi_version()` 返回 3。结构体布局未变 |
| `infra/physics/physx/native.go` | `World.SetPose(id, position, rotation, velocity, angularVelocity)`：与 `Step` 同一把锁；校验有限值与单位四元数；目标不是动态刚体返回 `ErrPoseTarget`；`New` 只认 ABI 3 |
| `infra/physics/physx/types.go` | `ErrPoseTarget`、常量 `nativeABI = 3` |
| `infra/physics/physx/disabled.go` | 不带 `physx` 标签时的同名桩，返回 `ErrUnavailable` |
| `infra/physics/physx/native_test.go` | `TestSetPoseMovesADynamicBodyAndClearsItsMotion` |

与方案文字的两处出入：

- 方案写“`clearForce` / `clearTorque`（`eFORCE` 与 `eACCELERATION` 两种都清）”。PhysX 里这两种模式共用同一组累加器，另一组是冲量 / 速度变化；实现清的是这两组（`eFORCE` 与 `eVELOCITY_CHANGE`），覆盖全部四种模式。
- 各 `set…` 调用都不带自动唤醒，统一由最后的 `wakeUp()` 唤醒，这样“唤醒”只有一处、能被用例单独证明。

### 验证

```
GOWORK=off CGO_ENABLED=1 go test -count=3 -race -tags physx ./infra/physics/physx    # ok
GOWORK=off go build ./...                                                           # 不带标签，通过
GOWORK=off CGO_ENABLED=1 go vet -tags physx ./infra/physics/physx                    # 通过
```

用例断言的内容：

- 带着水平速度和滚动角速度的球被传送后，下一步的位置等于目标位姿加一步重力位移，水平速度与角速度为零，旋转等于给定的四元数。
- 传送时给定的线速度与角速度在下一步生效。
- 两个休眠的球，把其中一个沿地面传送到与另一个重叠、速度为零：50 步后两球被推开（球心间距不小于 0.9，两半径之和是 1）。
- 静态、运动学、未知 ID 返回 `ErrPoseTarget`，运动学平台留在原处。
- 零四元数、非单位四元数、NaN、越界速度被拒绝；业务 worker 上调用返回 `fctx.ErrBlockingInBusinessWorker`；关闭后返回 `ErrClosed`。

新 API 在旧代码上写不出能编译的红，所以批 1 的红全部是变异红。另有一条真实的修前现象：新的 Go 绑定配旧原生库（ABI 2，没有 `rp_set_pose`）在链接期就失败：

```
Undefined symbols for architecture arm64:
  "_rp_set_pose", referenced from:
      __cgo_cfcf21c5b342_Cfunc_rp_set_pose in 000001.o
ld: symbol(s) not found for architecture arm64
FAIL	github.com/tjbdwanghaibo/roost-core/infra/physics/physx [build failed]
```

### 变异（[mutations-batch1.txt](mutations-batch1.txt)）

8 条，6 条变红。

| 变异 | 结果 | 失败文本（节选） |
| --- | --- | --- |
| native 不写线速度 | 红 | `next step does not start from the new pose`（x 漂到 5.227，速度 11.37） |
| native 不写角速度 | 红 | `old angular velocity survived the teleport on axis 2` |
| native 不唤醒 | 红 | `sleeping body was not woken by the teleport: still overlapping, gap 0.200` |
| Go 绑定不查运动类型 | 红 | `body 100: want ErrPoseTarget, got physx: set pose failed`、`body 50: …` |
| Go 绑定与 native 都不查运动学 | 红 | `body 50: want ErrPoseTarget, got <nil>`、`rejected teleport moved the kinematic platform: [0 9 0]` |
| 原生库仍报 ABI 2 | 红 | `physx: native ABI mismatch; rebuild native library` |
| 只有 native 不查运动学（Go 绑定仍查） | **没有变红** | Go 绑定的检查在前，请求到不了 native；native 这一处检查由上面“两边都不查”那条证明有效 |
| native 不清力与力矩累加器 | **没有变红** | 见下 |

**没有被用例覆盖的一处**：`rp_set_pose` 里的四行 `clearForce` / `clearTorque`。现在力只在 `rp_step_control` 里累积、并在同一次 `simulate` 里消耗，两次调用之间累加器本来就是空的，所以这四行在现有调用次序下不改变任何结果，Go 侧也写不出能区分的用例。保留它们是因为方案的接口约定写了“清掉累积的力和力矩”；它们是否正确只靠读 PhysX 文档判断，没有运行证据。

唤醒用例的一点说明：最初写的是“把休眠体放到空中，看它会不会下落”，以及“原地给一个初速度”，两种写法在去掉 `wakeUp()` 后都仍然通过——刚体脱离支撑或被赋予非零速度时 PhysX 自己会唤醒它。改成“沿地面传送到另一个休眠体身上、速度为零”后变异才变红，这也正是方案 §8 里“传送到有人的出生点”的情形。

“旧 ABI 被拒”没有做成进程内用例（一个进程里装不了两份原生库），由上表“原生库仍报 ABI 2”这条变异和上面的链接失败记录代替。
