"""E-3 汇总：每轮丢拍、按原因分、PB 往返分位与丢拍现场的应答耗时；有 load_timers 的轮次列出往返组成。
用法：python3 -I summarize.py <gate-drop-* 目录>..."""
import json
import statistics as st
import sys

for d in sys.argv[1:]:
    r = json.load(open(f'{d}/result.json'))
    m = r['Requests']['messages'][0]
    ev = r['drop_events']
    resp = [e['last_response_ms'] for e in ev]
    print(f"{d.rstrip('/').rsplit('/', 1)[-1]}: scheduled={r['Scheduled']} completed={r['Completed']} dropped={r['Dropped']}")
    print(f"  reasons={r['drop_reasons']}")
    print(f"  detail={r['drop_detail']}")
    print(f"  PB p50={m['p50_ms']:.2f} p95={m['p95_ms']:.2f} p99={m['p99_ms']:.2f} max={m['max_ms']:.2f}ms; "
          f"drop events={len(ev)} slept={sum(e['slept'] for e in ev)} "
          f"last_response median={st.median(resp):.2f} min={min(resp):.2f} max={max(resp):.2f}ms; "
          f"last_sync max={max(e['last_sync_ms'] for e in ev):.2f}ms")
    for t in sorted(r.get('load_timers') or [], key=lambda t: -t['mean_ms'] * t['count'])[:5]:
        print(f"  timer {t['name']} {t['labels'] or ''} n={t['count']} mean={t['mean_ms']:.3f} max={t['max_ms']:.3f}ms")
