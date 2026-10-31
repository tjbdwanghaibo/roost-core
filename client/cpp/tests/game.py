#!/usr/bin/env python3
"""Two real TCP generations; no request replay; native and managed consumers."""
import os
import json
import pathlib
import socket
import struct
import subprocess
import sys
import threading
import time
repo = pathlib.Path(__file__).resolve().parents[3]
golden = {x['name']: bytes.fromhex(x['hex'])[16:] for x in json.loads((repo/'client/spec/packets.json').read_text())}
errors = []
listener = socket.socket()
listener.bind(('127.0.0.1',0)); listener.listen(); listener.settimeout(10)
def exact(c,n):
    out=b''
    while len(out)<n:
        part=c.recv(n-len(out))
        if not part: raise EOFError()
        out+=part
    return out
def send(c,mid,seq,body=b'',flags=0):
    c.sendall(struct.pack('!2sBBIII',b'RS',2,flags,mid,seq,len(body))+body)
def serve():
    try:
        for generation in range(2):
            with listener.accept()[0] as c:
                c.settimeout(5); seen=set()
                while True:
                    magic,version,flags,mid,seq,n=struct.unpack('!2sBBIII',exact(c,16)); body=exact(c,n)
                    assert magic==b'RS' and version==2 and n<1024
                    if mid==0:
                        assert flags==0
                        if seq==1: assert body==b'ticket'
                        send(c,0,seq)
                        if seq==1: send(c,9,0,b'\x08\x03',1)
                    elif mid==42:
                        assert mid not in seen and body==b'\x08\x01'; seen.add(mid)
                        send(c,43,seq,b'\x08\x02')
                    elif mid==50001:
                        assert mid not in seen and flags==4 and body==golden['lockstep_input']; seen.add(mid)
                        send(c,50002,0,golden['lockstep'],5)
                        send(c,50002,0,golden['snapshot_first_chunk'],5)
                        send(c,50002,0,golden['snapshot_next_chunk'],5)
                    elif mid==99:
                        assert seen=={42,50001} and generation==0
                        break
                    else: raise AssertionError('unexpected route '+str(mid))
                    if seen=={42,50001} and generation==0:
                        # Wait for a test-only consumer notification confirming both callbacks.
                        continue
    except EOFError: pass
    except Exception as e: errors.append(e)
t=threading.Thread(target=serve);t.start()
env = os.environ.copy()
if env.get('ROOST_NATIVE_LIBRARY_DIR'):
    env['DYLD_LIBRARY_PATH'] = env['ROOST_NATIVE_LIBRARY_DIR']
    env['LD_LIBRARY_PATH'] = env['ROOST_NATIVE_LIBRARY_DIR']
try: subprocess.run(sys.argv[1:]+[str(listener.getsockname()[1])],check=True,timeout=15,env=env)
finally: listener.close();t.join(timeout=6)
assert not t.is_alive() and not errors,errors
