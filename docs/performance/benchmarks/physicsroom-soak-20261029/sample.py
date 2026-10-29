#!/usr/bin/env python3
"""进程外内存采样（macOS）：footprint 每 INTERVAL 秒一次，vmmap --summary 每 VMMAP_EVERY 次一次。

usage: sample.py <pid> <out.tsv> [interval_s=30] [vmmap_every=5]

footprint 给出物理占用（phys_footprint）与按类别的 dirty 字节：untagged VM_ALLOCATE 是 Go
运行时堆与栈的映射，MALLOC_* 是 libc malloc（PhysX 的 PxDefaultAllocator 在 macOS 上走 malloc）。
vmmap 给出 malloc 区已分配字节与分配个数，但会短暂挂起目标进程，所以降频并在输出里标记。
目标进程退出即结束。
"""
import re
import subprocess
import sys
import time

UNITS = {"": 1, "B": 1, "K": 1 << 10, "M": 1 << 20, "G": 1 << 30}


def size(text):
    m = re.fullmatch(r"([0-9.]+)([KMGB]?)", text)
    return int(float(m.group(1)) * UNITS[m.group(2)]) if m else 0


def footprint(pid):
    out = subprocess.run(["footprint", "-p", str(pid), "-f", "bytes"], capture_output=True, text=True).stdout
    total = re.search(r"Footprint: (\d+) B", out)
    if not total:
        return None
    go = malloc = 0
    for line in out.splitlines():
        m = re.match(r"\s*(\d+) B\s+\d+ B\s+\d+ B\s+\d+\s+(.+)$", line)
        if not m:
            continue
        dirty, category = int(m.group(1)), m.group(2).strip()
        if category == "untagged (VM_ALLOCATE)":
            go = dirty
        elif category.startswith("MALLOC"):
            malloc += dirty
    return int(total.group(1)), go, malloc


def vmmap(pid):
    out = subprocess.run(["vmmap", "--summary", str(pid)], capture_output=True, text=True).stdout
    allocated = count = 0
    in_zones = False
    for line in out.splitlines():
        if line.startswith("MALLOC ZONE"):
            in_zones = True
            continue
        if not in_zones or line.startswith("=") or not line.strip():
            continue
        cols = line.split()
        if len(cols) < 10 or not cols[-5].isdigit():
            continue  # 表后的说明行
        if cols[0] == "TOTAL":
            return size(cols[-4]), int(cols[-5])
        # 区名 虚拟 常驻 脏 换出 分配个数 已分配 碎片 %碎片 区域数
        allocated += size(cols[-4])
        count += int(cols[-5])
    return allocated, count


def rss(pid):
    out = subprocess.run(["ps", "-o", "rss=", "-p", str(pid)], capture_output=True, text=True).stdout.strip()
    return int(out) if out else None


def main():
    pid, path = int(sys.argv[1]), sys.argv[2]
    interval = float(sys.argv[3]) if len(sys.argv) > 3 else 30
    every = int(sys.argv[4]) if len(sys.argv) > 4 else 5
    with open(path, "w") as f:
        f.write("unix_s\trss_kb\tfootprint_bytes\tgo_vm_dirty_bytes\tmalloc_dirty_bytes\tmalloc_allocated_bytes\tmalloc_count\tvmmap_ms\n")
        n = 0
        while True:
            now = time.time()
            r = rss(pid)
            fp = footprint(pid)
            if r is None or fp is None:
                return
            allocated = count = elapsed = ""
            if n % every == 0:
                began = time.time()
                allocated, count = vmmap(pid)
                elapsed = int((time.time() - began) * 1000)
            f.write(f"{int(now)}\t{r}\t{fp[0]}\t{fp[1]}\t{fp[2]}\t{allocated}\t{count}\t{elapsed}\n")
            f.flush()
            n += 1
            time.sleep(max(0.0, interval - (time.time() - now)))


if __name__ == "__main__":
    main()
