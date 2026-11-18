#!/usr/bin/env python3
"""TCP application-layer faults; no claim of WAN packet-loss emulation."""
import socket, struct, subprocess, sys, threading, time


def exact(c, n):
    b = b""
    while len(b) < n:
        x = c.recv(n - len(b))
        if not x:
            raise EOFError()
        b += x
    return b


def packet(mid, seq, body=b"", flags=0):
    return struct.pack("!2sBBIII", b"RS", 2, flags, mid, seq, len(body)) + body


for mode in ["heartbeat", "timeout", "truncated", "overflow", "send_stall"]:
    listener = socket.socket()
    listener.bind(("127.0.0.1", 0))
    listener.listen()
    listener.settimeout(6)
    errors = []
    client_done = threading.Event()

    def serve():
        try:
            with listener.accept()[0] as c:
                c.settimeout(6)
                if mode == "send_stall":
                    c.setsockopt(socket.SOL_SOCKET, socket.SO_RCVBUF, 4096)
                head = exact(c, 16)
                exact(c, struct.unpack("!I", head[12:])[0])
                c.sendall(packet(0, 1))
                if mode == "send_stall":
                    # 停读并持有连接直到客户端进程结束，让客户端只可能撞上自己的发送期限。
                    # 原来固定睡 0.8s 后关闭：客户端从就绪到发出 16 MiB 请求若被调度拖过约
                    # 0.5s，关闭就先于 200ms 发送期限到达，报 "send failed"（EPIPE）而不是
                    # 发送超时——那是夹具计时，不是客户端分类错误（KNOWN-LIMITS E-2）。
                    client_done.wait(6)
                    return
                if mode == "truncated":
                    c.sendall(packet(9, 0, b"abcdef", 1)[:-3])
                    return
                if mode == "overflow":
                    time.sleep(0.1)
                    c.sendall(b"".join(packet(9, 0, b"x", 1) for _ in range(30)))
                while c.recv(4096):
                    pass
        except (EOFError, ConnectionResetError, BrokenPipeError):
            pass
        except Exception as e:
            errors.append(repr(e))

    thread = threading.Thread(target=serve)
    thread.start()
    try:
        subprocess.run(
            [sys.argv[1], mode, str(listener.getsockname()[1])], check=True, timeout=8
        )
    finally:
        client_done.set()
        listener.close()
        thread.join(7)
    assert not errors and not thread.is_alive(), errors
