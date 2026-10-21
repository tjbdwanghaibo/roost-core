# 汇总 delivery.jsonl：按 (label, mode, res, active, shards) 分组，报中位数与 [最小, 最大]。
import json, sys, statistics as st
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
groups = {}
for r in rows:
    c = r["config"]
    key = (c["label"], c["mode"], c["res"], c["active"], c["shards"])
    groups.setdefault(key, []).append(r)
def g(r, path):
    v = r
    for p in path.split("."):
        v = v[p]
    return float(v)
def cell(rs, path, fmt):
    vals = sorted(g(r, path) for r in rs)
    med = st.median(vals)
    if len(vals) == 1:
        return fmt % med
    return (fmt % med) + " [" + (fmt % vals[0]) + "–" + (fmt % vals[-1]) + "]"
order = {"main": 0, "shards": 1, "base": 2}
keys = sorted(groups, key=lambda k: (order.get(k[0], 9), k[2], k[3], k[4], k[1]))
cols = [
 ("心跳回调/s", "hb_beats_per_s", "%.0f"),
 ("准入消息/s", "hb_admissions_per_s", "%.0f"),
 ("跳拍/s", "hb_skipped_per_s", "%.0f"),
 ("间隔 p50 ms", "interval_ms.p50", "%.1f"),
 ("间隔 p99 ms", "interval_ms.p99", "%.1f"),
 ("|偏差| p99 ms", "absdev_ms.p99", "%.2f"),
 ("迟到 p50 ms", "lateness_ms.p50", "%.2f"),
 ("迟到 p99 ms", "lateness_ms.p99", "%.2f"),
 ("快池等待峰值(采样)", "queue_peak_sampled", "%.0f"),
 ("快池等待峰值(准入)", "queue_peak_admission", "%.0f"),
 ("进程 CPU 核", "cpu_cores_process", "%.2f"),
 ("驱动 CPU 核", "driver_cores", "%.3f"),
 ("业务 p50 ms", "biz_latency_ms.p50", "%.3f"),
 ("业务 p99 ms", "biz_latency_ms.p99", "%.3f"),
 ("业务 p99.9 ms", "biz_latency_ms.p999", "%.3f"),
]
print("| 组 | 模式 | 分辨率 | 移动比例 | 驱动数 | 次数 | " + " | ".join(c[0] for c in cols) + " |")
print("| --- | --- | --- | ---: | ---: | ---: | " + " | ".join("---:" for _ in cols) + " |")
for k in keys:
    rs = groups[k]
    print("| %s | %s | %s | %g | %d | %d | " % (k[0], k[1], k[2], k[3], k[4], len(rs)) + " | ".join(cell(rs, c[1], c[2]) for c in cols) + " |")
extra = []
for k in keys:
    for r in groups[k]:
        bad = {f: r[f] for f in ("hb_queue_full_total", "hb_admit_err_total", "biz_fail_total", "short_rejected", "y_trylock_fail_total") if r[f]}
        if bad:
            extra.append((k, bad))
print()
print("非零的失败 / 重投计数：" + ("无" if not extra else ""))
for k, bad in extra:
    print("- %s: %s" % (k, bad))
