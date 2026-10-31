#!/usr/bin/env python3
"""Generate every genre independently, then verify a custom module with this checkout."""
import argparse
import datetime
import hashlib
import json
import os
from pathlib import Path
import re
import subprocess
import sys
import time

GENRES = "slg moba mmorpg card-rpg tower-defense survival-extraction merge-casual idle-sim auto-chess party-room turn-pvp coop-roguelike social-sim".split()
ROOT = Path(__file__).resolve().parent.parent


def command(args, cwd, env, log):
    log.write(("$ " + " ".join(map(str, args)) + "\n").encode())
    log.flush()
    subprocess.run(args, cwd=cwd, env=env, stdout=log, stderr=subprocess.STDOUT, check=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--out", type=Path, required=True, help="new directory for retained projects and evidence")
    parser.add_argument("--genres", nargs="+", choices=GENRES, default=GENRES)
    args = parser.parse_args()
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=False)
    report = {"started_utc": datetime.datetime.now(datetime.timezone.utc).isoformat(),
              "git": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=ROOT, text=True).strip(),
              "dirty": bool(subprocess.check_output(["git", "status", "--porcelain"], cwd=ROOT)),
              "go": subprocess.check_output(["go", "version"], text=True).strip(),
              "scope": "generated gameplay fixtures; persistence substitutes unless separately recorded; not a capacity certification",
              "genres": []}
    for genre in args.genres:
        work = out / genre
        work.mkdir()
        env = os.environ.copy()
        for name in GENRES:
            prefix = "ROOST_" + name.upper().replace("-", "_")
            env[prefix + "_KEEP"] = "1"
            env[prefix + "_REAL"] = "0"
        env.update(ROOST_GENRE_KEEP="1", ROOST_GENRE_OUTPUT=str(work), GOWORK="off", ROOST_MOBA_ROUTING="0")
        env.pop("ROOST_GENRE_MODULE", None)
        result = {"genre": genre, "status": "failed", "module": "example.com/acceptance/" + genre}
        start = time.monotonic()
        print(f"Running {genre}: {work}", flush=True)
        try:
            with (work / "matrix.log").open("wb") as log:
                command(["bash", str(ROOT / "scripts" / f"test-{genre}-generated.sh")], ROOT, env, log)
                project = work / "project"
                old = "example.com/" + genre + "-practice"
                for path in project.rglob("*"):
                    if path.is_file() and (path.suffix in {".go", ".yaml", ".json", ".proto"} or path.name == "go.mod"):
                        content = path.read_text()
                        if old in content:
                            path.write_text(content.replace(old, result["module"]))
                command([str(work / "roost"), "generate"], project, env, log)
                command(["go", "build", "-mod=mod", "./..."], project, env, log)
                command(["go", "vet", "-mod=mod", "./..."], project, env, log)
                command([str(work / "roost"), "id", "check"], project, env, log)
                command([str(work / "roost"), "generate", "--check"], project, env, log)
                result["robot_pb_registry"] = (project / "loadtest/protocol/registry_gen.go").exists()
                result["genre_network_acceptance"] = "pending"
                names = []
                # Include supplementary offline fixtures (for example SLG alliance), not REAL suites.
                for evidence_path in work.glob("*.log"):
                    if evidence_path.name == "matrix.log":
                        continue
                    names.extend(re.findall(r"^--- PASS: (Test\w+)", evidence_path.read_text(), re.M))
                names = sorted(set(names))
                if not names:
                    raise RuntimeError("no executed gameplay tests found")
                expression = "^(" + "|".join(names) + ")$"
                with (work / "custom-module-tests.jsonl").open("wb") as custom_log:
                    command(["go", "test", "-mod=mod", "-race", "-count=1", "-timeout", "3m", "-json", "-run", expression, "./game/..."], project, env, custom_log)
                events = [json.loads(line) for line in (work / "custom-module-tests.jsonl").read_text().splitlines() if line.startswith("{")]
                passed = {event["Test"] for event in events if event.get("Action") == "pass" and event.get("Test") in names}
                skipped = [event["Test"] for event in events if event.get("Action") == "skip" and event.get("Test")]
                # go test emits package-level skip for packages with no test files; only test events indicate skipped cases.
                if passed != set(names) or skipped:
                    raise RuntimeError(f"custom module missing={sorted(set(names) - passed)}, skipped={skipped}")
                result["custom_module_passed_tests"] = len(passed)
                if "TestScenarioRobotCompleteCoopAndClientViews" in passed:
                    result["genre_network_acceptance"] = "tcp_pb_robot_complete_coop_rewards_client_views; substitute persistence"
                elif "TestScenarioRobotTCPRetryAndReconnect" in passed or any(name.endswith("RobotBusiness") for name in passed):
                    result["genre_network_acceptance"] = "embedded_tcp_pb_retry_reconnect; substitute persistence"
                elif "TestMOBAKCPRobotMatchSettlementAndClientResult" in passed:
                    result["genre_network_acceptance"] = "kcp_lockstep_robot_match_settlement_result; fixture auth"
            evidence = (work / "test.log").read_text()
            result["passed_tests"] = len(re.findall(r"^--- PASS:", evidence, re.M))
            result["skipped_tests"] = len(re.findall(r"^--- SKIP:", evidence, re.M))
            if result["passed_tests"] == 0 or result["skipped_tests"] != 0:
                raise RuntimeError("empty or skipped gameplay acceptance")
            result["status"] = "passed"
        except (subprocess.CalledProcessError, RuntimeError, OSError) as error:
            result["error"] = str(error)
        result["elapsed_seconds"] = round(time.monotonic() - start, 3)
        result["log_sha256"] = hashlib.sha256((work / "matrix.log").read_bytes()).hexdigest()
        report["genres"].append(result)
        (out / "report.json").write_text(json.dumps(report, indent=2) + "\n")
        print(f"{genre}: {result['status']}", flush=True)
    return int(any(item["status"] != "passed" for item in report["genres"]))


if __name__ == "__main__":
    sys.exit(main())
