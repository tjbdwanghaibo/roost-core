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

binary = sys.argv[1]
repo = pathlib.Path(__file__).resolve().parents[3]
golden = next(x for x in json.loads((repo / 'client/spec/packets.json').read_text()) if x['name'] == 'sync')
with tempfile.NamedTemporaryFile() as sample:
    sample.write(bytes.fromhex(golden['hex'])[16:]); sample.flush()
    subprocess.run([binary, 'sync', sample.name], check=True, timeout=10)

errors = []
heartbeats = []
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
                assert magic==b'RS' and version==2 and flags==0 and size<1024
                body=exact(conn,size)
                if mid==0:
                    if not authenticated: assert body==b'ticket'; authenticated=True
                    else: heartbeats.append(seq)
                    send(conn,0,seq)
                    if seq==1: send(conn,9,0,b'\x08\x03',1)
                elif mid==42:
                    assert body==b'\x08\x01'; send(conn,43,seq,b'\x08\x02')
                elif mid!=44: raise AssertionError('unexpected route')
    except EOFError: pass
    except Exception as e: errors.append(e)

thread=threading.Thread(target=serve);thread.start()
try:
    subprocess.run([binary,str(listener.getsockname()[1])],check=True,timeout=10)
finally:
    listener.close();thread.join(timeout=6)
assert not thread.is_alive() and not errors, errors
assert heartbeats, 'heartbeat not exercised'
