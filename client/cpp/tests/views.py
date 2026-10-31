#!/usr/bin/env python3
"""Independent CRP/RSSS wrapper over Go generated DAO protobuf goldens."""
import json
import pathlib
import struct
import subprocess
import sys
import tempfile
repo=pathlib.Path(__file__).resolve().parents[3]
rows=json.loads((repo/'client/spec/dao-sync.json').read_text())
def packet(tick,payload=None):
    header=struct.pack('!4sHBBQIIIHH',b'CRP1',1,1 if tick==1 else 2,0,1,1,tick,0 if tick==1 else tick-1,1,1)
    if tick==3: return header+struct.pack('!BHHHH',3,1,1,0,0)
    body=struct.pack('!4sHHQ I QQQ I HBB I HHI',b'RSSU',2,1 if tick==1 else 0,9007199254740993,1,tick,0 if tick==1 else tick-1,0,0,2,0,0,1,4,7,len(payload))+b'herodefault'+payload
    return header+struct.pack('!BHHHHBHHI',1 if tick==1 else 2,1,1,1 if tick==1 else 0,1,1,1,1,len(body))+body
with tempfile.TemporaryDirectory() as directory:
    files=[]
    for tick in range(1,4):
        path=pathlib.Path(directory)/str(tick)
        path.write_bytes(packet(tick,bytes.fromhex(rows[tick-1]['hex']) if tick<3 else None));files.append(str(path))
    subprocess.run([sys.argv[1],*files],check=True,timeout=10)
