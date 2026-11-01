#!/usr/bin/env python3
"""Run sequential native apply microbenchmarks; never mix build time into results."""
import argparse, json, pathlib, platform, subprocess, time

p = argparse.ArgumentParser()
p.add_argument("--binary", required=True)
p.add_argument("--output", required=True)
a = p.parse_args()
output = pathlib.Path(a.output)
if output.exists():
    p.error("output exists")
rows = []
for visible in (50, 500, 2000, 10000):
    for dirty in (1, 5):
        for repeat in range(3):
            row = json.loads(
                subprocess.check_output([a.binary, str(visible), str(dirty)], text=True)
            )
            row["repeat"] = repeat + 1
            rows.append(row)
report = {
    "kind": "native EntitySync apply microbenchmark, not network latency or Unity frame timing",
    "platform": platform.platform(),
    "commit": subprocess.check_output(["git", "rev-parse", "HEAD"], text=True).strip(),
    "timestamp": time.strftime("%Y-%m-%dT%H:%M:%S%z"),
    "rows": rows,
}
output.parent.mkdir(parents=True, exist_ok=True)
output.write_text(json.dumps(report, indent=2) + "\n")
print(output)
