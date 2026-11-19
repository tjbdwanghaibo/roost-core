"""Usage: python compare_names.py BASELINE_CLI MAIN_CLI MAIN_NAME_READER"""
from pathlib import Path
import subprocess
import sys
import tempfile

baseline, main, reader = (Path(arg).resolve() for arg in sys.argv[1:])
common = "a" * 40
projects = [common + "east", common + "west", "atlas_a", "atlas_b"]
for label, cli in [("baseline", baseline), ("main", main)]:
    for project in projects:
        with tempfile.TemporaryDirectory() as tmp:
            target = Path(tmp) / project
            subprocess.run(
                [cli, "project", "new", project, "-module", "example.com/" + project,
                 "-mods", "mongo,dataengine", "-skip-deps", "-out", target],
                check=True, stdout=subprocess.DEVNULL,
            )
            config = target / "configs/service/config.game.yaml"
            print(label, project)
            for line in config.read_text().splitlines():
                if line.startswith(("  database_prefix:", "  database:", "  databases:")):
                    print("  " + line.strip())
            if label == "main":
                names = subprocess.check_output([reader, config], text=True).strip()
                print("  database and sid 1000:", names)
