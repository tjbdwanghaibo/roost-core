import csv,sys,glob,os
from collections import Counter
path=[p for p in glob.glob(os.path.join(sys.argv[1],'dispatch-*.csv')) if 'handlerAddExp' in open(p).read()][0]
rows=list(csv.DictReader(open(path)))
for r in rows:
    for k in ('admitted_us','ready_us','started_us','ended_us'): r[k]=int(r[k])
ae=[r for r in rows if r['handler']=='handlerAddExp']
W=Counter(t for r in ae for t in r['targets'].split()).most_common(1)[0][0]
t0=min(r['admitted_us'] for r in ae); burst=[r for r in ae if r['admitted_us']-t0<1_000_000]
t1=t0+3_000_000
w=sorted([r for r in rows if W in r['targets'].split()],key=lambda r:r['started_us'])
# World 链在 AddExp 第一次准入时排在它前面、尚未结束的作业
ahead=[r for r in w if r['admitted_us']<=t0 and r['ended_us']>t0]
print('burst AddExp admitted in first 1s:',len(burst))
print('World jobs admitted before first AddExp and not yet finished:',Counter(r['handler'] for r in ahead),
      'their remaining World time ms=%.0f'%(sum(r['ended_us']-max(r['started_us'],t0) for r in ahead)/1000))
inw=[r for r in w if r['started_us']<t1 and r['ended_us']>t0]
busy=sum(min(r['ended_us'],t1)-max(r['started_us'],t0) for r in inw)
print('World busy in [t0,t0+3s]: %.0f ms (%.0f%%), jobs %d'%(busy/1000,busy/30000,len(inw)), Counter(r['handler'] for r in inw))
ok=[r for r in burst if r['started_us']-r['admitted_us']<3_000_000-30_000]
print('burst AddExp started within 3s of admission:',len(ok))
fast=[r for r in burst if r['ended_us']-r['started_us']<3000]; slow=[r for r in burst if r['ended_us']-r['started_us']>=3000]
import statistics as st
print('burst AddExp run<3ms (likely deadline already expired):',len(fast),' run>=3ms:',len(slow),' median run of the latter %.1fms'%(st.median([r['ended_us']-r['started_us'] for r in slow])/1000))
print('started-admitted for run<3ms jobs: min %.0f ms'%(min(r['started_us']-r['admitted_us'] for r in fast)/1000) if fast else '')
