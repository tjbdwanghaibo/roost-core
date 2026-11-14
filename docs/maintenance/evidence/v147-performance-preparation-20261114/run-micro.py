import os,sys,pathlib,subprocess,time,json,datetime,signal,argparse,re
entry=pathlib.Path(__file__).resolve().parent
root=pathlib.Path(os.environ.get('ROOST_PERF_REPO',entry.parents[3])).resolve()
bins=pathlib.Path(os.environ.get('ROOST_PERF_BINS','/private/tmp/v147-perf-bin')).resolve()
parser=argparse.ArgumentParser()
parser.add_argument('segment')
parser.add_argument('--output-name')
parser.add_argument('--select',default='.*',help='fullmatch run labels; resume only unexecuted runs into a new output directory')
parser.add_argument('--plan',action='store_true',help='read-only command listing; no processes or output directory created')
cli=parser.parse_args()
segment=cli.segment
name=cli.output_name or segment
if not re.fullmatch(r'[A-Za-z0-9_-]+',name): parser.error('invalid output name')
selector=re.compile(cli.select)
out=pathlib.Path(os.environ.get('ROOST_PERF_OUTPUT_ROOT',root/'artifacts/perf/v147-remaining')).resolve()/name
env=dict(os.environ,ROOST_PERF_REPO=str(root),GOMAXPROCS='0',GOWORK='off',GOFLAGS='-p=2',ALLIANCE_E2E_ETCD_PORT='43379',ALLIANCE_E2E_PORT_BASE='43500',ROOST_SEAMLESS_ETCD_PORT='58379')
tasks=[]
def run(label,args,extra=None): tasks.append((label,args,extra or {}))
if segment=='s1-micro':
 run('m2-move',[str(bins/'movement.test'),'-test.run','^$','-test.bench','MoveBeatAllocations','-test.benchmem','-test.count','6'])
 run('m9-relation',[str(bins/'aoi-head.test'),'-test.run','^$','-test.bench','RelationViewAllianceFlush','-test.benchtime','50x','-test.count','3'])
 run('m18-group',[str(bins/'aoi-head.test'),'-test.run','^$','-test.bench','GroupViewFlush','-test.benchtime','200x','-test.count','3'])
 for n in range(1,11):
  for side in ['base','head','no-branches']:
   run(f'e1-{n:02d}-{side}',[str(bins/f'aoi-{side}.test'),'-test.run','^$','-test.bench','AOIBatchRecompute/dense=false/coverage=false/workers=(1|4)$','-test.count','1','-test.benchmem'])
elif segment=='s2-heartbeat':
 for n in (1,2):
  for cb in (0,5,10,20):
   for hot in (0,64):
    label=f'm1-r{n}-cb{cb}-hot{hot}'
    run(label,[str(bins/'hb-g4-s100.test'),'-test.run','^TestDeliveryRun$','-test.v','-test.timeout','60s'],{'HB_DELIVERY':'1','HB_MODE':'c','HB_CB_US':str(cb),'HB_HOT':str(hot),'HB_HOT_SHARE':'0' if hot==0 else '0.5','HB_OUT':str(out/'delivery.jsonl'),'HB_LABEL':label})
 for n in (1,2):
  for group,slice_us in ((2,50),(4,100),(2,100),(4,50)):
   label=f'm1-r{n}-g{group}-s{slice_us}'
   run(label,[str(bins/f'hb-g{group}-s{slice_us}.test'),'-test.run','^TestDeliveryRun$','-test.v','-test.timeout','60s'],{'HB_DELIVERY':'1','HB_MODE':'c','HB_CB_US':'10','HB_HOT':'0','HB_HOT_SHARE':'0','HB_OUT':str(out/'variants.jsonl'),'HB_LABEL':label})
