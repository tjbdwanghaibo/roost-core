# 汇总 delivery.jsonl：按 (组, 移动比例) 分组，报中位数 [最小–最大]。组名取 HB_LABEL 的 "/" 之前部分。
import json, sys, statistics as st
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
groups = {}
for r in rows:
    c = r["config"]
    lab = c["label"].split("/")[0]
    act = c["active"] if c["mode"] != "none" else 0
    groups.setdefault((lab, act), []).append(r)
def g(r, path):
    v = r
    if path == "c_converted_per_s" and path not in r:
        return 0.0
    for p in path.split("."):
        v = v[p]
    return float(v)
def cell(rs, path, fmt):
    vals = sorted(g(r, path) for r in rs)
    med = st.median(vals)
    if len(vals) == 1:
        return fmt % med
    return (fmt % med) + " [" + (fmt % vals[0]) + "–" + (fmt % vals[-1]) + "]"
order = ["none", "X0", "X", "Y", "C", "C20"]
keys = sorted(groups, key=lambda k: (k[1], order.index(k[0]) if k[0] in order else 99))
cols = [
 ("准入消息/s", "hb_admissions_per_s", "%.0f"),
 ("进程 CPU 核", "cpu_cores_process", "%.2f"),
 ("驱动 CPU 核", "driver_cores", "%.3f"),
 ("业务 p50 ms", "biz_latency_ms.p50", "%.3f"),
 ("业务 p99 ms", "biz_latency_ms.p99", "%.3f"),
 ("业务 p99.9 ms", "biz_latency_ms.p999", "%.2f"),
 ("迟到 p50 ms", "lateness_ms.p50", "%.2f"),
 ("迟到 p99 ms", "lateness_ms.p99", "%.2f"),
 ("间隔 p99 ms", "interval_ms.p99", "%.1f"),
 ("快池等待峰值", "queue_peak_admission", "%.0f"),
 ("跳拍/s", "hb_skipped_per_s", "%.0f"),
 ("冲突转单目标/s", "c_converted_per_s", "%.1f"),
]
print("| 组 | 移动 | 次数 | " + " | ".join(c[0] for c in cols) + " |")
print("| --- | ---: | ---: | " + " | ".join("---:" for _ in cols) + " |")
for k in keys:
    rs = groups[k]
    print("| %s | %s | %d | " % (k[0], ("%g%%" % (k[1]*100)) if k[1] else "—", len(rs)) + " | ".join(cell(rs, c[1], c[2]) for c in cols) + " |")
extra = []
for k in keys:
    for r in groups[k]:
        bad = {f: r[f] for f in ("hb_queue_full_total", "hb_admit_err_total", "biz_fail_total", "short_rejected", "hb_skipped_total", "c_handed_back_total") if r.get(f)}
        if bad:
            extra.append((k, r["config"]["label"], bad))
print()
print("非零的跳拍 / 准入失败 / 业务失败 / 快池拒绝：" + ("无" if not extra else ""))
for k, lab, bad in extra:
    print("- %s: %s" % (lab, bad))
