"""Offline check, executable without Go, Git, network or a product checkout. Never change the stock verdict."""
import argparse, collections, gzip, hashlib, json, pathlib, re, sys, zipfile
P=pathlib.Path
META=dict(branches=['session_miss','lifetime_mismatch','held','work_conflict','new_work','same_work'],kinds=['snapshot','live','leaving'],results=['no_prepare','not_dirty','closed','commit_pending','other_error','durability','prepared'],fanout=['0','1','2..8','9..32','33..64','65+'])
def ok(cond,msg):
 if not cond:raise ValueError(msg)
def add(a,b):
 if isinstance(b,int):return (a or 0)+b
 if isinstance(b,list):return [add(None if a is None else a[i],v) for i,v in enumerate(b)]
 return {k:add(None if a is None else a[k],v) for k,v in b.items()}
def dense(d):
 v,e,b,k,c=d['Visits'],d['Edges'],d['Branch'],d['Kind'],d['DistinctClass'];ok(sum(b)==e==sum(k) and sum(c)==v and 65*v<=e,'dense denominator');ok(c[0]==d['AllSameWork'] and c[1]==d['CleanNew'] and c[1]+c[3]==d['AnyNew'] and c[2]+c[3]==d['AnyException'],'whole-visit joint classification');ok(d['LeavingVisits']<=min(v,k[2]) and bool(d['LeavingVisits'])==bool(k[2]) and d['AnyException']<=sum(b[:4]) and bool(d['AnyException'])==bool(sum(b[:4])) and d['AnyNew']<=b[4] and bool(d['AnyNew'])==bool(b[4]),'dense support');ok(all(x>=0 for x in b+k+c),'negative cell')
