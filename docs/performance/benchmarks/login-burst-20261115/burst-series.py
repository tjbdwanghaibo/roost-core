import sys,collections
rows=collections.defaultdict(dict)
for l in open(sys.argv[1]):
    p=l.split()
    if len(p)<4: continue
    ts=float(p[0]); port=p[1]; name=p[2]; val=float(p[3])
    rows[round(ts,1)][port+' '+name]=val
keys=['9100 player_tcp_connections','9100 nest_handler_lock_hold_count{handler="handlerRecordEnter"}','9100 nest_handler_lock_hold_sum_nanos{handler="handlerRecordEnter"}','9100 nestwal_fsync_duration_count','9100 nestwal_fsync_duration_sum_nanos','9100 player_tcp_dispatch_timeout_total']
qk=[k for r in rows.values() for k in r if 'queue_len' in k]
qk=sorted(set(qk))
prev=None
print('t conns RE_count RE_holdavg_ms fsync_n fsync_avg_ms timeouts', ' '.join(k.split('{')[1] if '{' in k else k for k in qk))
for t in sorted(rows):
    r=rows[t]
    if '9100 player_tcp_connections' not in r: continue
    c=r.get(keys[1],0); s=r.get(keys[2],0); fc=r.get(keys[3],0); fs=r.get(keys[4],0)
    if prev:
        dc=c-prev[0]; ds=s-prev[1]; dfc=fc-prev[2]; dfs=fs-prev[3]
        hold=ds/dc/1e6 if dc else 0; fav=dfs/dfc/1e6 if dfc else 0
    else: dc=hold=dfc=fav=0
    prev=(c,s,fc,fs)
    if r.get(keys[0],0)==0 and dc==0: continue
    print(f"{t:.1f} {r.get(keys[0],0):.0f} +{dc:.0f} {hold:.1f} +{dfc:.0f} {fav:.1f} {r.get(keys[5],0):.0f}", ' '.join(f"{r.get(k,0):.0f}" for k in qk))
