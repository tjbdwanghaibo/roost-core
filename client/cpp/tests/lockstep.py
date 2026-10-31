#!/usr/bin/env python3
"""Core Go protocol goldens plus an independent SHA-256 oracle."""
import hashlib
import json
import pathlib
import subprocess
import sys
import tempfile
repo = pathlib.Path(__file__).resolve().parents[3]
with tempfile.NamedTemporaryFile(mode='w') as fixture:
    for packet in json.loads((repo / 'client/spec/packets.json').read_text()):
        fixture.write(packet['name'] + ' ' + bytes.fromhex(packet['hex'])[16:].hex() + '\n')
    for size in (0, 1, 55, 56, 63, 64, 65, 1024, 65537):
        digest = hashlib.sha256(bytes((i*31) % 256 for i in range(size))).hexdigest()
        fixture.write('sha_' + str(size) + ' ' + digest + '\n')
    fixture.flush()
    subprocess.run([sys.argv[1], fixture.name], check=True, timeout=30)
