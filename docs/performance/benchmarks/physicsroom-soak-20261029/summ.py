#!/usr/bin/env python3
"""汇总长稳输出：起点 / 预热结束 / 终点，以及稳定窗口的最小二乘斜率。

usage: summ.py <go.tsv> <proc.tsv> [warmup_s=300]

go.tsv 由 TestPhysicsRoomSoak 写出（每窗口一行），proc.tsv 由 sample.py 写出（footprint / vmmap）。
估算的原生内存 = footprint − Go 映射的 dirty（footprint 的 untagged VM_ALLOCATE）；另给 malloc 已分配
（vmmap，PhysX 在 macOS 上经 PxDefaultAllocator 走 malloc）。斜率单位为每分钟。
"""
import sys


def load(path):
    with open(path) as f:
        head = f.readline().rstrip("\n").split("\t")
        rows = []
        for line in f:
            cols = line.rstrip("\n").split("\t")
            rows.append({k: (float(v) if v != "" else None) for k, v in zip(head, cols)})
    return rows


def slope(points):
    """最小二乘斜率与相对残差；points 为 (秒, 值)。"""
    points = [(x, y) for x, y in points if y is not None]
    n = len(points)
    if n < 3:
        return None, None, n
    mx = sum(x for x, _ in points) / n
    my = sum(y for _, y in points) / n
    sxx = sum((x - mx) ** 2 for x, _ in points)
    sxy = sum((x - mx) * (y - my) for x, y in points)
    b = sxy / sxx
    return b * 60, my, n


def main():
    go, proc = load(sys.argv[1]), load(sys.argv[2])
    warmup = float(sys.argv[3]) if len(sys.argv) > 3 else 300
    t0 = go[0]["unix_s"] - go[0]["elapsed_s"]
    for r in proc:
        r["elapsed_s"] = r["unix_s"] - t0
        r["native_est_bytes"] = r["footprint_bytes"] - r["go_vm_dirty_bytes"]
    for r in go:
        r["go_resident_bytes"] = r["go_mapped_bytes"] - r["go_released_bytes"]
    series = [
        ("rss_kb (ps, Go 侧)", go, "rss_kb", 1 / 1024, "MB"),
        ("footprint", proc, "footprint_bytes", 1 / 2**20, "MB"),
        ("Go 映射 dirty (footprint)", proc, "go_vm_dirty_bytes", 1 / 2**20, "MB"),
        ("估算原生 = footprint − Go dirty", proc, "native_est_bytes", 1 / 2**20, "MB"),
        ("malloc dirty (footprint)", proc, "malloc_dirty_bytes", 1 / 2**20, "MB"),
        ("malloc 已分配 (vmmap)", proc, "malloc_allocated_bytes", 1 / 2**20, "MB"),
        ("malloc 分配个数 (vmmap)", proc, "malloc_count", 1, "个"),
        ("Go heap in-use", go, "heap_inuse_bytes", 1 / 2**20, "MB"),
        ("Go 映射 − 已归还", go, "go_resident_bytes", 1 / 2**20, "MB"),
        ("Go heap 对象数", go, "heap_objects", 1, "个"),
        ("goroutines", go, "goroutines", 1, "个"),
        ("物理 Hz / 房间", go, "physics_hz", 1, "Hz"),
        ("最慢房间 Hz", go, "min_room_hz", 1, "Hz"),
        ("交付 Hz / 客户端", go, "delivered_hz", 1, "Hz"),
        ("Step P99", go, "step_p99_ms", 1, "ms"),
        ("Poll P99", go, "poll_p99_ms", 1, "ms"),
    ]
    print(f"窗口数 {len(go)}，时长 {go[-1]['elapsed_s']:.0f}s，稳定窗口取 elapsed ≥ {warmup:.0f}s")
    print("| 指标 | 起点 | 预热结束 | 终点 | 稳定窗口均值 | 稳定窗口斜率 / 分钟 | 点数 |")
    print("| --- | ---: | ---: | ---: | ---: | ---: | ---: |")
    for name, rows, key, scale, unit in series:
        vals = [(r["elapsed_s"], r[key]) for r in rows if r.get(key) is not None]
        if not vals:
            continue
        first = vals[0][1] * scale
        warm = next((v for t, v in vals if t >= warmup), vals[-1][1]) * scale
        last = vals[-1][1] * scale
        b, mean, n = slope([(t, v * scale) for t, v in vals if t >= warmup])
        bs = "-" if b is None else f"{b:+.4f}"
        ms = "-" if mean is None else f"{mean:.3f}"
        print(f"| {name} ({unit}) | {first:.3f} | {warm:.3f} | {last:.3f} | {ms} | {bs} | {n} |")
    # 累计量换算为速率
    a, z = go[0], go[-1]
    dt = z["elapsed_s"] - a["elapsed_s"]
    print(f"\n分配速率 {(z['total_alloc_bytes'] - a['total_alloc_bytes']) / 1e6 / dt:.2f} MB/s，GC {(z['gc_cycles'] - a['gc_cycles']) / dt:.2f} 次/s")
    print(f"空 Poll 合计 {sum(r['empty_polls'] for r in go):.0f}；物理 Hz 最小窗口 {min(r['physics_hz'] for r in go):.2f}，最慢房间最小 {min(r['min_room_hz'] for r in go):.2f}，交付 Hz 最小 {min(r['delivered_hz'] for r in go):.2f}")
    vm = [r["elapsed_s"] for r in proc if r.get("vmmap_ms") is not None]
    print("vmmap 采样时刻（s，会挂起进程约 0.4s，对应窗口物理 Hz 略低）:", ", ".join(f"{t:.0f}" for t in vm))


if __name__ == "__main__":
    main()
