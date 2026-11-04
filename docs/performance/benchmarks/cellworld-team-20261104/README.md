# cellworld v2.5：跨 Base 的队伍面板（2026-11-04）

无缝大世界原型第 4 项 = A 的修前 / 修后输出，说明见 [FEATURE §13](../../../maintenance/FEATURE-2026-11-03-seamless-world.md#13-v25跨-base-的队伍面板第-4-项--a2026-11-04)。同机 macOS（Apple M5），`GOWORK=off`。

| 文件 | 内容 |
| --- | --- |
| `mirror-hold-red-before.txt` | `policy.MirrorItem.Hold` 字段已加、`Expire` 还按 TTL 时 `TestMirrorHoldOverridesTTLPerItem` 的输出 |
| `unit-red-before.txt` | cellworld 队伍面板用例在只有类型、线上消息与 API 骨架时的输出（全部红） |
| `unit-red-stage2.txt` | 快照、保留时间、组关系视野做了，通知与重读还没做时：通知、重读、重启后重读各自的红 |
| `demo-unit-red-before.txt` | 生成 game-demo 的单测：Team 成员表方法不做事、Player 只记队伍 ID、队伍面板字段没映射、`scene.partition.team` 不校验时的输出 |
| `e2e-first-run-fenced.txt` | 第一轮端到端：建队后没有切 Remote 共享模式，p6 在另一个 Base 上接受被 fence 拒绝 |
| `e2e-red-before.txt` | 修前端到端：组队协议、Team、共享模式都在，服务里不配 `GroupView`（队伍面板关闭）——只有 `team_panel` 红（p1 看不到 p6）。同一轮 `base_restart` 也红，是那一步原有的断言按字面 1200 比较、walkTo 越过 40 的误报，见 FEATURE §13.4 |
| `e2e-cross-base-write-conflict.txt` | 全部实现后第三轮：队伍面板断言都通过，最后 p1（sid 1000）在 p6（sid 1004）写过 Team 之后读同一支队伍得到 `state version conflict`（FEATURE §13.6；已由 main `e12e3dc8` 修复） |
| `e2e-green.txt`、`e2e-green-evidence.json` | 合入 main 前的端到端输出：13 步通过，`team_roster_cross_base` 标为已知失败（EXPECTED-FAIL） |
| `e2e-after-remote-fix.txt`、`e2e-after-remote-fix-evidence.json` | 分支 merge main（含 Remote 修复 `e12e3dc8`）、去掉 `-expect-fail` 之后的端到端：14 步全部通过，`team_roster_cross_base` ok（FEATURE §13.6） |
