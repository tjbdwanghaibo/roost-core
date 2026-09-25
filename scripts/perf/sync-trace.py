#!/usr/bin/env python3
"""Associate bounded Sync diagnostics with the worst client outliers (offline)."""
import argparse
import json
from pathlib import Path


def analyze(directory, limit):
    client = json.loads((directory / "client.json").read_text())
    samples = sorted(client.get("outliers", []),
                     key=lambda x: x["Received"] - x["Changed"], reverse=True)[:limit]
    results = [{"sample": x, "stages": {}} for x in samples]
    by_subject, by_session = {}, {}
    for result in results:
        x = result["sample"]
        by_subject.setdefault(x["SubjectID"], []).append(result)
        by_session.setdefault(x["Session"], []).append(result)
    overwritten = 0
    with (directory / "trace.jsonl").open() as stream:
        for line in stream:
            e = json.loads(line)
            overwritten += e.get("overwritten", 0)
            candidates = by_subject.get(e.get("SubjectID"), []) if e.get("SubjectID") else by_session.get(e.get("Session"), [])
            for result in candidates:
                x, stages = result["sample"], result["stages"]
                at, stage = e.get("At", 0), e.get("Stage")
                if not at or at > x["Received"]:
                    continue
                if e.get("SubjectID"):
                    if e.get("Session"):
                        if e["Session"] != x["Session"] or e.get("Epoch", 0) not in (0, x["Epoch"]):
                            continue
                        if stage == "selected" and e["Version"] != x["Version"]:
                            continue
                    elif at < x["Changed"]:
                        continue
                    if stage == "prepared" and e["Version"] != x["Version"]:
                        continue
                elif e.get("Epoch") != x["Epoch"] or e.get("Tick") != x["Tick"]:
                    continue
                stages[stage] = e
    for result in results:
        x, stages = result["sample"], result["stages"]
        spans = {"change_to_receive": (x["Received"] - x["Changed"]) / 1e6,
                 "admission_to_receive": (x["Received"] - x["Admitted"]) / 1e6}
        for stage, event in stages.items():
            spans["change_to_" + stage] = (event["At"] - x["Changed"]) / 1e6
        request, selected = stages.get("snapshot_requested"), stages.get("selected")
        if request and selected and selected["Snapshot"] and request["At"] <= selected["At"]:
            spans["snapshot_request_to_selection"] = (selected["At"] - request["At"]) / 1e6
            result["classification"] = "snapshot_requested_after_change" if request["At"] > x["Changed"] else "snapshot_pending_at_change"
        else:
            result["classification"] = "incomplete_or_non_snapshot"
        result["milliseconds"] = spans
    report = {"trace_overwritten": overwritten,
              "client_outliers_omitted": client.get("outliers_omitted", 0),
              "note": "Diagnostic runs affect timing. Missing/overwritten stages are not evidence of zero delay. Original latency gates remain unchanged.",
              "results": results}
    (directory / "outlier-stages.json").write_text(json.dumps(report, indent=2) + "\n")
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path, help="sample-N directory containing client.json and trace.jsonl")
    parser.add_argument("--limit", type=int, default=16)
    args = parser.parse_args()
    if not 1 <= args.limit <= 256:
        parser.error("limit must be 1..256")
    report = analyze(args.directory, args.limit)
    for result in report["results"][:3]:
        print(json.dumps({"subject": result["sample"]["SubjectID"], "classification": result["classification"], "milliseconds": result["milliseconds"]}))
    print("trace_overwritten:", report["trace_overwritten"])
