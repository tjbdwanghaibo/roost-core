"""Independent exact ID, frame, time and stock reconciliation; acceptance traces remain separate."""
from collections import Counter,defaultdict
import gzip,json,re,sys
from pathlib import Path

def need(v,msg):
 if not v:raise ValueError(msg)
def check(t,r):
 lo,hi=t['formal_ns'],t['end_ns'];c=r['Config'];s=r['SyncReport'];full=c['Players']==1000;limit=100 if full else 50;need(c['Frontend']=='gate' and c['Warmup']==lo-t['start_ns'] and hi-lo==c['Duration'] and r['CountsVerified'] and r['EntityValuesVerified'] and s['ValuesVerified'] and not r.get('ClientFirstFailure'),'wrong config/values');need(s['CommittedP99LimitMS']==limit,'wrong sync gate');x=t['client'];sv=t['server'];need(x['schema']==2 and len(x['clients'])==c['Players'],'client schema/size');facts=t['interests'];need(len(facts)==1,'Interest ownership');fs=facts[0]['Facts'];need(len(fs)==sum(r[k]['EntityChanges'] for k in ('Messages','Heartbeats')),'producer facts vs stock changes');fk={};ids=set();plannedset=set();
 for f in fs:
  key=(f['Subject'],f['Planned'],f['Changed']);need(f['ID'] not in ids and key not in fk and 0<f['Planned']<=f['Changed']<=f['Registered']<=f['Ready']<=f['Eligible']<=f['Selected']<=f['Released'] and f['Exception']==0,'invalid/ambiguous fact '+str((key,f)));fk[key]=f;ids.add(f['ID']);
  if lo<key[1]<=hi:plannedset.add(f['ID'])
 need(len(plannedset)==sum(r[k]['MeasuredEntityChanges'] for k in ('Messages','Heartbeats')),'formal source plans vs stock');pre=defaultdict(list);frm=defaultdict(list)
 for subject,b,v,ns in sv['prepared']:pre[subject,b,v].append(ns)
 for row in sv['frames']:frm[tuple(row[1:4])].append(row)
 lags=[[],[]];over=[0,0];active=set();versions=set();n=0;inversions=Counter();sids=set();degree=Counter();commits={};all_ns=Counter();foot=Counter();
 for st in x['clients']:
  opens=st['opens'];need(len(opens)==1 and opens[0][1]==1,'unusable/reused session');sid=opens[0][0];need(sid not in sids,'cross-client SID');sids.add(sid);seen=set()
  for rr in st['rows'] or []:
   sub,b,v,planned,commit,end,ep,tick,apply,locked,decoded,changed=rr;pk=(sub,b,v);key=(sub,planned,changed);wire=(ep,tick,*pk);need(wire not in seen and v>b and lo<planned<=hi and key in fk and len(pre[pk])==1 and len(frm[sid,ep,tick])==1,'unpaired/repeated/tag mismatch');seen.add(wire);f=fk[key];fr=frm[sid,ep,tick][0];need(fr[8] is True and fr[7]>=fr[6],'push unsuccessful');z=f['ID'];need(commits.setdefault(z,commit)==commit,'one producer change, different commit clock');active.add(z);degree[z]+=1;versions.add((*pk,z));phase=int(end>hi);d=end-commit;lags[phase].append(d);over[phase]+=d>100_000_000;n+=1;times=[commit,f['Eligible'],f['Selected'],f['Released'],pre[pk][0],fr[4],fr[5],fr[6],apply,locked,decoded,end];tags=['commit_to_first_observed_eligible','eligible_to_selected','selected_to_released','released_to_prepared','prepared_to_encode','encode','before_push','push_to_apply','apply_lock','client_decode','client_update'];parts=[y-x for x,y in zip(times,times[1:])];need(sum(parts)==d,'discontinuous time');
   for k,w in zip(tags,parts):
    all_ns[k]+=w
    if w<0:inversions[k]+=1
   if f['EligibleLane']!=1 or f['SelectedLane']!=1:foot['nonordinary_deliveries']+=1
 def quant(x,p):return x[(len(x)*p+99)//100-1] if x else None
 def bucket(w):return -1 if w is None or w>60_000_000_000 else max(0,(w+9999999)//10000000*10)
 for g,xs in zip(('ThroughWindowEnd','AfterWindowEnd','All'),lags+[lags[0]+lags[1]]):
  xs=sorted(xs);h=s['CommittedDeliveries'][g];need(h['Samples']==len(xs) and h['Over50MS']==sum(a>50_000_000 for a in xs) and h['Over60S']==sum(a>60_000_000_000 for a in xs) and all(bucket(quant(xs,p))==h['P'+str(p)+'LEMS'] for p in (50,95,99)),'stock histogram '+g)
 w=s['CommittedDeliveries']['All'];need(n==s['CommittedToClient']['Samples']>0 and s['LatencyPassed']==(0<=w['P99LEMS']<=limit and not any(s[k]['NegativeDurations'] for k in ('CommittedToClient','PlannedToClient','ChangedToClient'))),'stock verdict');need(all(z in plannedset for z in active),'non-formal source included');return dict(ok=not inversions,all_frames_and_changes_uniquely_matched=n,source_facts=len(fs),formal_source_facts=len(plannedset),represented_formal_facts=len(active),not_represented_in_formal_nonfull_deliveries=len(plannedset-active),subject_version_facts=len(versions),fanout_max=max(degree.values()),fanout_mean=n/len(active),noncausal=dict(inversions),exceptional_paths=dict(foot),sum_ns_by_stage=dict(all_ns),stock=s['CommittedDeliveries'],sync_limit_ms=limit,above_100ms_by_delivery_phase=over,all_above_100ms=sum(over),stock_go_verdict='PASS' if r['LatencyPassed'] else 'FAIL')
if __name__=='__main__':
 data=json.loads(gzip.decompress(Path(sys.argv[1]).read_bytes()));report=json.loads(Path(sys.argv[2]).read_bytes());print(json.dumps(check(data,report),indent=2))
