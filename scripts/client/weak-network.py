#!/usr/bin/env python3
"""Exercise bounded client failures and delayed/fragmented TCP on loopback."""
import argparse
import json
import os
import pathlib
import platform
import subprocess
import sys
import time

parser = argparse.ArgumentParser()
parser.add_argument("--native-build", required=True)
parser.add_argument("--unity-player", required=True)
parser.add_argument("--output", required=True)
args = parser.parse_args()
root = pathlib.Path(__file__).resolve().parents[2]
output = pathlib.Path(args.output).resolve()
if output.exists():
    parser.error("output exists; retain previous evidence")
output.mkdir(parents=True)
build = pathlib.Path(args.native_build).resolve()
tests = root / "client/cpp/tests"
report = {
    "kind": "TCP loopback application faults, not WAN packet-loss emulation",
    "platform": platform.platform(),
    "commit": subprocess.check_output(
        ["git", "-C", str(root), "rev-parse", "HEAD"], text=True
    ).strip(),
    "rows": [],
}


def run(name, command, settings):
    environment = os.environ.copy()
    environment.update(settings)
    started = time.monotonic()
    with (output / (name + ".log")).open("w") as log:
        try:
            process = subprocess.run(
                command,
                env=environment,
                stdout=log,
                stderr=subprocess.STDOUT,
                timeout=40,
            )
            status = process.returncode
        except subprocess.TimeoutExpired:
            status = -1
    report["rows"].append(
        {
            "name": name,
            "exit_code": status,
            "seconds": time.monotonic() - started,
            "settings": settings,
        }
    )
    # Failure evidence survives an interrupted suite.
    (output / "summary.json").write_text(json.dumps(report, indent=2) + "\n")
    if status:
        raise SystemExit(f"{name} failed; see {output}")


run(
    "native-faults",
    [sys.executable, str(tests / "weak.py"), str(build / "roost_native_weak")],
    {},
)
for name, delay, fragment in (("normal", "0", "0"), ("delayed-fragments", "60", "7")):
    settings = {"ROOST_TEST_DELAY_MS": delay, "ROOST_TEST_FRAGMENT_BYTES": fragment}
    run(
        "native-" + name,
        [sys.executable, str(tests / "game.py"), str(build / "roost_native_game")],
        settings,
    )
    run(
        "unity-" + name,
        [
            sys.executable,
            str(tests / "game.py"),
            args.unity_player,
            "-batchmode",
            "-nographics",
            "-logFile",
            str(output / ("player-" + name + ".log")),
            "-roostPort",
        ],
        {**settings, "ROOST_TEST_SYNC": "1"},
    )
print(output / "summary.json")
