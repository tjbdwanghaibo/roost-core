#!/usr/bin/env python3
"""Generate a game consumer and run its actual Base, two Cells, account, TCP clients and private dependencies.
Requires Go, redis-server/cli, mongod/mongosh, nats-server, etcd. All sockets are loopback/ephemeral.
Movement, disconnect/reconnect, grace expiry and CellMove rejections use real TCP; read-only NATS writebacks
and Redis grants corroborate the client views. --grace-seconds shortens the product's 30 s grace for this drill.
Pass --out NEW_DIRECTORY to retain the consumer and per-process diagnostics. Does not use installed daemons.
"""
import argparse, contextlib, json, os, pathlib, re, shutil, signal, socket, subprocess as sp, tempfile, time, urllib.request

ROOT=pathlib.Path(__file__).resolve().parents[1]
parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('--out',type=pathlib.Path)
parser.add_argument('--grace-seconds', type=int, choices=[*range(8, 21), 30], metavar='8..20|30', default=8,
                    help='drill grace (default 8 s); 30 omits the key to exercise the product default')
args=parser.parse_args()
grace=f'{args.grace_seconds}s'
names=['go','redis-server','redis-cli','mongod','mongosh','nats-server','etcd']; missing=[x for x in names if not shutil.which(x)]
if missing: raise SystemExit(f'missing executables: {missing}')
if args.out: args.out.mkdir(parents=True,exist_ok=False)
work=args.out.resolve() if args.out else pathlib.Path(tempfile.mkdtemp(prefix='cellspace-'))
print('diagnostics:',work,flush=True); project=work/'explorer'; procs=[]; ports=set()
def port():
    while True:
        with socket.socket() as s:
            s.bind(('127.0.0.1',0)); n=s.getsockname()[1]
        if n not in ports: ports.add(n); return n
def run(*argv,cwd=None,env=None,timeout=120):
    return sp.run(list(map(str,argv)),cwd=cwd or work,env=env,text=True,stdout=sp.PIPE,stderr=sp.STDOUT,timeout=timeout,check=True).stdout
def start(name,*argv,cwd=None):
    with (work/(name+'.log')).open('ab') as out:
        p=sp.Popen(list(map(str,argv)),cwd=cwd or work,stdout=out,stderr=sp.STDOUT); procs.append((name,p));return p
def stop(p):
    if p.poll() is None:
        p.terminate()
        try:p.wait(timeout=20)
        except sp.TimeoutExpired:p.kill();p.wait()
def wait(fn,secs=30):
    end=time.monotonic()+secs;why=None
    while time.monotonic()<end:
        try:
            r=fn()
            if r:return r
        except (OSError,ValueError,sp.SubprocessError) as e:why=e
        time.sleep(.15)
    raise RuntimeError('condition timed out: '+repr(why))
def alive(portno):
    with socket.create_connection(('127.0.0.1',portno),timeout=.3):return True
def request(url,body=None):
    req=urllib.request.Request(url,data=None if body is None else json.dumps(body).encode(),headers={'Content-Type':'application/json'});return urllib.request.urlopen(req,timeout=3).read()
