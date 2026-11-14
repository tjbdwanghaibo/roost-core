import json,collections,sys
P=collections.defaultdict(dict)
def ts(d): return d['server_time_ms']/1000.0 if 'server_time_ms' in d else None
for l in open(sys.argv[1]):
    try: d=json.loads(l)
    except: continue
    pid=d.get('player_id')
    if not pid: continue
    m=d['msg']; t=ts(d)
    p=P[pid]
    key={'login: handshake authenticated':'hs','enter: first entry confirmed':'init','enter: player entered':'done'}.get(m)
    if key: p[key]=t
    elif d['level'] in('WARN','ERROR'):
        p.setdefault('fail',(t,m,d.get('hook','')))
ok=[p for p in P.values() if 'done' in p and 'fail' not in p]
bad=[p for p in P.values() if 'fail' in p]
print('players',len(P),'ok',len(ok),'bad',len(bad))
t0=min(p['hs'] for p in P.values() if 'hs' in p)
def q(xs): xs=sorted(xs); return [round(xs[min(len(xs)-1,int(len(xs)*f))],3) for f in (0,.5,.9,.99,1)] if xs else None
print('ok  hs',q([p['hs']-t0 for p in ok]),'hs->init',q([p['init']-p['hs'] for p in ok if 'init' in p]),'init->done',q([p['done']-p['init'] for p in ok if 'init' in p]),'hs->done',q([p['done']-p['hs'] for p in ok]))
print('bad hs',q([p['hs']-t0 for p in bad]),'hs->init',q([p['init']-p['hs'] for p in bad if 'init' in p]),'init->fail',q([p['fail'][0]-p['init'] for p in bad if 'init' in p]),'hs->fail',q([p['fail'][0]-p['hs'] for p in bad]))
print(collections.Counter((p['fail'][1][:50],p['fail'][2]) for p in bad))
b=collections.Counter(); g=collections.Counter()
for p in P.values():
  if 'hs' not in p: continue
  k=int(p['hs']-t0)
  (b if 'fail' in p else g)[k]+=1
for k in sorted(set(b)|set(g)): print('hs second',k,'ok',g[k],'bad',b[k])
