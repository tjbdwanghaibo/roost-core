# 物理房间基础能力补验（2026-10-25）

基线为上一阶段物理/Chunk 提交，当前 macOS ARM64 + PhysX 4.1.2。维护者要求 Linux 实跑暂缓，后续封装独立房间 service。本轮只验证已有底层能力及修复确认缺陷，不实现生产服务接口。

## 已确认问题及修复

并发房间创建/销毁之后，后续 `physx.New` 可能持续返回 `physx: create failed`。首次四包 race 三轮检查复现；单独非 race 十轮未复现，见 [before.txt](before.txt)，不能据此排除竞争。恢复旧原生实现后，`go test -race -tags physx -count=30 ./infra/physics/physx` 再次复现，见 [before-race.txt](before-race.txt)。这些是功能断言失败，不是 Go race 检测到 C++ 数据竞争；Go race 不覆盖未插桩的 PhysX。

根因：钉住的 SDK `physx/source/physx/src/NpPhysics.cpp` 中 `createInstance` / `releaseInstance` 使用普通静态 `mRefCount`，`PxCreatePhysics` 返回进程单例。桥接原先用 foundationMutex 保护创建，却在锁外调用 physics->release，导致增减并发。修复将 release 与创建放在同一临界区，再结算 Foundation 引用。Scene/Actor/dispatcher 仍属于各世界，Step 不持进程级创建锁，没有把各房间模拟串行化。

这次问题说明此前“Foundation 有引用计数即足够”的实现前提不完整：共享资源包含 PxPhysics。后续 service 应明确进程级 SDK 所有权与房间级 Scene 所有权，不继续以每世界一个独立 SDK 的假设扩展。

## 验证覆盖

- 8 个并发世界，每个反复创建/推进/销毁 20 次；每世界使用相同 body ID=1、不同初态及加速度。按独立轨迹核验结果，关闭后 Step 返回 ErrClosed，重复 Close 无副作用。
- 一个 Run 提交失败立即退出，另一个 Run 继续至至少 10 tick，再通过 context 取消退出；关闭世界在 Run 返回之后进行。
- Chunk 包同时有删除和跨块更新，逐一截断每个字节位置；所有截断都拒绝且原副本不变，完整包才提交变化。
- 原生包与负对照相同命令 `-race -count=30` 通过，见 [after-race.txt](after-race.txt)，共验证 4800 次房间生命周期。
- 四包 `go test -race -tags physx -count=10` 通过，见 [after.txt](after.txt)；带 tag 的四包 vet 通过。
- 真实 20 TCP 客户端、71 动态体、20Hz 同步运行 20 秒：8000 输入全接收、0 拒绝、0 失败客户端，完整世界 998 tick。见 [host.log](tcp/host.log)、[result.json](tcp/result.json)。此轮与回归部分并行，资源数据仅保留，不作为新的性能基准。

## 仍需后续实现与验证

多世界测试不等同于房间 service 的创建/加入/退出/销毁 RPC 验收。RoomID/代际路由、生产 Gate 身份认证、房间级预算与排空、断线策略、动态刚体增删、Raycast 和碰撞事件尚未实现；先完成服务设计再验收这些能力。原有 Unity 200 帧验证仍是前一阶段证据，本轮没有重新运行 Unity；解码代码未修改。没有原生内存泄漏检测或多房间容量上限结论。

复跑需要先重建 native 动态库，随后执行：

```sh
GOWORK=off go test -race -tags physx -count=30 ./infra/physics/physx
GOWORK=off go test -race -tags physx -count=10 ./infra/physics/physx ./gameplay/physics ./framework/sync/chunksync ./cmd/physics-viewer
GOWORK=off go build -tags physx -o artifacts/physics-viewer ./cmd/physics-viewer
python3 scripts/physics/measure-crowd.py --seconds 20 --hz 20 --port 19092 --output artifacts/physics-room-smoke
```

CBM 本 worktree 索引仍因 worker exit=1 不可用，根因已通过桥接和钉住的 SDK 源码核对；未重启共享索引服务。