elif segment=='s3-sync':
 for n,mode in enumerate(('periodic','on_change','periodic','on_change'),1):
  run(f'm4-{n}-{mode}',['bash','scripts/perf/sync-business.sh'],{'ROOST_PERF_LABEL':f'v147-m4-{n}-{mode}','ROOST_PERF_COUNT':'1','ROOST_PERF_CPU':'0','ROOST_SYNC_SCHEDULE':mode})
 for n,mode in enumerate(('sync','async','sync','async'),1):
  run(f'm8-{n}-{mode}',[str(bins/'sync-aoi-views16'),'-players=1000','-entities=10000','-hz=20','-dirty=1','-visible=50','-views-per-player=16',f'-apply-mode={mode}',f'-output={out}/m8-{n}-{mode}'])
 for n,scale in enumerate(('demo','game','demo','game'),1):
  run(f'm14-{n}-{scale}',[str(bins/'cellworld.test'),'-test.run','^TestCellWorldMeasure$','-test.count','1','-test.timeout','5m','-test.v'],{'ROOST_CELLWORLD_PERF':'1','ROOST_CELLWORLD_PERF_SCALE':scale,'ROOST_CELLWORLD_PERF_COUNT':'1000','ROOST_CELLWORLD_PERF_NPCS':'2250','ROOST_CELLWORLD_PERF_ACTIVE':'5','ROOST_CELLWORLD_PERF_SECONDS':'60'})
elif segment=='s4-nest-game':
 for n,delivery in enumerate(('batch','dispatch','batch','dispatch'),1):
  run(f'm3-{n}-{delivery}',['bash','scripts/perf/nest-game.sh'],{'ROOST_PERF_LABEL':f'v147-m3-{n}-{delivery}','ROOST_PERF_COUNT':'1','ROOST_PERF_CPU':'0','ROOST_NEST_GAME_HEARTBEAT_DRIVER':'heartbeat','ROOST_NEST_GAME_HEARTBEAT_DELIVERY':delivery,'ROOST_NEST_GAME_DIRTY':'5','ROOST_NEST_GAME_DURATION':'5m','ROOST_NEST_GAME_ENTITIES':'10000','ROOST_NEST_GAME_EMBEDDED':'1','ROOST_NEST_GAME_HZ':'10','ROOST_NEST_GAME_IO_WORKERS':'8','ROOST_NEST_GAME_MESSAGES_PER_PLAYER':'10','ROOST_NEST_GAME_PLAYERS':'1000','ROOST_NEST_GAME_SHORT_QUEUE':'65536','ROOST_NEST_GAME_SHORT_WORKERS':'8','ROOST_NEST_GAME_SYNC':'1'})
elif segment=='s5-gate-budget':
 run('m6-gate-selfsync',['bash','scripts/perf/nest-game.sh'],{'ROOST_PERF_LABEL':'v147-m6-selfsync','ROOST_PERF_COUNT':'1','ROOST_PERF_CPU':'0','ROOST_NEST_GAME_GATE':'1','ROOST_NEST_GAME_SELF_SYNC':'1','ROOST_NEST_GAME_PLAYERS':'1000','ROOST_NEST_GAME_ENTITIES':'10000','ROOST_NEST_GAME_MESSAGES_PER_PLAYER':'10','ROOST_NEST_GAME_SHORT_WORKERS':'8','ROOST_NEST_GAME_IO_WORKERS':'8','ROOST_NEST_GAME_SHORT_QUEUE':'65536','ROOST_NEST_GAME_DURATION':'15m'})
 for rate in (20,100):
  run(f'm7-budget-rate{rate}',[str(bins/'gateway.test'),'-test.run','^TestOutboundBudgetDrill$','-test.count','1','-test.timeout','10m','-test.v'],{'ROOST_TCP_BUDGET_DRILL':'1','DRILL_HEALTHY':'1000','DRILL_STALLED':'50','DRILL_FRAME_BYTES':'4096','DRILL_RATE':str(rate),'DRILL_SECONDS':'120'})
