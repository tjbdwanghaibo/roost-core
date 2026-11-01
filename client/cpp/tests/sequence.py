"""Native application admission vs heartbeats; enforce Gate's increasing sequence rule."""
import socket
import struct
import subprocess
import sys
import threading

errors = []
seen = []
listener = socket.socket()
listener.bind(("127.0.0.1", 0))
listener.listen()
listener.settimeout(12)

def exact(conn, count):
    data = b""
    while len(data) < count:
        part = conn.recv(count - len(data))
        if not part:
            raise EOFError()
        data += part
    return data

def serve():
    try:
        with listener.accept()[0] as conn:
            conn.settimeout(12)
            previous = 0
            while True:
                magic, version, flags, mid, seq, size = struct.unpack("!2sBBIII", exact(conn, 16))
                assert magic == b"RS" and version == 2 and flags == 0 and size <= 1
                body = exact(conn, size)
                assert seq > previous, (previous, mid, seq)
                previous = seq
                seen.append(mid)
                assert (seq == 1 and mid == 0 and body == b"t") or (seq > 1 and mid in (0, 42) and not body)
                conn.sendall(struct.pack("!2sBBIII", b"RS", 2, 0, 43 if mid else 0, seq, 0))
    except (EOFError, ConnectionResetError, BrokenPipeError):
        pass
    except Exception as error:
        errors.append(error)

thread = threading.Thread(target=serve)
thread.start()
try:
    subprocess.run([sys.argv[1], str(listener.getsockname()[1])], check=True, timeout=13)
finally:
    listener.close()
    thread.join(timeout=13)
assert not thread.is_alive() and not errors, errors
assert seen.count(42) == 500 and seen.count(0) > 1, (seen.count(42), seen.count(0))
print("PASS strictly increasing wire sequence; heartbeats:", seen.count(0) - 1)
