# 汇总 run-aoi.sh 的结果：每次运行一行 jsonl（口径同 benchmarks/d19-multiview-20261021/applymode-runs.jsonl，另加 CPU 秒），
# 再按配置出 base / after 对照表（每格 "第1轮 / 第2轮"）。
import json, os, re, sys, glob
out = sys.argv[1]
rows = []
def cpu_secs(t):
    # ps time: [[dd-]hh:]mm:ss.cc
    parts = t.split(":")
    secs = 0.0
    for p in parts:
        secs = secs * 60 + float(p.split("-")[-1])
    return secs
for d in sorted(glob.glob(os.path.join(out, "runs", "*"))):
    name = os.path.basename(d)
    if not os.path.isdir(d) or ".discarded" in name:
        continue
    try:
        srv = json.load(open(os.path.join(d, "server.json")))
        cli = json.load(open(os.path.join(d, "client.json")))
    except Exception as e:
        rows.append({"run": name, "error": str(e)}); continue
    mc = srv["manager_counters"]; calls = mc["FlushCalls"] or 1
    ns = 1e6
    chg = cli.get("state_change_to_client_ms") or {}
    sb = srv.get("same_basis", {})
    pol = sb.get("policy_phase_per_tick_ms") or {}
    drv = sb.get("driver_interest_per_tick_ms") or {}
    cpu = {}
    cf = d + ".cpu"
    if os.path.exists(cf):
        for line in open(cf):
            f = line.split()
            if len(f) == 3:
                cpu[(f[0], f[1])] = cpu_secs(f[2])
    srv_cpu = sum(v for (p, r), v in cpu.items() if r == "server")
    cli_cpu = sum(v for (p, r), v in cpu.items() if r == "client")
    r = {"run": name,
         "chg_p50": chg.get("p50"), "chg_p95": chg.get("p95"), "chg_p99": chg.get("p99"), "chg_max": chg.get("max"),
         "delta_p99": (cli.get("delta_mutation_to_client_ms") or {}).get("p99"),
         "full_p99": (cli.get("full_mutation_to_client_ms") or {}).get("p99"),
         "frames": cli.get("frames"), "vis": (srv.get("visible_final") or {}).get("mean"),
         "flushes": mc["FlushCalls"], "flush_ms": mc["FlushDuration"] / calls / ns, "policy_ms": mc["PolicyDuration"] / calls / ns,
         "pack_ms": (mc["FlushDuration"] - mc["PolicyDuration"]) / calls / ns,
         "pol_tick_p50": pol.get("p50"), "pol_tick_p95": pol.get("p95"), "pol_tick_max": pol.get("max"),
         "drv_tick_p50": drv.get("p50"), "drv_tick_p95": drv.get("p95"),
         "flush_failures": mc.get("FlushFailures"), "flush_failures_before_stop": srv.get("flush_failures_before_stop"),
         "manager_last_error": srv.get("manager_last_error"), "client_error": srv.get("client_error"),
         "server_cpu_s": round(srv_cpu, 2), "client_cpu_s": round(cli_cpu, 2), "elapsed_s": srv.get("elapsed_seconds"),
         "slow_ticks": srv.get("slow_ticks"), "work_over_50ms_ticks": srv.get("server_work_over_50ms_ticks")}
    rows.append(r)
with open(os.path.join(out, "aoi-runs.jsonl"), "w") as f:
    for r in rows:
        f.write(json.dumps(r, ensure_ascii=False) + "\n")
def key(name):
    m = re.match(r"(\w)-v(\d+)-(sync|async)-(nest|direct)-(base|after)-r(\d+)", name)
    return m.groups() if m else None
groups = {}
for r in rows:
    k = key(r["run"])
    if not k or "error" in r: continue
    sc, v, a, d, which, rnd = k
    groups.setdefault((sc, int(v), d, a, which), []).append(r)
def fmt(rs, f, p="%.1f"):
    vals = [r.get(f) for r in sorted(rs, key=lambda r: r["run"])]
    return " / ".join(("—" if x is None else (p % x)) for x in vals)
cols = [("P50", "chg_p50", "%.1f"), ("P99", "chg_p99", "%.1f"), ("帧数", "frames", "%d"), ("Flush 均值", "flush_ms", "%.1f"),
        ("政策", "policy_ms", "%.1f"), ("政策 P95/tick", "pol_tick_p95", "%.1f"), ("压测线程 P50/tick", "drv_tick_p50", "%.1f"),
        ("全量 P99", "full_p99", "%.1f"), ("Flush 次数", "flushes", "%d"), ("服务端 CPU s", "server_cpu_s", "%.1f")]
print("| 场景 | 视野 | 驱动 | 模式 | 代码 | " + " | ".join(c[0] for c in cols) + " |")
print("| --- | ---: | --- | --- | --- | " + " | ".join("---:" for _ in cols) + " |")
for k in sorted(groups, key=lambda k: (k[0], k[1], k[2], k[3], 0 if k[4] == "base" else 1)):
    rs = groups[k]
    print("| %s | %d | %s | %s | %s | " % k + " | ".join(fmt(rs, c[1], c[2]) for c in cols) + " |")
bad = [r for r in rows if r.get("flush_failures") or r.get("manager_last_error") or r.get("client_error") or "error" in r]
print("\n异常运行：" + ("无" if not bad else ""))
for r in bad:
    print("-", r["run"], {k: r.get(k) for k in ("error", "flush_failures", "manager_last_error", "client_error")})
