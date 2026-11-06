"""Diagnostic only: change the test relay receive buffer using Go overlay."""
import json
import pathlib
import subprocess
import sys
import tempfile
root = pathlib.Path(sys.argv[1]).resolve()
source = root / 'framework/sync/lockstep/tcp_test.go'
old = 'const deepLinkReadBuffer = 16 << 10'
text = source.read_text()
if text.count(old) != 1:
    raise SystemExit('Expected v1.40.1 fixture not found; inspect the new baseline first.')
with tempfile.TemporaryDirectory(prefix='lockstep-buffer-control-') as directory:
    work = pathlib.Path(directory)
    replacement = work / 'tcp_test.go'
    replacement.write_text(text.replace(old, 'const deepLinkReadBuffer = 256 << 10'))
    overlay = work / 'overlay.json'
    overlay.write_text(json.dumps({'Replace': {str(source): str(replacement)}}))
    subprocess.run(['go', 'test', '-overlay=' + str(overlay), '-race', './framework/sync/lockstep', '-run', '^TestASnapshotOverEmbeddedTCPFollowsTheLinkAndKeepsOtherSessionsLive$', '-count=5', '-v'], cwd=root, check=True)
