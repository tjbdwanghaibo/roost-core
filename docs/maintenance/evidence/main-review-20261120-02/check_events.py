"""Run the tested shell script's exact Python event classifier against generated logs."""
import pathlib
import re
import subprocess
import sys

script, events = map(pathlib.Path, sys.argv[1:])
classifier = re.search(r"(?ms)^python3 - [^\n]+ <<'PY'\n(.*?)^PY$", script.read_text()).group(1)
subject = (events / "entity-id").read_text()
for early, expected in [("false", 0), ("true", 1)]:
    result = subprocess.run(
        [sys.executable, "-", str(events / (early + ".jsonl")), subject, str(events / (early + ".json"))],
        input=classifier, capture_output=True, text=True)
    print(f"early={early} rc={result.returncode} {result.stderr.strip()}")
    assert result.returncode == expected
    if expected:
        assert "inconclusive" in result.stderr
