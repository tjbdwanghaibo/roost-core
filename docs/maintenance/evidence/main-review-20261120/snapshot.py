"""Compare the drill's two effective query targets to Mongo handles for each generated service config."""
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import tempfile

repo, cli, drill, reader = [Path(arg).resolve() for arg in sys.argv[1:]]
script = (repo / "scripts/test-demo-rehome.sh").read_text()
lib = repo / "scripts/testdata/demo-drill/lib.sh"
variables = "\n".join(line for line in script.splitlines() if line.startswith(("source_db=", "target_db=")))
snapshot = re.search(r"(?ms)^snapshot\(\) \{\n.*?^\}", script).group()
# Observe the original mongosh expression without a service. Return the database
# actually selected only if the expression reads the expected player's collection.
javascript = """
const Long = String, print = console.log, EJSON = JSON;
const db = {getSiblingDB(name) {return {player: {findOne(query) {
    if (query._id !== '7') throw Error('unexpected player');
    return name;
}}}}};
"""
for name in ["rhd123456", "local", "a" * 40 + "east"]:
    with tempfile.TemporaryDirectory() as temp:
        project = Path(temp) / "app"
        subprocess.run([cli, "project", "new", name, "-module", "example.com/" + name,
                        "-out", project, "-template", "game-demo", "--skip-deps"],
                       stdout=subprocess.DEVNULL, check=True)
        (project / "bin").mkdir(exist_ok=True)
        (project / "bin/rehomedrill").symlink_to(drill)
        source = project / "configs/service/config.game.yaml"
        target = project / subprocess.check_output(
            ["bash", "-eu", "-c", 'source "$1"; port_shift=29000; drill_second_game_config 1001', "_", lib],
            cwd=project, text=True).strip()
        for altered in ([False, True] if name == "local" else [False]):
            if altered:
                config, count = re.subn(r"(?m)^(  database_prefix:).*$", r"\1 target_zone", target.read_text())
                assert count == 1
                target.write_text(config)
            expected = [subprocess.check_output([reader, path], text=True).split()[i]
                        for i, path in enumerate((source, target))]
            shell = variables + "\n" + snapshot + """
mongosh() { node -e "$js ${*: -1}"; }
drill() { true; }
snapshot after
"""
            subprocess.run(["bash", "-eu", "-c", shell], check=True, cwd=project, env={
                **os.environ, "case_name": ".", "out": temp, "entity_id": "7", "js": javascript,
                "game_source": str(source), "game_target": str(target), "ROOST_DATAENGINE_IT_MONGO_URI": "unused"})
            actual = json.loads((Path(temp) / "after-documents.json").read_text())
            print(f"{name} custom_target={altered} handles={expected} queried={actual}", flush=True)
            assert [actual["source"], actual["target"]] == expected
