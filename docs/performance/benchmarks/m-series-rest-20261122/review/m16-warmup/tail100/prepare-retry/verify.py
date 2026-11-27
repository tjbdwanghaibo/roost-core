"""Fully offline; requires parent tail100 validators and the raw archive, no Go or private work folder."""
from pathlib import Path
import json,sys,gzip,zipfile,hashlib
D=Path(__file__).resolve().parent;sys.path.insert(0,str(D.parent));from audit import check as delivery_check
from analyze import analyze
from validate100 import validate,private_localhost
readj=lambda p:json.loads(Path(p).read_bytes()); sha=lambda b:hashlib.sha256(b).hexdigest()
from collections import Counter,defaultdict
from bisect import bisect_right
schema=readj(D/'schema.json');NAMES=schema['result'];COL=schema['meter_costs'];REAS=schema['append_reason'];SET=schema['actual_settle'];need=lambda condition,label:None if condition else (_ for _ in ()).throw(ValueError(str(label)))
def union(pairs):
 m=[]
 for x,y in sorted(pairs):
  if x==y:continue
  if m and x<=m[-1][1]:m[-1][1]=max(y,m[-1][1])
  else:m.append([x,y])
 return m
def analyze_meter(t,report):
 s=t['server'];lo,end=t['formal_ns'],t['end_ns'];mid=(lo+end)//2;facts={(f['Subject'],f['Planned'],f['Changed']):f for o in t['interests'] for f in o['Facts']};rank=sorted((r[5]-r[4],c['index'],r[6],r[7],r[0],r[1],r[2],r[3],r[11],r[4],r[5],i) for c in t['client']['clients'] for i,r in enumerate(c['rows'] or []) if lo<r[3]<=end);need(rank,'no formal deliveries');co=rank[-((len(rank)+99)//100):];fx={facts[k[4],k[7],k[8]]['ID']:facts[k[4],k[7],k[8]] for k in co};windows=union([(f['Eligible'],f['Selected']) for f in fx.values()]);begins=[a for a,b in windows];uw=sum(b-a for a,b in windows);rs={r[0]:r for r in s['rounds']};need(len(rs)==len(s['round_work']),'round ownership');costs=defaultdict(lambda:defaultdict(Counter));returned=defaultdict(Counter);retries=defaultdict(Counter);fan=defaultdict(Counter);repeat=defaultdict(Counter);overlap=Counter();overlap_n=Counter();series={};closed=Counter();max_streak=(0,None);last={};formal_rids=0
 for w in s['round_work']:
  rid=w['round'];_,a,b,empty,ok,force=rs[rid];phase='half_first' if b<=mid else ('half_second' if a>=mid else 'cross_mid');formal=lo<=a<=b<=end and not force and ok;labels=['all','force' if force else 'ordinary']+(['formal',phase] if formal else []);formal_rids+=int(formal);objs=w['objects'] or [];codes=w.get('meter_codes') or [];need(len(objs)==len(codes),(rid,'codes vs objects',len(objs),len(codes)));local=Counter();object_ns=Counter();need(not (len(objs)>0 and empty),'empty with objects')
  for ob,v in zip(objs,codes):
   id,x,y,wants,inp=ob;n=v&15;snap=(v>>4)&1;rr=(v>>5)&3;sett=(v>>7)&3;need(v<512 and n<len(NAMES) and sett<len(SET) and (n!=0)==bool(wants) and (not snap or wants),(rid,ob,v));name=NAMES[n];local[name]+=1;object_ns[name]+=y-x
   if n in (1,2):need(rr==1,(rid,id,'pending without append'))
   if sett:need(rr!=0,(rid,id,'settle without append'))
   if rr and not sett:need(w.get('meter_retry_all') or not ok,(rid,id,'unaccounted requeue'))
   for key in labels:
    returned[key][name]+=1
    if rr:retries[key][REAS[rr]+'/'+SET[sett]]+=1
    if n==1:fan[key]['0' if inp==0 else '1' if inp==1 else '2..8' if inp<=8 else '9..64' if inp<=64 else '65+']+=1
    if n in (1,2) and id in last:
     prev,pn=last[id];repeat[key]['previous_visit_pending' if pn in (1,2) else 'previous_visit_not_pending']+=1
     if pn in (1,2):repeat[key]['same_result_previous_visit' if n==pn else 'different_pending_reason_previous_visit']+=1;repeat[key]['adjacent_round' if prev+1==rid else 'nonadjacent_round']+=1
   if n in (1,2):
    if id in series:series[id][1]+=1
    else:series[id]=[rid,1]
    length=series[id][1];max_streak=(length,id) if length>max_streak[0] else max_streak
   elif id in series:
    first,length=series.pop(id);closed[name]+=1
   last[id]=(rid,n)
   j=max(0,bisect_right(begins,x)-1);duration=0
   while j<len(windows) and windows[j][0]<y:
    l,h=windows[j];duration+=max(0,min(y,h)-max(x,l));j+=1
   if duration:overlap[name]+=duration;overlap_n[name]+=1
  found=set()
  for raw in w.get('meter_costs') or []:
   need(len(raw)==len(COL),(rid,'cost width',len(raw),len(COL)));z=dict(zip(COL,raw));n=z.pop('result');name=NAMES[n];need(name not in found and z['count']==local[name] and min(z.values())>=0,(rid,name,'count/cost'));found.add(name);need(z['object_ns']==sum(raw[7:])==object_ns[name],(rid,name,'object partition',z,object_ns[name]));need(z['api_ns']==sum(raw[11:22]),(rid,name,'api partition'));need((n==1)==(z['policy_hold_depth_max']>0),(rid,name,'hold priority'))
   for key in labels:
    dest=costs[key][name];prior=dest['policy_hold_depth_max'];dest.update(z);dest['policy_hold_depth_max']=max(prior,z['policy_hold_depth_max'])
  need(found==set(local)-{'no_prepare'} or found==set(local),(rid,'missing costs',found,set(local)))
 return dict(ok=True,complete_normal_formal_rounds=formal_rids,returned={k:dict(v) for k,v in returned.items()},costs={k:{j:dict(z) for j,z in v.items()} for k,v in costs.items()},retry_appends_and_settle={k:dict(v) for k,v in retries.items()},pending_consecutive_visit_details={k:dict(v) for k,v in repeat.items()},policy_hold_fanout_visits={k:dict(v) for k,v in fan.items()},whole_run_episodes=dict(closed_by_actual_next_visited_outcome=dict(closed),right_censored_subjects=len(series),max_consecutive_observed_pending_visits=max_streak),tail=dict(n=len(co),facts=len(fx),unique_eligible_selected_ns=uw,by_actual_object_result_ns=dict(overlap),intersecting_visits=dict(overlap_n),outside_object_ns=uw-sum(overlap.values())),boundary='Next observed visit may not be next round; no inference of success without an actual result. Object ns includes all observation overhead and partial endpoint waits use object IDs only, not prorated per-result stage sums.')

if __name__=='__main__':
 scope=sys.argv[1]
 with zipfile.ZipFile(D/(scope+'-raw.zip')) as f:
  read=f.read;r=json.loads(read('sample.json'));log=read('run.log').decode();code=json.loads(read('exit.json'))['original_exit'];stock=json.loads(read('stock.json'));assert stock['original_exit']==code and sha(read('trace.json.gz'))==stock['trace'] and sha(read('sample.json'))==stock['report'] and sha(read('run.log'))==stock['log'] and stock['ports']==private_localhost(log,[read(f'nats-{j}.log').decode() for j in range(3)]);g=validate(r,log,code)['gates'] if scope=='c4' else {};t=json.loads(gzip.decompress(read('trace.json.gz')));d=delivery_check(t,r);m=analyze_meter(t,r);c=analyze(t,r);assert d['ok'] and c['ok'];print(json.dumps(dict(ok=True,stock=stock,gates=g,delivery=d,meter=m,cohorts=c),ensure_ascii=False,indent=2,sort_keys=True))