def main():
    run('go','run','./codegen/cmd/roost','project','new','explorer','-module','example.com/explorer','-template','game','-recipe','cellworld','-skip-deps','-out',project,cwd=ROOT)
    run('go','mod','edit','-replace',f'github.com/tjbdwanghaibo/roost-core={ROOT}',cwd=project)
    testdir=project/'internal/access/player/tcp'
    for src in sorted((ROOT/'scripts/testdata/cellspace').glob('*_test.go.txt')):
        (testdir/('cellspace_live_'+src.name.removesuffix('.txt'))).write_text(src.read_text().replace('MMO_MODULE','example.com/explorer'))
    for path,package in [('app','.'),('accountctl','./cmd/accountctl')]:run('go','build','-mod=mod','-o',work/path,package,cwd=project)
    run('go','test','-mod=mod','-c','-o',work/'probe','./internal/access/player/tcp',cwd=project)
    r,n,m,ec,ep=[port() for _ in range(5)]; addr=lambda p:f'127.0.0.1:{p}'; url=lambda p:'http://'+addr(p)
    (work/'mongo').mkdir(); start('redis','redis-server','--bind','127.0.0.1','--port',r,'--appendonly','yes','--appendfsync','always','--maxmemory-policy','noeviction','--dir',work)
    start('nats','nats-server','-a','127.0.0.1','-p',n,'-js','-sd',work/'js');start('mongo','mongod','--bind_ip','127.0.0.1','--port',m,'--dbpath',work/'mongo','--replSet','rs0')
    start('etcd','etcd','--name','partition','--data-dir',work/'etcd','--listen-client-urls',url(ec),'--advertise-client-urls',url(ec),'--listen-peer-urls',url(ep),'--initial-advertise-peer-urls',url(ep),'--initial-cluster','partition='+url(ep));wait(lambda:alive(m));run('mongosh','--quiet','--port',m,'--eval',f'rs.initiate({{_id:"rs0",members:[{{_id:0,host:"{addr(m)}"}}]}})');wait(lambda:'true' in run('mongosh','--quiet','--port',m,'--eval','db.hello().isWritablePrimary'))
    cp=project/'configs/service'; service={}; web=port(); tcp=port()
    def boot(kind,sid,enabled):
        ops,listen=port(),tcp if sid==1000 and kind=='game' else port();text=(cp/f'config.{kind}.yaml').read_text(); text=re.sub(r'^sid: \d+',f'sid: {sid}',text,flags=re.M);text=re.sub(r'^  addr: 127\.0\.0\.1:910[01]$',f'  addr: {addr(ops)}',text,flags=re.M)
        for old,new in [(6379,r),(4222,n),(27017,m),(2379,ec),(9000,port()),(8081,web),(7000,tcp)]:text=text.replace(addr(old),addr(new))
        text=text.replace('0.0.0.0:7000',addr(listen)).replace('8589934592','67108864').replace('session_secret: CHANGE_ME','session_secret: private-process-drill-only-secret');
        if kind=='game':
            text+=f'\ncellspace:\n  enabled: {str(enabled).lower()}\n'
            if args.grace_seconds!=30:text+=f'  reconnect_grace: {grace}\n'
        cfg=work/f'{kind}{sid}.yaml';cfg.write_text(text);p=start(kind+str(sid),work/'app',kind,'--sid',sid,'--config',cfg,cwd=project);wait(lambda: p.poll() is None and request(url(ops)+'/readyz'),45);return p
    boot('account',1000,False)
    for s in (1000,1001,1002):run(work/'accountctl','-redis',addr(r),'-prefix','roost:explorer:account','upsert-server','-sid',s,cwd=project)
    for s in (1001,1002):service[s]=boot('game',s,True)
    service[1000]=boot('game',1000,False)
    def keys():return run('redis-cli','-p',r,'--scan','--pattern','roost:cellworld:overworld:owner:*').strip().splitlines()
    def owner():
        ks=keys();assert len(ks)==1,ks;return json.loads(run('redis-cli','--raw','-p',r,'get',ks[0]).split('\n',1)[1])
    def client(mode, test='TestPartitionLiveClient', **settings):
        resp=json.loads(request(url(web)+'/portal/login',{'channel':'dev','open_id':'walker','credential':'dev-recipe-local','enter':{'server_id':1000}}))
        env=dict(os.environ,CELL_ADDR=addr(tcp),CELL_TICKET=resp['enter']['ticket'],CELL_ACTION=mode,**settings)
        probe=sp.run([work/'probe',f'-test.run=^{test}$','-test.v'],cwd=testdir,env=env,timeout=2*args.grace_seconds+35,text=True,stdout=sp.PIPE,stderr=sp.STDOUT)
        out=probe.stdout;(work/f'client-{mode}.log').write_text(out);print(out,flush=True);probe.check_returncode()
        if f'--- PASS: {test} ' not in out: raise RuntimeError(f'{test} did not run')
        match=re.search(r'identity (\d+) (\d+) (\d+)',out);return tuple(map(int,match.groups())) if match else None
    client('ordinary');assert not keys(),'ordinary-map login created a CellPlayer';stop(service[1000]);service[1000]=boot('game',1000,True);assert not keys(),'old document or start eagerly spawned an actor'
    ids=client('right');first=owner();assert first['actor']==ids[2] and first['grant']['process']['sid']==1002 and first['entry']==1,first
    # The online right-cell probe awaited durable writeback before disconnecting.
    stop(service[1000]);time.sleep(4); service[1000]=boot('game',1000,True);assert client('recover')==ids;rec=wait(lambda:(v if (v:=owner())['entry']>first['entry'] and v['grant']['process']['sid']==1002 else None),12)
    assert client('left')==ids;last=owner();assert last['grant']['process']['sid']==1001 and last['entry']==rec['entry'] and last['grant']['epoch']>rec['grant']['epoch'],last
    client('reconnect','TestPartitionReconnectLive',CELL_GRACE=grace,CELL_BASE=str(ids[1]),CELL_REDIS=addr(r),CELL_NATS='nats://'+addr(n),CELL_OWNER=json.dumps(last))
    fresh=owner();assert fresh['actor']==ids[2] and fresh['entry']==last['entry']+1 and fresh['closed_entry']==last['entry'],fresh
    print(f'PASS unique={ids[0]}, Base={ids[1]}, CellPlayer={ids[2]}; owner SID 1002 -> recovered 1002 -> 1001; entry {first["entry"]} -> {rec["entry"]} (restart) -> {last["entry"]} (reconnect) -> {fresh["entry"]} (offline past {grace})',flush=True)
try:
    main()
except sp.CalledProcessError as e:
    print(e.output,flush=True);raise
finally:
    for name,p in reversed(procs):stop(p)
    if not args.out: print('private diagnostics retained:',work)