def analyze(O):
 get=lambda n:json.loads((O/n).read_bytes());r=get('report.json');p=get('params.json');rc=get('original-exit.json');S=r['SyncReport'];cfg=r['Config'];tiny=p['mode']!='c4';players={'c4':1000,'small':8,'small64':64}[p['mode']];sec=2 if tiny else 30;entities={'c4':10000,'small':64,'small64':512}[p['mode']];stand={'Players':players,'MessageTargets':players,'Entities':entities,'MessagesPerPlayer':10,'HeartbeatHz':1,'DirtyPercent':1,'Workers':8,'IOWorkers':8,'Queue':65536,'Warmup':sec*10**9,'Duration':sec*10**9,'Settle':60*10**9,'Frontend':'gate','SceneSync':True,'SelfSync':False,'ObservedBlocks':4,'Layout':'concentrated','HeartbeatDriver':'ticker'};ok(all(cfg.get(k)==v for k,v in stand.items()),'upstream load config mismatch');ok(r['GateEnabled'] and S['Mode']=='on_change' and S['CommittedP99LimitMS']==(50 if tiny else 100),'stock gate');log=(O/'run.log').read_text();v=re.findall(r'^--- (PASS|FAIL): TestNestGameLoad\b',log,re.M);ok(len(v)==1 and rc==(0 if v[0]=='PASS' else 1),'original verdict log/exit');expected=r['CountsVerified'] and r['EntityValuesVerified'] and r['LatencyPassed'] and not r.get('ClientFirstFailure');ok(expected==(rc==0),'fixture flags vs exit');split=S['CommittedDeliveries'];a=split['All'];ok(all(a[k]==sum(split[z][k] for z in ['ThroughWindowEnd','AfterWindowEnd']) for k in ['Samples','Over50MS','Over60S']) and a['Samples']==S['CommittedToClient']['Samples'],'client cuts');syncpass=a['Samples']>0 and all(S[z]['NegativeDurations']==0 for z in ['PlannedToClient','ChangedToClient','CommittedToClient']) and ((tiny and S['CommittedToClient']['P99MS']<=50) or (not tiny and 0<=a['P99LEMS']<=100));ok(syncpass==S['LatencyPassed'],'sync latency verdict');ok(r['Messages']['Planned']==players*10*2*sec and r['Heartbeats']['Planned']==entities*2*sec,'planned volume');
 src=get('source.json');provenance=get('build.json');ok(src['head']=='24b704bf1627d4db3f64c4f87880bfce808af326' and src['main']=='58f47cbe3e5e486d919e6a843252846e2e14355d','source pin');ok(set(provenance['overlay'])=={'flush.go','manager.go','watcher_review.go','load_test.go'},'unexpected observer overlay');roles=p['ports'];ok(len(roles)==6==len(set(roles)) and all(('nats://127.0.0.1:'+str(port)) in log for port in roles[:3]),'private role endpoints');ok(re.search(r'player tcp listener started addr=127\.0\.0\.1:\d+',log),'not private Gate');
 if src.get('preload'):
  con=re.findall(r'WATCHER_PRELOAD_BOUND players=(\d+) checks=(\d+) elapsed_ns=(\d+)',log);ok(len(con)==1 and int(con[0][0])==players and int(con[0][1])>=1,'preload all identities not confirmed');iso=[json.loads(z) for z in (O/'isolation.jsonl').read_text().splitlines()];ok(all(not z['foreign'] and z['visible']>2 for z in iso) and {'before-build','before-load','after-load'}<=set(z['stage'] for z in iso),'isolation');
 if (O/'SHA256.json').is_file():
  for n,h in get('SHA256.json').items():ok(hashlib.sha256((O/n).read_bytes()).hexdigest()==h,'archive hash: '+n)
 with gzip.open(O/'trace.json.gz','rt') as f:t=json.load(f)
 rows=t['Rows'];s,f,e=t['Start'],t['From'],t['End'];ok(t['Schema']==1 and f-s==e-f==sec*10**9,'window');ok(len(rows)==t['Counters']['FlushCalls'] and rows,'observation coverage');cnt=[0]*4;last_end=None;groups={};phase_n=collections.Counter();previous=None;entire=None;pair=None;dense_rows=[]
 for i,row in enumerate(rows,1):
  x,y=row['Start'],row['End'];ok(row['Seq']==i and x<=y and (last_end is None or last_end<=x),'nonadjacent or overlapping flush serials');last_end=y;ordinary=not row['Force'] and not row['Failed'];phase='straddle'
  for name,lo,hi in [('setup',None,s),('warmup',s,f),('formal',f,e),('after',e,None)]:
   if (lo is None or x>=lo) and (hi is None or y<=hi):phase=name;break
  key=phase+('/regular-ok' if ordinary else '/forced-or-failed');phase_n[key]+=1;good=(phase=='formal' and ordinary)
  for j,q in enumerate(row['By']):
   n=q['Candidates'];ok(n==sum(q['Branch'])==sum(z[1] for z in q['Fanout']) and sum(z[0] for z in q['Fanout'])==q['Visits'],f'visit denominator {i}:{j}');ok(all(sum(k)==b and ov<=k[0] for k,b,ov in zip(q['Kinds'],q['Branch'],q['Over'])),f'branch kinds {i}:{j}');ok(q['RepeatCandidates']<=n and q['Repeated']<=q['Visits'] and q['ConsecutivePending']<=q['Repeated'] and q['ConsecutivePendingCandidates']<=q['RepeatCandidates'],'repeat denominator');ok((j==3 or not q['ConsecutivePending']),'pending in wrong result');
  groups[key]=add(groups.get(key),row['By']);d=row['DensePending'];both=row['BothDensePending'];dense(d);dense(both);ok(both['Edges']<=d['Edges'] and both['Visits']<=d['Visits']<=row['By'][3]['ConsecutivePending'] and d['Edges']<=row['By'][3]['ConsecutivePendingCandidates'],'dense subset');entire=add(entire,d)
  if good and previous:pair=add(pair,d);dense_rows.append(i)
  previous=good;ok(row['EmptyFull']<=row['Full']<=row['Frames'] and (row['Bytes']>0)==(row['Frames']>0),'successful wire frame subset');cnt=[n+row[k] for n,k in zip(cnt,['Frames','Creates','Updates','Removes'])];ok(cnt==row['Counter'],f'admitted cumulative contract at seq={i}')
 ok(cnt==[t['Counters'][k+'Admitted'] for k in ['Frames','Creates','Updates','Removes']],'frozen producer prefix');ok(sum(x['Failed'] for x in rows)==t['Counters']['FlushFailures'],'failed round coverage');den=pair['Visits'] if pair else 0
 return dict(schema=1,legend=META,original_exit=rc,fixture_verdict=v[0],stock_config=cfg,GOMAXPROCS=r['GOMAXPROCS'],counts_pass=r['CountsVerified'],values_pass=r['EntityValuesVerified'],sync_values_pass=S['ValuesVerified'],client_failure=r.get('ClientFirstFailure'),sync_latency_pass=syncpass,sync_limit_ms=S['CommittedP99LimitMS'],committed_deliveries=split,settle_ms=S['SettleMS'],gate_completed=r['GateCompleted'],expected_gate_completions=players*10*2*sec,gate_roundtrip=r['GateRoundTrip'],strict_final_visible=[S['VisibleMin'],S['VisibleMax']],phase_rounds=phase_n,complete_formal_adjacent_pairs=len(dense_rows),intersection_all_rounds=entire,intersection_complete_formal_adjacent_regular=pair,intersection_note='no qualifying samples' if den==0 else 'observed; NOT permission to skip Prepare',admitted=dict(zip(['frames','creates','updates','removes'],cnt)),client_received_at_report=S['Frames'],admitted_minus_client_at_report=cnt[0]-S['Frames'],by_phase_result=groups)
def archive(O,z):
 files=['isolation.jsonl','report.json','trace.json.gz','original-exit.json','run.log','source.json','inputs.json','build.json','params.json','build.log','checked.json','observed/observation.patch','nats-0.log','nats-1.log','nats-2.log'];D=O/'replay';ok(D.is_dir(),'archive requires frozen replay snapshot')
 with zipfile.ZipFile(z,'x',zipfile.ZIP_DEFLATED) as arc:
  hashes={}
  for n in files:
   b=(O/n).read_bytes();arc.writestr(n,b);hashes[n]=hashlib.sha256(b).hexdigest()
  for n in ('verify.py','runner.py','probe.go','observed_test.go','common_test.go','README.md'):
   b=(D/n).read_bytes();arc.writestr('replay/'+n,b);hashes['replay/'+n]=hashlib.sha256(b).hexdigest()
  arc.writestr('SHA256.json',json.dumps(hashes,sort_keys=True,indent=2))
if __name__=='__main__':
 ap=argparse.ArgumentParser();ap.add_argument('dir',type=P);ap.add_argument('--archive',type=P);a=ap.parse_args();out=analyze(a.dir);p=a.dir/'checked.json';
 if p.exists():ok(json.loads(p.read_bytes())==out,'checked result changed')
 else:p.write_text(json.dumps(out,indent=2)+'\n')
 if a.archive:archive(a.dir,a.archive)
 print('observer reconciliation OK; original',out['fixture_verdict'],'exit',out['original_exit'],'accepted',out['admitted'],'formal adjacent 65+ pending',out['intersection_complete_formal_adjacent_regular'])