elif segment=='s6-catchup-mail':
 run('m10-catchup',[str(bins/'gate.test'),'-test.run','^TestRealGateLockstepCatchupOverASlowLinkDrill$','-test.count','1','-test.timeout','15m','-test.v'],{'LS_DRILL_FRAMES':'3000','LS_DRILL_RATES':'256,64'})
 for kind,addr in [('standalone',os.environ.get('ROOST_DATAENGINE_IT_REDIS_ADDR','<required-private-redis>')),('cluster',os.environ.get('ROOST_MIRROR_LOCAL_REDIS_CLUSTER','<required-private-redis-cluster>'))]:
  run('m13-mail-'+kind,[str(bins/'mail.test'),'-test.run','^$','-test.bench','BroadcastLedgerAtCapacity','-test.benchtime','2000x','-test.count','3','-test.benchmem'],{'REDIS_ADDR':addr})
elif segment=='s7-block-aoi':
 for blocks in (0,4):
  run(f'm16-blocks{blocks}',['bash','scripts/perf/nest-game.sh'],{'ROOST_PERF_LABEL':f'v147-m16-blocks{blocks}','ROOST_PERF_COUNT':'1','ROOST_PERF_CPU':'0','ROOST_NEST_GAME_GATE':'1','ROOST_NEST_GAME_SYNC':'1','ROOST_NEST_GAME_LAYOUT':'concentrated','ROOST_NEST_GAME_OBSERVED_BLOCKS':str(blocks),'ROOST_NEST_GAME_SHORT_WORKERS':'8','ROOST_NEST_GAME_IO_WORKERS':'8','ROOST_NEST_GAME_SHORT_QUEUE':'65536','ROOST_NEST_GAME_PLAYERS':'1000','ROOST_NEST_GAME_ENTITIES':'10000','ROOST_NEST_GAME_MESSAGES_PER_PLAYER':'10','ROOST_NEST_GAME_HZ':'1','ROOST_NEST_GAME_DIRTY':'1','ROOST_NEST_GAME_DURATION':'10m'})
elif segment=='s8-alliance-effects':
 for round in (1,2,3):
  for workers in (16,1):
   label=f'm11-r{round}-w{workers}'
   run(label,['bash','scripts/test-alliance-e2e.sh'],{'ALLIANCE_E2E_PERF':'1','ALLIANCE_E2E_RETRY':'1','ALLIANCE_E2E_EFFECT_WORKERS':str(workers),'ALLIANCE_E2E_RUN':'^TestAllianceE2EJoinRetryableSources$','ALLIANCE_E2E_LOGS':str(out/label)})
elif segment=='b2-footprints':
 run('b2-business',['bash','scripts/test-alliance-e2e.sh'],{'ALLIANCE_E2E_PERF':'1','ALLIANCE_E2E_MEMBERS':'500','ALLIANCE_E2E_MIRROR_COPIES':'16','ALLIANCE_E2E_RUN':'^TestAllianceE2ELargeAllianceFootprint$','ALLIANCE_E2E_LOGS':str(out/'business')})
 run('b2-synthetic',['bash','scripts/test-alliance-e2e.sh'],{'ALLIANCE_E2E_PERF':'1','ALLIANCE_E2E_SHAPE_COUNTS':'1000,5000,10000','ALLIANCE_E2E_MIRROR_COPIES':'16','ALLIANCE_E2E_RUN':'^TestAllianceE2ESyntheticRosterFootprint$','ALLIANCE_E2E_LOGS':str(out/'synthetic')})
elif segment=='s11-cellworld-robot':
 run('m15-cellworld',['bash','scripts/test-seamless-demo.sh'],{'ROOST_SEAMLESS_OUT':str(out/'robot'),'ROOST_SEAMLESS_ROBOT_ONLY':'1','ROOST_SEAMLESS_CAPACITY':'1','ROOST_SEAMLESS_ROBOTS':'1000','ROOST_SEAMLESS_ROBOT_SECONDS':'60','ROOST_SEAMLESS_ROBOT_ACTIVE':'5'})
elif segment=='s12-gate-pacing':
 for n in range(1,6):
  run(f'e3-gate-{n}',['bash','scripts/perf/gate-mixed.sh'],{'ROOST_PERF_CPU':'0','ROOST_PERF_LABEL':f'v147-e3-{n}','ROOST_GATE_LOAD_RATE':'100','ROOST_GATE_LOAD_SECONDS':'30'})
