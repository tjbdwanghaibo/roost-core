# 把 benchstat 输出整理成“结构为列”的表：每个指标一张表，单元格为中位数 ±CI。
import re, sys
text = open(sys.argv[1]).read().split("\n")
metric = None; data = {}
for line in text:
    m = re.match(r"\s+│\s+(\S.*?)\s+│\s*$", line)
    if m and not m.group(1).endswith(".txt"):
        metric = m.group(1); continue
    m = re.match(r"(?:Sched/)?(op=\S+|SSRMapHint/\S+)-10\s+(\S+)\s+±\s*(\S+)", line)
    if m and metric:
        data.setdefault(metric, {})[m.group(1)] = (m.group(2), m.group(3))
impls = ["ssr50", "tier50", "tier10", "wheel10", "heap10"]
def table(metric, op, title):
    d = data.get(metric, {})
    print("\n**%s**（%s）\n" % (title, metric))
    print("| 登记数 / 分布 | " + " | ".join(impls) + " |")
    print("| --- | " + " | ".join("---:" for _ in impls) + " |")
    for n in ["50000", "500000"]:
        for mix in ["d20mix", "all100"]:
            row = []
            for im in impls:
                k = "op=%s/impl=%s/n=%s/mix=%s" % (op, im, n, mix)
                v = d.get(k)
                row.append("%s ±%s" % v if v else "—")
            print("| %s / %s | " % (n, mix) + " | ".join(row) + " |")
for op, title in [("add", "登记"), ("remove", "撤销"), ("settier", "改档 100ms↔1s")]:
    table("sec/op", op, title); table("allocs/op", op, title + " 分配")
table("sec/op", "step", "每模拟秒收集到期项的驱动耗时")
table("p50-sec/step", "step", "每格耗时 p50"); table("p99-sec/step", "step", "每格耗时 p99"); table("max-sec/step", "step", "每格耗时 max")
table("liveB/reg", "build", "常驻内存 / 登记"); table("sec/op", "build", "从空建到 n 个登记")
print("\nssr 每格 map 预分配：", data.get("liveB", {}))
