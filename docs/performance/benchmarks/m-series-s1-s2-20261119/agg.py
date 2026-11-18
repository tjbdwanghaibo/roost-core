import json, sys, statistics as st
# 用法：python3 -I agg.py <jsonl>；按 label 去掉 r1-/r2- 前缀分组，列中位数与每轮值
rows = [json.loads(l) for l in open(sys.argv[1]) if l.strip()]
groups = {}
for r in rows:
    lab = r["config"]["label"].split("-", 1)[1]
    groups.setdefault(lab, []).append(r)
def f(v, d=3):
    return f"{v:.{d}f}"
print("| 组 | n | 进程 CPU 核 | 驱动 CPU 核 | 业务 p50 / p99 / p99.9 ms | 业务 max ms | 迟到 p50 / p99 ms | 间隔 p99 ms | 拍/s | 转单目标/s | 转单目标/拍 | 快池等待峰值 | 跳拍/队列满/准入失败/业务失败/快池拒绝 |")
print("| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |")
for lab, rs in groups.items():
    med = lambda k: st.median(r[k] for r in rs)
    medm = lambda k, q: st.median(r[k][q] for r in rs)
    per = lambda k, d=3: "/".join(f(r[k], d) for r in rs)
    perm = lambda k, q, d=3: "/".join(f(r[k][q], d) for r in rs)
    errs = "/".join(str(sum(r[k] for r in rs)) for k in ["hb_skipped_total", "hb_queue_full_total", "hb_admit_err_total", "biz_fail_total", "short_rejected"])
    conv = [r.get("c_converted_per_beat", 0) for r in rs]
    print(f"| {lab} | {len(rs)} | {f(med('cpu_cores_process'),2)} ({per('cpu_cores_process',2)}) | {f(med('driver_cores'))} | "
          f"{f(medm('biz_latency_ms','p50'))} / {f(medm('biz_latency_ms','p99'))} ({perm('biz_latency_ms','p99')}) / {f(medm('biz_latency_ms','p999'))} | "
          f"{f(medm('biz_latency_ms','max'),2)} | {f(medm('lateness_ms','p50'),2)} / {f(medm('lateness_ms','p99'),2)} ({perm('lateness_ms','p99',2)}) | "
          f"{f(medm('interval_ms','p99'),1)} | {f(med('hb_beats_per_s'),0)} | {f(med('c_converted_per_s'),1)} | {f(st.median(conv),5)} | "
          f"{f(med('queue_peak_admission'),0)} | {errs} |")