elif segment=='s13-generated-tcp':
 for label,dur,sync,mode,rate in [('strict','strict','0','distinct','0'),('strict-sync','strict','1','distinct','0'),('pipelined-open1500','pipelined','0','distinct','1500'),('pipelined-shared','pipelined','0','shared','0')]:
  run('m17-'+label,['bash','scripts/perf/generated-tcp.sh'],{'ROOST_GENERATED_TCP_OUTPUT':str(out/label),'ROOST_GENERATED_TCP_SECONDS':'300','DURABILITY':dur,'SELF_SYNC':sync,'ENTITY_MODE':mode,'CONNECTIONS':'50','OPEN_LOOP_RATE':rate})
elif segment in ('s9-demo100','s10-demo200'):
 count='100' if segment=='s9-demo100' else '200'
 for mode in ('burst','ramp20','ramp10'):
  run(f'm12-{count}-{mode}',['python3',str(entry/'demo-robot-retained.py')],{'DEMO_ROBOT_OUT':str(out/mode),'DEMO_ROBOT_COUNT':count,'DEMO_ROBOT_MODE':mode,'DEMO_ROBOT_MIRROR_OFFSET':'36600'})
elif segment in ('s14-checkpoint128','s15-checkpoint256'):
 concurrency='128' if segment=='s14-checkpoint128' else '256'
 experiment=os.environ.get('ROOST_CHECKPOINT_TREE',str(root.parent/'v147-background-checkpoint'))
 for round in (1,2,3):
  for enabled in ('0','1'):
   label=f'm19-c{concurrency}-r{round}-bg{enabled}'
   run(label,['bash',experiment+'/scripts/test-alliance-e2e.sh'],{'ALLIANCE_E2E_PERF':'1','ALLIANCE_E2E_BACKGROUND_CHECKPOINT':enabled,'ALLIANCE_E2E_STACKS':'1','ALLIANCE_E2E_RPC_CONCURRENCY':concurrency,'ALLIANCE_E2E_RUN':'^TestAllianceE2ERPCConcurrency$','ALLIANCE_E2E_LOGS':str(out/label)})
else: raise ValueError(segment)

selected=[]
for label,args,extra in tasks:
 if selector.fullmatch(label):
  extra=dict(extra)
  if 'ROOST_PERF_LABEL' in extra: extra['ROOST_PERF_LABEL']+='-'+name
  selected.append((label,args,extra))
if not selected: parser.error('selection matches no runs')
if cli.plan:
 print(json.dumps({'segment':segment,'output':str(out),'selected':[{'label':a,'args':b,'env':c} for a,b,c in selected]},indent=2))
 sys.exit(0)
