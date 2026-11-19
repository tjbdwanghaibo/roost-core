"""Print the game-demo database handles and the names queried by the drill snapshot."""
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

source, cli, reader = (Path(arg).resolve() for arg in sys.argv[1:])
drill = (source / "scripts/test-demo-rehome.sh").read_text()
snapshot = re.search(r"(?ms)^snapshot\(\) \{\n.*?^\}", drill).group(0)
for name in ["rhd123456", "local", "a" * 40 + "east"]:
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary)
        subprocess.run([cli, "project", "new", name, "-module", "example.com/" + name,
                        "-out", root / "app", "-template", "game-demo", "--skip-deps"],
                       check=True, stdout=subprocess.DEVNULL)
        config = root / "app/configs/service/config.game.yaml"
        expected = subprocess.check_output([reader, config], text=True).strip().split()
        # Capture only the --eval argument. No Mongo or account operation is performed.
        shell = snapshot + """
mongosh() { printf '%s\\n' "${@: -1}"; }
drill() { true; }
snapshot after
"""
        subprocess.run(["bash", "-eu", "-c", shell], check=True, env={
            **os.environ, "case_name": ".", "out": str(root), "project": name,
            "entity_id": "7", "ROOST_DATAENGINE_IT_MONGO_URI": "unused"})
        query = (root / "after-documents.json").read_text()
        queried = re.findall(r"getSiblingDB\('([^']+)'\)", query)
        assert len(queried) == len(expected) == 2
        print(f"project={name} handles={expected} snapshot={queried} match={expected == queried}")
