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
                    time.sleep(0.8)
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
        listener.close()
        thread.join(7)
    assert not errors and not thread.is_alive(), errors
