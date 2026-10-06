# revleft 证据（N11～N13 留项与小防护，2026-10-06）

基线 `dc877c04`，分支 `revleft`。[本轮记录](../../../review/REVIEW-2026-10-06-revleft.md)。macOS arm64，go1.27.0，`GOWORK=off`。

| 文件 | 内容 |
| --- | --- |
| [nc260-red.txt](nc260-red.txt) | NC-260 修前：`go test -count=1 -run TestMongoModStop ./kit/mongo/` |
| [nc261-262-red.txt](nc261-262-red.txt) | NC-261 / 262 修前：`./robot/session/` 两条用例 |
| [nc263-red.txt](nc263-red.txt) | NC-263 修前：`./log/` 两条用例（临时目录路径换成 `<tmp>`） |
| [nc264-red.txt](nc264-red.txt) | NC-264 修前：`./metrics/` |
| [nc265-red.txt](nc265-red.txt) | NC-265 修前：`./robot/` |
| [nc266-red.txt](nc266-red.txt) | NC-266 修前：`./robot/loadtest/` |
| [nc267-269-red.txt](nc267-269-red.txt) | NC-267～269 修前：`./container/`、`./goroutine/` |
| [nc270-red.txt](nc270-red.txt) | NC-270 修前：全新生成 game-demo（`roost project new revleftgd -template game-demo`，replace 到本分支）`go test -race -count=1 -run TestPathFindingWhileTheSceneStopsIsSafe ./game/scene/runtime/`，连续 3 次均红 |
| [gate-red.txt](gate-red.txt) | 小防护 A：临时冲突的红、对 `0aa2e1b9` 的扫描 |
| [green.txt](green.txt) | 修后全部新用例（race×3）、门禁、生成工程 |

复跑：在本分支 `git stash push -- <实现文件>` 后跑上表命令得到红；恢复后得到绿。生成工程命令见 nc270 一行。
