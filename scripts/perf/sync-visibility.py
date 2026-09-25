#!/usr/bin/env python3
"""Measure cold visibility and reset-cohort completion from same-host diagnostics."""
import argparse
import json
import math
from pathlib import Path


STAGES = {"measurement_started", "recovery_started", "session_reset",
          "baseline_requested", "baseline_cancelled"}


def quantiles(values):
    if not values:
        return None
    values = sorted(values)
    result = {"min": values[0], "max": values[-1], "mean": sum(values) / len(values)}
    result.update({name: values[math.ceil(len(values) * q) - 1]
                   for name, q in (("p50", .5), ("p95", .95), ("p99", .99))})
    return result


def summarize(events, receipts, overwritten=0, omitted=0):
    starts = [e["At"] for e in events if e["Stage"] == "measurement_started"]
    recoveries = [e["At"] for e in events if e["Stage"] == "recovery_started"]
    start = min(starts, default=0)
    timeline = list(events) + [dict(r, Stage="received") for r in receipts]
    # Same timestamp: requests must be visible before their receive/cancel.
    timeline.sort(key=lambda e: (e["At"], e["Stage"] in ("received", "baseline_cancelled")))
    pending, present, cohorts, lifetimes = {}, set(), {}, {}
    cancelled_inflight = {}
    cancelled_receipts = 0
    unmatched_examples = []
    samples, unmatched = [], 0
    for e in timeline:
        stage, at = e["Stage"], e["At"]
        if stage in ("measurement_started", "recovery_started"):
            continue
        session = (e["Session"], e["Epoch"])
        if e.get("Lifetime"):
            lifetimes.setdefault(session, set()).add(e["Lifetime"])
        if stage == "session_reset":
            if at >= start:
                cohorts[session] = {"session": session[0], "epoch": session[1],
                                    "started_ns": at, "members": []}
            continue
        key = session + (e["SubjectID"],)
        if stage == "baseline_requested":
            # Repeated profile changes while cold retain the first request time.
            # A Create may reach the client before the server adopts the frame.
            if key in present or key in pending:
                continue
            sample = {"requested": at, "received": None, "cancelled": None,
                      "recovery": bool(e.get("Snapshot")) and session in cohorts}
            pending[key] = sample
            samples.append(sample)
            if sample["recovery"]:
                cohorts[session]["members"].append(sample)
        elif stage == "baseline_cancelled":
            sample = pending.pop(key, None)
            if sample is not None:
                sample["cancelled"] = at
                cancelled_inflight[key] = sample
        elif stage == "received":
            if e.get("Operation", 1) == 3:  # ObjectRemove
                present.discard(key)
                cancelled_inflight.pop(key, None)
                continue
            present.add(key)
            sample = pending.pop(key, None)
            if sample is None:
                if cancelled_inflight.pop(key, None) is not None:
                    # Reliable Create already in flight when AOI withdrew interest.
                    # This remains a cancellation, not a successful recovery.
                    cancelled_receipts += 1
                else:
                    unmatched += 1
                    if len(unmatched_examples) < 16:
                        unmatched_examples.append(e)
            else:
                sample["received"] = at
    fresh = [s for s in samples if s["requested"] >= start and not s["recovery"]]
    recovery = [s for s in samples if s["recovery"]]
    def stats(group):
        received = [s for s in group if s["received"] is not None]
        cancelled = sum(s["cancelled"] is not None for s in group)
        return {"requested": len(group), "received": len(received), "cancelled": cancelled,
                "pending": len(group) - len(received) - cancelled,
                "request_to_client_ms": quantiles([(s["received"] - s["requested"]) / 1e6 for s in received])}
    complete = (len(starts) == 1 and len(recoveries) <= 1 and not overwritten and not omitted
                and not unmatched and all(len(v) == 1 for v in lifetimes.values()))
    sessions = []
    for cohort in cohorts.values():
        group = cohort.pop("members")
        state = stats(group)
        settled = max([s["received"] or s["cancelled"] or 0 for s in group] + [cohort["started_ns"]])
        cohort.update(state)
        cohort["settled_at_ns"] = settled if not state["pending"] else None
        cohort["settled_ms"] = (settled - cohort["started_ns"]) / 1e6 if not state["pending"] and complete else None
        # Cancelled entities are NOT counted as received.
        cohort["all_received_ms"] = cohort["settled_ms"] if not state["cancelled"] else None
        sessions.append(cohort)
    all_settled = bool(sessions) and all(s["settled_at_ns"] is not None for s in sessions)
    end = max((s["settled_at_ns"] or 0 for s in sessions), default=0)
    settled_ms = (end - recoveries[0]) / 1e6 if complete and len(recoveries) == 1 and all_settled else None
    recovery_stats = stats(recovery)
    return {"complete_evidence": complete, "trace_overwritten": overwritten, "client_records_omitted": omitted,
            "unmatched_creates": unmatched, "unmatched_examples": unmatched_examples,
            "creates_received_after_cancellation": cancelled_receipts, "new_visibility": stats(fresh), "recovery_baselines": recovery_stats,
            "recovery_cohort_settled_ms": settled_ms,
            "recovery_cohort_all_received_ms": settled_ms if not recovery_stats["cancelled"] else None,
            "recovery_sessions": sessions,
            "note": "Same-host diagnostic timing, not a performance gate. Recovery cohort is the subscriptions held at reset. Cancelled entries are separate; new entries are measured separately. Missing evidence invalidates completion claims."}


def analyze(directory):
    records = json.loads((directory / "baseline-receipts.json").read_text())
    events, overwritten = [], 0
    with (directory / "trace.jsonl").open() as stream:
        for line in stream:
            event = json.loads(line)
            overwritten += event.get("overwritten", 0)
            if event.get("Stage") in STAGES:
                if len(events) >= 2_000_000:
                    raise ValueError("visibility analysis exceeds 2000000 events; use shorter diagnostic runs")
                events.append(event)
    report = summarize(events, records["receipts"] or [], overwritten, records["omitted"])
    client = json.loads((directory / "client.json").read_text())
    config = json.loads((directory / "config.json").read_text())
    expected = (config.get("reconnect_players") or config["players"]) if config.get("reconnect_tick") else 0
    verified = client.get("recovery_client_verified_sessions", 0)
    report["recovery_expected_sessions"] = expected
    report["recovery_client_verified_sessions"] = verified
    report["recovery_client_verified_ms"] = client.get("recovery_client_verified_ms")
    report["recovery_checkpoint_complete"] = verified == expected and not client.get("errors")
    # A checkpoint is an actual client set comparison after queue drain. Its time
    # is an upper bound on convergence, not the first instant of convergence.
    report["checkpoint_note"] = "Current Interest set verified on each reset client; diagnostic checkpoint provides an upper bound, independently of cohort cancellations."
    if len(report["recovery_sessions"]) != expected:
        report["complete_evidence"] = False
        report["recovery_cohort_settled_ms"] = None
        report["recovery_cohort_all_received_ms"] = None
    (directory / "visibility-latency.json").write_text(json.dumps(report, indent=2) + "\n")
    return report


if __name__ == "__main__":
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("directory", type=Path)
    args = parser.parse_args()
    result = analyze(args.directory)
    print(json.dumps({k: v for k, v in result.items() if k != "recovery_sessions"}, indent=2))
    if not result["complete_evidence"] or not result["recovery_checkpoint_complete"]:
        raise SystemExit("incomplete diagnostic evidence; completion not certified")
