# 汇总 run-list.sh 的 sync-aoi 运行：每次运行一行写 <out>/runs.jsonl（口径同 interest-single-writer-20261022/agg-aoi.py，
# 另加运行开始时 1 分钟 loadavg、Interest 指标与心跳报告），再按“去掉 -r<轮> 的运行名”分组出表，每格按轮次列出。
import json, os, re, sys, glob
out = sys.argv[1]; order = sys.argv[2:]  # 可选：分组显示顺序（前缀）
la = {}
for l in open(os.path.join(out, "run.log")):
    m = re.match(r"\S+ (\S+) start loadavg=\{ (\S+)", l)
    if m: la[m.group(1)] = float(m.group(2))
def cpu_secs(t):
    s = 0.0
    for p in t.split(":"): s = s * 60 + float(p.split("-")[-1])
    return s
rows = []
for d in sorted(glob.glob(os.path.join(out, "runs", "*"))):
    name = os.path.basename(d)
    if not os.path.isdir(d) or ".discarded" in name: continue
    try:
        srv = json.load(open(os.path.join(d, "server.json"))); cli = json.load(open(os.path.join(d, "client.json")))
    except Exception as e:
        rows.append({"run": name, "error": str(e)}); continue
    mc = srv["manager_counters"]; calls = mc["FlushCalls"] or 1
    chg = cli.get("state_change_to_client_ms") or {}
    pol = (srv.get("same_basis") or {}).get("policy_phase_per_tick_ms") or {}
    cpu = {}
    if os.path.exists(d + ".cpu"):
        for line in open(d + ".cpu"):
            f = line.split()
            if len(f) == 3: cpu[(f[0], f[1])] = cpu_secs(f[2])
    im = srv.get("interest_metrics") or {}
    hb = srv.get("heartbeat") or {}
    r = {"run": name, "loadavg1": la.get(name),
         "chg_p50": chg.get("p50"), "chg_p99": chg.get("p99"),
         "full_p99": (cli.get("full_mutation_to_client_ms") or {}).get("p99"),
         "frames": cli.get("frames"), "vis": (srv.get("visible_final") or {}).get("mean"),
         "flushes": mc["FlushCalls"], "flush_ms": mc["FlushDuration"] / calls / 1e6, "policy_ms": mc.get("PolicyDuration", 0) / calls / 1e6,
         "pol_tick_p95": pol.get("p95"), "pol_tick_max": pol.get("max"),
         "work_p95": (srv.get("server_active_work_ms") or {}).get("p95"),
         "flush_failures": mc.get("FlushFailures"), "flush_failures_before_stop": srv.get("flush_failures_before_stop"), "manager_last_error": srv.get("manager_last_error"), "client_error": srv.get("client_error"),
         "server_cpu_s": round(sum(v for (p, x), v in cpu.items() if x == "server"), 2),
         "client_cpu_s": round(sum(v for (p, x), v in cpu.items() if x == "client"), 2),
         "budget_stop": (im.get("entitysync.interest.budget_stop.total") or {}).get("value", 0 if im else None),
         "log_backlog_end": (im.get("entitysync.interest.log_backlog") or {}).get("value"),
         "policy_hist_p95": (im.get("entitysync.interest.policy_duration") or {}).get("p95_ms"),
         "settle_hist_p95": (im.get("entitysync.interest.settle_duration") or {}).get("p95_ms"),
         "heartbeat": hb or None}
    rows.append(r)
with open(os.path.join(out, "runs.jsonl"), "w") as f:
    for r in rows: f.write(json.dumps(r, ensure_ascii=False) + "\n")
groups = {}
for r in rows:
    g = re.sub(r"-r\d+$", "", r["run"]); groups.setdefault(g, []).append(r)
def fmt(rs, f, p):
    return " / ".join("—" if r.get(f) is None else (p % r[f]) for r in sorted(rs, key=lambda r: r["run"]))
cols = [("loadavg", "loadavg1", "%.2f"), ("改值→客户端 P50", "chg_p50", "%.1f"), ("P99", "chg_p99", "%.1f"), ("全量 P99", "full_p99", "%.1f"),
        ("帧数", "frames", "%d"), ("Flush 次数", "flushes", "%d"), ("Flush 均值 ms", "flush_ms", "%.1f"), ("政策 P95/tick", "pol_tick_p95", "%.1f"),
        ("政策 max/tick", "pol_tick_max", "%.1f"), ("budget_stop", "budget_stop", "%d"), ("服务端 CPU s", "server_cpu_s", "%.1f"), ("客户端 CPU s", "client_cpu_s", "%.1f")]
print("| 配置 | " + " | ".join(c[0] for c in cols) + " |")
print("| --- | " + " | ".join("---:" for _ in cols) + " |")
keys = sorted(groups)
if order: keys = sorted(groups, key=lambda g: ([i for i, o in enumerate(order) if g.startswith(o)] or [99])[0])
for g in keys:
    print("| %s | " % g + " | ".join(fmt(groups[g], c[1], c[2]) for c in cols) + " |")
# 停止时取消在途准入会记一次 FlushFailures（manager_last_error = context canceled），只看停止前的失败。
bad = [r for r in rows if r.get("flush_failures_before_stop") or r.get("client_error") or "error" in r]
print("\n异常运行：" + ("无" if not bad else ""))
for r in bad: print("-", r["run"], {k: r.get(k) for k in ("error", "flush_failures_before_stop", "manager_last_error", "client_error")})
