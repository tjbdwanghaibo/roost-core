#!/usr/bin/env python3
"""精简正式工程的业务接入：两份活动表、一个开发用进程驱动，生产模块不增加入口。"""
import pathlib
import sys

repo, project = map(pathlib.Path, sys.argv[1:3])
mutant = sys.argv[3] if len(sys.argv)>3 else ""
module = "example.com/activity-accept"
out=project/"e2e"
out.mkdir()
src=repo/"scripts/testdata/alliance-e2e/harness_test.go.txt"
(out/"harness_test.go").write_text(src.read_text().replace("example.com/planet",module))
src=repo/"scripts/testdata/operations-real/runtime_test.go.txt"
(out/"runtime_test.go").write_text(src.read_text().replace("OPERATIONS_MODULE",module))
# 复用既有 TCP 客户端，仅保留通用握手/收包/请求相关辅助；精简配方没有 demo SceneReady 或 AddItem。
text=(repo/"scripts/testdata/alliance-e2e/game_test.go.txt").read_text()
text=text[text.index("// player 是"):text.index("// addGold")]
text=text.replace('Channel: "demo", OpenID: openID, Credential: "demo:" + openID','Channel: "dev", OpenID: openID, Credential: "dev-recipe-local"')
text=text.replace('&pb.EnterGameRequest{ClientVersion: "e2e"}', '&pb.EnterGameRequest{}').replace('enter.Code != 0 || enter.PlayerID != playerID','enter.Code != 0')
begin=text.index('\tready := call(')
end=text.index('\treturn p',begin)
text=text[:begin]+text[end:]
(out/"player_test.go").write_text('''package e2e
import (
 "context"
 "fmt"
 "net"
 "sync"
 "testing"
 "time"
 "github.com/tjbdwanghaibo/roost-core/client/wire"
 svcaccount "github.com/tjbdwanghaibo/roost-core/service/account"
 "ACTIVITY_MODULE/protocol/msgid"
 "ACTIVITY_MODULE/protocol/pb"
)
'''.replace("ACTIVITY_MODULE",module)+text)
for src in (repo/"scripts/testdata/activity-real").glob("*_test.go.txt"):
    (out/src.name.removesuffix(".txt")).write_text(src.read_text().replace("ACTIVITY_MODULE",module))
(project/"cmd/activityaccept").mkdir(parents=True)
(project/"cmd/activityaccept/main.go").write_text((repo/"scripts/testdata/activity-real/main.go.txt").read_text().replace("ACTIVITY_MODULE",module))
# 配方明确要求：正式 rehome 安装后移除配方原来的占位在途拒绝，否则会抢先拒绝恢复续行。
(project/"internal/access/player/tcp/recipe_entering.go").unlink()
(project/"configs/table/activity.csv").write_text("id,version,scope,trigger,event,start_milli,delay_millis,warmup_millis,open_millis,repeat,period,max_cycles,until_milli,catchup,logic,disabled\n"
    "1,1,player,event,operations,0,0,0,60000,none,0,1,0,queue,operations-pending,false\n"
    "2,1,world,event,operations,0,0,0,60000,none,0,1,0,queue,operations-pending,false\n"
    "3,1,alliance,event,operations,0,0,0,60000,none,0,1,0,queue,operations-pending,false\n")
(project/"configs/table/activity_scope.csv").write_text("id,bootstrap_sid,created_at\n77,4100,1700000000000\n")
# 两个 World 进程必须各持自己的 WAL，父目录保持框架按 SID 隔离的默认行为。
if mutant=="world-driver":
    path=project/"internal/rpc/activity_world/mod.go"
    text=path.read_text()
    anchor="views, err := m.service.Pulse(ctx, id, \"\", 0)"
    assert text.count(anchor)==1,"world driver mutation drift"
    path.write_text(text.replace(anchor,"if true { return nil } // MUTANT: drop the recovery scan\n\t\t"+anchor))