out.mkdir(parents=True,exist_ok=False)
start=float(os.environ.get('ROOST_SEGMENT_START_MONO',time.monotonic()))
deadline=float(os.environ.get('ROOST_SEGMENT_DEADLINE_MONO',start+1800))
reserve=120
metadata={'started_utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'GOMAXPROCS_env':'0','GOFLAGS':'-p=2','segment_limit_seconds':1800,'shutdown_reserve_seconds':reserve,'output_name':name,'selection':cli.select,'shared_deadline_monotonic':deadline}
for key,cmd in [('git_head',['git','rev-parse','HEAD']),('git_status',['git','status','--short']),('toolchain',['go','version']),('cpu',['sysctl','hw.logicalcpu']),('battery',['pmset','-g','batt']),('load',['sysctl','-n','vm.loadavg']),('swap',['sysctl','vm.swapusage'])]:
 metadata[key]=subprocess.check_output(cmd,cwd=root,text=True).strip()
(out/'environment.json').write_text(json.dumps(metadata,indent=2)+'\n')
(out/'selection.json').write_text(json.dumps({'all_labels':[a for a,_,_ in tasks],'selected_labels':[a for a,_,_ in selected]},indent=2)+'\n')
with (out/'processes-before.txt').open('w') as f: subprocess.run(['ps','-axo','pid,ppid,pcpu,comm'],stdout=f,check=True)
results=[]
attempted=[]
def interrupt(signum,frame): raise KeyboardInterrupt()
signal.signal(signal.SIGTERM,interrupt)
def execute(label,args,extra):
 remaining=deadline-time.monotonic()-reserve
 if remaining<=0: raise TimeoutError('segment budget exhausted before run; no workload started')
 attempted.append(label)
 extra=dict(extra)
 meta={'label':label,'utc':datetime.datetime.now(datetime.timezone.utc).isoformat(),'args':args,'env':extra,'cpu_env':'0'}
 for key,cmd in [('load',['sysctl','-n','vm.loadavg']),('swap',['sysctl','vm.swapusage']),('battery',['pmset','-g','batt'])]:
  meta[key]=subprocess.check_output(cmd,text=True).strip()
 begin=time.monotonic();process=None
 with (out/(label+'.log')).open('w') as log:
  try:
   process=subprocess.Popen(args,cwd=root,env=env|extra,stdout=log,stderr=subprocess.STDOUT,start_new_session=True)
   (out/'active.json').write_text(json.dumps({'label':label,'pid':process.pid,'supervisor':os.getpid(),'args':args}))
   meta['exit']=process.wait(timeout=remaining)
  except (subprocess.TimeoutExpired,KeyboardInterrupt) as stopped:
   if process is not None:
    try: os.killpg(process.pid,signal.SIGTERM)
    except ProcessLookupError: pass
    try: process.wait(timeout=10)
    except subprocess.TimeoutExpired:
     try: os.killpg(process.pid,signal.SIGKILL)
     except ProcessLookupError: pass
     process.wait()
   meta['exit']=124 if isinstance(stopped,subprocess.TimeoutExpired) else 130
  except OSError as exc:
   meta['exit']=126;meta['launch_error']=str(exc)
 meta['seconds']=time.monotonic()-begin
 meta['ended_utc']=datetime.datetime.now(datetime.timezone.utc).isoformat()
 for key,cmd in [('load_after',['sysctl','-n','vm.loadavg']),('swap_after',['sysctl','vm.swapusage']),('battery_after',['pmset','-g','batt'])]:
  meta[key]=subprocess.check_output(cmd,text=True).strip()
 results.append(meta)
 (out/'runs.json').write_text(json.dumps(results,indent=2)+'\n')
 (out/'active.json').unlink(missing_ok=True)
 print(label,meta['exit'],round(meta['seconds'],2),flush=True)
 if meta['exit']!=0: raise SystemExit(meta['exit'] if 0<meta['exit']<256 else 1)
status=0
try:
 for task in selected: execute(*task)
except TimeoutError as exc:
 status=124;(out/'timeout.txt').write_text(str(exc)+'\n')
except KeyboardInterrupt: status=130
except SystemExit as exc: status=int(exc.code)
except Exception as exc:
 status=1;(out/'runner-error.txt').write_text(type(exc).__name__+': '+str(exc)+'\n')
finally:
 done=[r['label'] for r in results]
 summary={'exit':status,'attempted_labels':attempted,'finished_labels':done,'succeeded_labels':[r['label'] for r in results if r['exit']==0],'failed_labels':[r['label'] for r in results if r['exit']!=0],'unfinished_labels':[a for a in attempted if a not in done],'unstarted_labels':[a for a,_,_ in selected if a not in attempted],'elapsed_seconds_before_dependency_down':time.monotonic()-start,'note':'Finished means an exit was recorded, including failure/timeout; succeeded means command exit 0, not capacity acceptance. Inspect failed and unfinished labels before any resumption.'}
 (out/'segment-result.json').write_text(json.dumps(summary,indent=2)+'\n')
 with (out/'processes-after.txt').open('w') as f: subprocess.run(['ps','-axo','pid,ppid,pcpu,comm'],stdout=f)
print(json.dumps(summary),flush=True)
sys.exit(status)
