import sys, json, re
# usage: condense.py <log> [regex on msg]  -> "HH:MM:SS.mmm LEVEL msg k=v ..." for matching lines (token values masked)
pat = re.compile(sys.argv[2]) if len(sys.argv) > 2 else None
skip = {"time","level","msg","goId","frame","server_time_ms"}
for line in open(sys.argv[1], errors="replace"):
    line=line.rstrip("\n")
    try:
        d=json.loads(line)
    except Exception:
        if pat is None or pat.search(line): print("  (stderr) "+line[:300])
        continue
    msg=d.get("msg","")
    if pat and not pat.search(msg): continue
    t=d.get("time","")[11:23]
    kv=" ".join(f"{k}={v}" for k,v in d.items() if k not in skip)
    kv=re.sub(r"[0-9a-f]{16}\|[^ |]+\|(\d+)\|\d+", r"<token>|<host>|\1|<ms>", kv)
    print(f"{t} {d.get('level','')[:4]} {msg} {kv}"[:400])
