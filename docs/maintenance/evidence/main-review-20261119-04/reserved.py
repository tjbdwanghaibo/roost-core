"""Usage: python reserved.py CLI NAME_READER (both built from the tested revision)."""
from pathlib import Path
import subprocess
import sys
import tempfile

cli, reader = (Path(arg).resolve() for arg in sys.argv[1:])
for name in ["admin", "admin_data", "local", "local_data", "config", "config_data", "atlas_a", "atlas_b"]:
    with tempfile.TemporaryDirectory() as temporary:
        root = Path(temporary) / name
        subprocess.run([cli, "project", "new", name, "-module", "example.com/" + name,
                        "-mods", "mongo,dataengine", "-skip-deps", "-out", root],
                       check=True, stdout=subprocess.DEVNULL)
        config = root / "configs/service/config.game.yaml"
        names = subprocess.check_output([reader, config], text=True).strip()
        print(f"{name}: {names}")
