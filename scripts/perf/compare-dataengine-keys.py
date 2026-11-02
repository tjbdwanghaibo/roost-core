#!/usr/bin/env python3
"""Build first, then compare generated Nest/WAL/Mongo consumers sequentially.

Use an isolated Mongo replica set via ROOST_DATAENGINE_IT_MONGO_URI.
The baseline directory must be a clean export of the desired release.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import subprocess
import time


def command(args, cwd, env, log):
    with log.open("a") as output:
        return subprocess.run(args, cwd=cwd, env=env, stdout=output,
                              stderr=subprocess.STDOUT, check=False).returncode


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--baseline", type=Path, required=True)
    parser.add_argument("--out", type=Path, required=True)
    parser.add_argument("--seconds", type=int, default=30)
    parser.add_argument("--rates", default="1000,3000,6000")
    parser.add_argument("--policies", default="async,strict,pipelined")
    parser.add_argument("--reverse-order", action="store_true", help="Reverse the first-level execution order")
    args = parser.parse_args()
    rates = [int(rate) for rate in args.rates.split(",")]
    policies = args.policies.split(",")
    if args.seconds < 1 or any(rate < 1 or rate * args.seconds > 100000 for rate in rates):
        parser.error("Each rate must be positive; each sample must have 1..100000 requests")
    if any(policy not in ("async", "strict", "pipelined") for policy in policies):
        parser.error("Unknown durability policy")
    repo = Path(__file__).resolve().parents[2]
    baseline = args.baseline.resolve()
    out = args.out.resolve()
    out.mkdir(parents=True, exist_ok=False)
    env = os.environ.copy()
    if not env.get("ROOST_DATAENGINE_IT_MONGO_URI"):
        parser.error("Set ROOST_DATAENGINE_IT_MONGO_URI to an isolated replica set")
    # Do not inherit unrelated pressure options or force runtime CPU limits.
    env = {key: value for key, value in env.items() if not key.startswith("ROOST_PERF_")}
    env.pop("GOMAXPROCS", None)
    env["GOWORK"] = "off"
    fixture = repo / "codegen/internal/entity/testdata/dataengine"
    variants = ("before", "after", "short")
    builds = {}
    metadata = {"started": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
                "go": subprocess.check_output(["go", "version"], text=True).strip(),
                "head": subprocess.check_output(["git", "rev-parse", "HEAD"], cwd=repo, text=True).strip(),
                "baseline": str(baseline), "seconds": args.seconds, "rates": rates,
                "policies": policies, "reverse_order": args.reverse_order, "GOMAXPROCS": "unset", "builds": {}}
    for variant in variants:
        source = baseline if variant == "before" else repo
        work = out / variant
        (work / "def").mkdir(parents=True)
        database = f"roost_generated_it_keys_{os.getpid()}_{variant}"
        # The same pressure harness and correctness checks run against both runtimes.
        for path in fixture.glob("*.go"):
            text = path.read_text()
            if variant == "short":
                for key, short in {"payload": "1", "coins": "2", "items": "3", "transfers": "4"}.items():
                    text = text.replace(f'bson:"{key}"', f'bson:"{short}"')
            (work / path.name).write_text(text)
        for path in (source / "codegen/internal/entity/testdata/dataengine/def").glob("*.go"):
            text = path.read_text().replace("roost_generated_it_placeholder", database)
            if variant == "short":
                for key, short in {"payload": "1", "coins": "2", "items": "3", "transfers": "4",
                                   "x": "5", "y": "6", "changed": "7"}.items():
                    text = text.replace(f'store:"{key}"', f'store:"{short}"')
            (work / "def" / path.name).write_text(text)
        (work / "go.mod").write_text(
            "module persistflow\n\ngo 1.27.0\n\nrequire github.com/tjbdwanghaibo/roost-core v0.0.0\n"
            f"replace github.com/tjbdwanghaibo/roost-core => {source}\n")
        commands = [(["go", "run", "./codegen/cmd/dao", "-def", str(work / "def"), "-out", str(work), "-pkg", "persistflow", "-force"], source),
                    (["go", "run", "./codegen/cmd/entity", "-dir", str(work), "-force"], source),
                    (["go", "test", "-mod=mod", "-c", "-o", "persist.test", "."], work)]
        for cmd, cwd in commands:
            if command(cmd, cwd, env, out / f"build-{variant}.log"):
                raise RuntimeError(f"Build failed: {variant}")
        metadata["builds"][variant] = {str(path.relative_to(work)): hashlib.sha256(path.read_bytes()).hexdigest()
                                         for path in sorted(work.rglob("*.go"))}
        builds[variant] = work
    (out / "environment.json").write_text(json.dumps(metadata, indent=2) + "\n")
    results = []
    for level, rate in enumerate(rates):
        for policy in policies:
            order = variants if (level + int(args.reverse_order)) % 2 == 0 else tuple(reversed(variants))
            for variant in order:
                name = f"{variant}-{policy}-{rate}"
                work = builds[variant]
                run = work / name
                run.mkdir()
                test_env = env | {"ROOST_DATAENGINE_PERF": "1", "ROOST_PERF_SHAPE": "pair",
                                  "ROOST_PERF_ENTITIES": "1000", "ROOST_PERF_WRITERS": "32",
                                  "ROOST_PERF_REQUESTS": str(rate * args.seconds), "ROOST_PERF_RATE": str(rate),
                                  "ROOST_PERF_POLICY": policy, "ROOST_PERF_OUTPUT": str(out / f"{name}.json"),
                                  "ROOST_GENERATED_WAL_DIR": str(run / "wal")}
                print(f"{time.strftime('%H:%M:%S')} start {name}", flush=True)
                code = command([str(work / "persist.test"), "-test.run", "^TestGeneratedDataEnginePressure$",
                                "-test.timeout", "600s", "-test.v"], work, test_env, out / f"{name}.log")
                cleanup = command([str(work / "persist.test"), "-test.run", "^TestGeneratedDataEngineProcessLifecycle$",
                                   "-test.timeout", "45s"], work, test_env | {"ROOST_GENERATED_PHASE": "cleanup"},
                                  out / f"{name}-cleanup.log")
                results.append({"name": name, "exit_code": code, "cleanup_exit_code": cleanup})
                (out / "runs.json").write_text(json.dumps(results, indent=2) + "\n")
                print(f"{time.strftime('%H:%M:%S')} done {name}: test={code} cleanup={cleanup}", flush=True)
                if cleanup:
                    raise RuntimeError("Database cleanup failed; stop before reusing the fixture")
    if any(result["exit_code"] for result in results):
        raise SystemExit(1)


if __name__ == "__main__":
    main()
