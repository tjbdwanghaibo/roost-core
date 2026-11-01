import json
import pathlib
import subprocess
import sys
import tempfile
root = pathlib.Path(__file__).resolve().parents[3]
rows = json.loads((root / 'client/spec/dao-sync.json').read_text())
with tempfile.TemporaryDirectory() as directory:
    files=[]
    for row in rows:
        path=pathlib.Path(directory)/row['name']
        path.write_bytes(bytes.fromhex(row['hex']));files.append(str(path))
    subprocess.run([sys.argv[1],*files],check=True,timeout=10)

    collections = json.loads((root / 'client/spec/dao-collections.json').read_text())
    for row in collections:
        paths = []
        for key in ('full', 'patch', 'want'):
            path = pathlib.Path(directory) / key
            path.write_bytes(bytes.fromhex(row[key]))
            paths.append(str(path))
        subprocess.run([sys.argv[1], *paths], check=True, timeout=10)
