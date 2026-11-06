#!/usr/bin/env python3
"""Run the native client against fragmented loopback RS packets and Go Sync goldens."""
import json
import pathlib
import socket
import struct
import subprocess
import sys
import tempfile
import threading
import time

binary = sys.argv[1]
repo = pathlib.Path(__file__).resolve().parents[3]
goldens = {x['name']: x for x in json.loads((repo / 'client/spec/packets.json').read_text())}
golden = goldens['sync']
with tempfile.NamedTemporaryFile() as sample:
    sample.write(bytes.fromhex(golden['hex'])[16:]); sample.flush()
    subprocess.run([binary, 'sync', sample.name], check=True, timeout=10)
auth = goldens['auth_generation']
subprocess.run([binary, 'auth', auth['hex'][32:], auth['generation']], check=True, timeout=10)

def auth_payload(body):
    """拆开鉴权首包：信封（0x00 'G' 8 字节大端代号 票据）返回 (票据, 代号)，否则 (原文, 0)。"""
    if len(body) > 10 and body[:2] == b'\x00G':
        return body[10:], struct.unpack('!Q', body[2:10])[0]
    return body, 0

errors = []
heartbeats = []
routes = set()
listener = socket.socket()
listener.bind(('127.0.0.1', 0)); listener.listen()

def exact(conn, count):
    data = b''
    while len(data) < count:
        part = conn.recv(count - len(data))
        if not part: raise EOFError()
        data += part
    return data

def send(conn, mid, seq, payload=b'', flags=0):
    data = struct.pack('!2sBBIII', b'RS', 2, flags, mid, seq, len(payload)) + payload
    # Deliberately split both header and body to exercise TCP framing.
    for start in range(0, len(data), 3): conn.sendall(data[start:start+3])

def serve():
    try:
        with listener.accept()[0] as conn:
            conn.settimeout(5)
            authenticated = False
            while True:
                magic,version,flags,mid,seq,size=struct.unpack('!2sBBIII',exact(conn,16))
                assert magic==b'RS' and version==2 and flags in (0,4) and size<1024
                body=exact(conn,size)
                if mid: routes.add(mid)
                if mid==0:
                    if authenticated and body == b'RCN1':
                        stamp = time.time_ns()
                        send(conn,0,seq,b'RCN1'+struct.pack('!qq',stamp,stamp))
                        continue
                    if not authenticated:
                        ticket, generation = auth_payload(body)
                        assert ticket==b'ticket' and generation>=started_ns, (body, started_ns); authenticated=True
                    else: heartbeats.append(seq)
                    send(conn,0,seq)
                    if seq==1: send(conn,9,0,b'\x08\x03',1)
                elif mid==42:
                    assert body==b'\x08\x01'; send(conn,43,seq,b'\x08\x02')
                elif mid==46: send(conn,10,0,b'',1)
                elif mid==50001: assert flags==4 and body==bytes.fromhex('c8010301')
                elif mid!=44: raise AssertionError('unexpected route')
    # Explicit client disconnect may be FIN or RST while a heartbeat reply is
    # in flight. The subprocess must still exit successfully and every expected
    # route must have been observed; premature closure cannot pass the test.
    except (EOFError, ConnectionResetError, BrokenPipeError): pass
    except Exception as e: errors.append(e)

# 连接代号取自 UTC 纳秒时钟（max(上一个 + 1, 当前时钟)），不会早于客户端进程启动前的时刻。
started_ns=time.time_ns()
thread=threading.Thread(target=serve);thread.start()
try:
    subprocess.run([binary,str(listener.getsockname()[1])],check=True,timeout=10)
finally:
    listener.close();thread.join(timeout=6)
    if errors: print('server-side check failed:', errors, file=sys.stderr)
assert not thread.is_alive() and not errors, errors
assert heartbeats, 'heartbeat not exercised'

assert routes == {42, 44, 46, 50001}, routes

# 同一进程先后两次连接：每次鉴权首包都带新的连接代号，后一次更大（跨 Gate 时 Game 据此认出迟到的旧握手）。
generations = []
reconnect = socket.socket()
reconnect.bind(('127.0.0.1', 0)); reconnect.listen()
def serve_reconnect():
    try:
        for _ in range(2):
            with reconnect.accept()[0] as conn:
                conn.settimeout(5)
                magic,version,flags,mid,seq,size=struct.unpack('!2sBBIII',exact(conn,16))
                ticket, generation = auth_payload(exact(conn,size))
                assert mid==0 and seq==1 and ticket==b'ticket', (mid, seq, ticket)
                generations.append(generation)
                send(conn,0,seq)
                try: exact(conn,16)
                except (EOFError, ConnectionResetError): pass
    except Exception as e: errors.append(e)
thread=threading.Thread(target=serve_reconnect);thread.start()
try:
    subprocess.run([binary,'reconnect',str(reconnect.getsockname()[1])],check=True,timeout=10)
finally:
    reconnect.close();thread.join(timeout=6)
    if errors: print('server-side check failed:', errors, file=sys.stderr)
assert not thread.is_alive() and not errors, errors
assert len(generations)==2 and generations[0]!=0 and generations[1]>generations[0], generations
