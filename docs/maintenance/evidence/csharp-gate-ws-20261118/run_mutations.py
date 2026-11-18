#!/usr/bin/env python3
"""C# 经 WebSocket 连玩家接入的变异（在仓库根目录运行）：每次改一处、构建、只跑覆盖它的用例，记下结果，再从备份恢复并逐字节核对。
   python3 docs/maintenance/evidence/csharp-gate-ws-20261118/run_mutations.py > mutations.txt
"""
import os
import pathlib
import subprocess

ROOT = pathlib.Path.cwd()
DLL = ROOT / "client/dotnet/Roost.Client.Tests/bin/Debug/net10.0/Roost.Client.Tests.dll"
ENV = dict(os.environ, GOWORK="off", DOTNET_CLI_TELEMETRY_OPTOUT="1", DOTNET_NOLOGO="1", ROOST_CLIENT_TEST_DLL=str(DLL))
LOCAL = ["dotnet", str(DLL), "client/spec/packets.json"]
GO_WS = ["go", "test", "-count=1", "-run", "^TestTheCSharpClientWorksOverWebSocket$", "./infra/network/gateway"]
NODE = ["node", "client/dotnet/unity-webgl-smoke/check-jslib-frames.mjs"]

MUTATIONS = [
    ("M1 一帧拆成两条消息写（包头一条、载荷一条）", "client/dotnet/Roost.Client/TcpSession.cs",
     "await transport.SendAsync(data, budget.Token).ConfigureAwait(context);",
     "await transport.SendAsync(data.AsSpan(0, 16).ToArray(), budget.Token).ConfigureAwait(context); await transport.SendAsync(data.AsSpan(16).ToArray(), budget.Token).ConfigureAwait(context);",
     LOCAL),
    ("M2 PumpDriven 下读循环的等待用回线程池（ConfigureAwait(false)）", "client/dotnet/Roost.Client/TcpSession.cs",
     "Packet packet = await PacketCodec.ReadAsync(transport, maxPayload, context).ConfigureAwait(context);",
     "Packet packet = await PacketCodec.ReadAsync(transport, maxPayload, false).ConfigureAwait(false);",
     GO_WS),
    ("M3 PumpDriven 下等待（心跳间隔、重连退避）仍用 Task.Delay", "client/dotnet/Roost.Client/PumpScheduler.cs",
     "if (!PumpDriven) return Task.Delay(delay, cancellation);",
     "if (PumpDriven || !PumpDriven) return Task.Delay(delay, cancellation);",
     GO_WS),
    ("M4 GameClient.Pump 不触发计时（不调 Connection.Pump）", "client/dotnet/Roost.Client/GameClient.cs",
     "                Connection.Pump();\n",
     "",
     GO_WS),
    ("M5 PumpDriven 下超时仍用 CancelAfter 的计时器", "client/dotnet/Roost.Client/PumpScheduler.cs",
     "if (!PumpDriven) { source.CancelAfter(delay); return null; }",
     "if (PumpDriven || !PumpDriven) { source.CancelAfter(delay); return null; }",
     GO_WS),
    ("M6 浏览器桥接 C# 一侧拼帧时丢掉剩余字节（一次取满中转缓冲、只交 count 个）", "client/dotnet/unity-webgl-smoke/Assets/GateSmoke/BrowserGateTransport.cs",
     "int got = js.Receive(id, scratch, Math.Min(count, scratch.Length));",
     "int got = Math.Min(count, js.Receive(id, scratch, scratch.Length));",
     LOCAL),
    ("M7 jslib 取了一部分就丢掉这条消息的剩余字节", "client/dotnet/unity-webgl-smoke/Assets/Plugins/WebGL/RoostRelayWebSocket.jslib",
     "      if (entry.head === chunk.length) {",
     "      if (true) {",
     NODE),
]


def run(command):
    result = subprocess.run(command, cwd=ROOT, env=ENV, capture_output=True, text=True, timeout=600)
    return result.returncode, result.stdout + result.stderr


def build():
    return run(["dotnet", "build", "client/dotnet/Roost.Client.Tests", "-nologo", "-v", "q"])


def interesting(output):
    keep = [line.strip() for line in output.splitlines()
            if any(k in line for k in ("FAIL", "Exception", "Check failed", "violations", "check-jslib-frames", "timed out"))]
    return "\n".join(line[:600] for line in keep[:6]) or output[-600:]


for name, path, old, new, command in MUTATIONS:
    file = ROOT / path
    original = file.read_bytes()
    text = original.decode()
    assert text.count(old) == 1, (name, old)
    try:
        file.write_text(text.replace(old, new))
        code, out = build()
        if code != 0:
            print(f"## {name}\n构建失败（不算变异结果）：\n{out[-1500:]}\n")
            continue
        code, out = run(command)
        print(f"## {name}\n命令：{' '.join(command)}\n退出码：{code}（{'红' if code != 0 else '没红'}）\n{interesting(out)}\n")
    finally:
        file.write_bytes(original)
        assert file.read_bytes() == original
print("## 恢复")
code, out = build()
print("构建", "成功" if code == 0 else "失败:\n" + out[-800:])
