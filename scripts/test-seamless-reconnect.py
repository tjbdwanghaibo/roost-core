#!/usr/bin/env python3
"""Run the full generated game-demo's two-player TCP reconnect drill. Requires Go and local
mongod/mongosh, Redis, NATS JetStream and etcd executables. No shared instances, external
endpoints or prebuilt demo. --out must be new; keeps generator, consumer, logs and timeline.
--grace-seconds 30 omits the override, exercising the actual default.
"""
import argparse
import hashlib
import json
import os
from pathlib import Path
import re
import secrets
import shutil
import signal
import socket
import subprocess as sp
import time
import urllib.request

ROOT = Path(__file__).resolve().parents[1]
SERVICES = ('account', 'activity', 'chat', 'global', 'mail', 'match', 'platform', 'rank', 'session', 'alliance')
P = argparse.ArgumentParser(description=__doc__)
P.add_argument('--out', required=True, type=Path)
P.add_argument('--grace-seconds', type=int, choices=(8, 12, 30), default=8)
args = P.parse_args()
out = args.out.resolve()
for cmd in ('go', 'mongod', 'mongosh', 'redis-server', 'redis-cli', 'nats-server', 'etcd'):
    if not shutil.which(cmd):
        raise SystemExit(f'missing {cmd}')
out.mkdir(mode=0o700, parents=True, exist_ok=False)
demo = out / 'cwdemo'
env = dict(os.environ, GOWORK='off', GOPROXY='off')
# This run builds the CLI from the checked-out source; nested generation tools must use it too.
env['PATH'] = str(out) + os.pathsep + env['PATH']
processes = []
listeners = {}
world = 'reconnect-' + secrets.token_hex(5)

def run(name, argv, cwd=None, timeout=300):
    with (out / (name + '.log')).open('wb') as f:
        result = sp.run(list(map(str, argv)), cwd=cwd or out, env=env, stdout=f, stderr=sp.STDOUT, timeout=timeout)
    if result.returncode:
        raise RuntimeError(f'{name} exited {result.returncode}: ' + (out/(name+'.log')).read_text(errors='replace')[-2400:])
    return (out / (name + '.log')).read_text(errors='replace')

def start(name, argv, cwd=None):
    with (out / (name + '.log')).open('wb') as f:
        p = sp.Popen(list(map(str, argv)), cwd=cwd or out, env=env, stdout=f, stderr=sp.STDOUT)
    processes.append((name,p)); return p

def wait(fn, seconds=40):
    end = time.monotonic() + seconds
    while time.monotonic() < end:
        for name,p in processes:
            if p.poll() is not None:
                raise RuntimeError(f'{name} exited {p.returncode}: ' + (out/(name+'.log')).read_text(errors='replace')[-2400:])
        try:
            if fn(): return
        except (OSError, ValueError): pass
        time.sleep(.12)
    raise RuntimeError(f'timed out waiting for {fn}')

def port(key):
    if key not in listeners:
        while True:
            with socket.socket() as s:
                s.bind(('127.0.0.1', 0)); number=s.getsockname()[1]
            if number not in listeners.values(): break
        listeners[key] = number
    return listeners[key]

def addr(key): return f'127.0.0.1:{port(key)}'
def http(key): return 'http://' + addr(key)
def listening(key):
    with socket.create_connection(('127.0.0.1', port(key)), timeout=.4): return True

def main():
    print(f'generating game-demo from {ROOT} into {demo}', flush=True)
    run('git-head', ['git','rev-parse','HEAD'], ROOT)
    diff = sp.check_output(['git','diff','HEAD','--','demo','modules','gameplay','codegen'],cwd=ROOT)
    (out/'source-diff-sha256.txt').write_text(hashlib.sha256(diff).hexdigest()+'\n')
    run('build-roost',['go','build','-o',out/'roost','./codegen/cmd/roost'], ROOT)
    run('generate',[out/'roost','project','new','cwdemo','-module','example.com/cwdemo','-template','game-demo','--skip-deps','-out',demo], ROOT)
    run('replace',['go','mod','edit','-replace',f'github.com/tjbdwanghaibo/roost-core={ROOT}'],demo)
    check=(demo/'game/settings/partition.go').read_text()
    if not re.search(r'config:"reconnect_grace"[^`]*default:"30s"',check):
        raise RuntimeError('generated demo does not declare scene.partition.reconnect_grace with default 30s')
    probe=demo/'cmd'/'reconnectprobe';probe.mkdir()
    for name in ('drill','terrain','reconnect'):
        source=(ROOT/'scripts/testdata/seamless'/(name+'.go.txt')).read_text().replace('MMO_MODULE','example.com/cwdemo')
        (probe/(name+'.go')).write_text(source)
    # Hash the actual generated sources and probe, not just tracked template changes.
    files=sorted(demo.rglob('*.go'))
    (out/'generated-sha256.txt').write_text(''.join(f'{hashlib.sha256(f.read_bytes()).hexdigest()}  {f.relative_to(demo)}\n' for f in files))
    run('build-app',['go','build','-mod=mod','-o',out/'app','.'],demo)
    run('build-accountctl',['go','build','-mod=mod','-o',out/'accountctl','./cmd/accountctl'],demo)
    run('build-probe',['go','build','-mod=mod','-o',out/'probe','./cmd/reconnectprobe'],demo)
    print('built; starting private loopback data services',flush=True)
    (out/'mongo').mkdir()
    start('mongo',['mongod','--bind_ip','127.0.0.1','--port',port(27017),'--dbpath',out/'mongo','--replSet','rs0','--oplogSize','64','--setParameter','transactionLifetimeLimitSeconds=20'])
    start('redis',['redis-server','--bind','127.0.0.1','--port',port(6379),'--dir',out,'--appendonly','yes','--appendfsync','always','--maxmemory-policy','noeviction'])
    start('nats',['nats-server','-a','127.0.0.1','-p',port(4222),'-js','-sd',out/'jetstream'])
    start('etcd',['etcd','--name','reconnect','--data-dir',out/'etcd','--listen-client-urls',http(2379),'--advertise-client-urls',http(2379),'--listen-peer-urls',http(2380),'--initial-advertise-peer-urls',http(2380),'--initial-cluster','reconnect='+http(2380)])
    wait(lambda:all(listening(k) for k in (27017,6379,4222,2379)))
    run('mongo-init',['mongosh','--quiet','--port',port(27017),'--eval','rs.initiate({_id:"rs0", members:[{_id:0,host:"'+addr(27017)+'"}]})'])
    wait(lambda:'true' in run('mongo-primary',['mongosh','--quiet','--port',port(27017),'--eval','db.hello().isWritablePrimary']))
    cfgdir=demo/'configs/service'; secret=secrets.token_hex(32)
    scene=f'''\nscene:
  partition:
    enabled: true
    world: {world}
    cell_size: 3000
    cols: 2
    rows: 1
    owners: ["1001", "1002"]
    ghost_band: 500
    hysteresis: 20
    view_radius: 400
    idle_period: 0s
    writeback_every: 500ms
    lease_timeout: 3s
    max_speed: 300
    attack_damage: 10
    spawn_x: 2970
    spawn_y: 1500
    player_hp: 100
    spawns:
      witness_walker: {{x: 2895, y: 1520, count: 1, vx: 12}}
'''
    if args.grace_seconds!=30:scene+=f'    reconnect_grace: {args.grace_seconds}s\n'
    def boot(kind, sid, baseops):
        name=kind if kind!='game' else f'game-{sid}'
        cfg=(cfgdir/f'config.{kind}.yaml').read_text()
        # Translate every active loopback/default public listen address, never comments or prod examples.
        # All TCP/ops binds (including generated public player binds) are now local only.
        def endpoint(m):
            key=int(m.group(1))
            if kind=='game' and sid!=1000 and key in (7000,9000,9100):key=(key,sid)
            return addr(key)
        cfg=''.join(line if line.lstrip().startswith('#') else re.sub(r'\b(?:127\.0\.0\.1|0\.0\.0\.0):(\d+)\b',endpoint,line) for line in cfg.splitlines(keepends=True))
        cfg=re.sub(r'^sid: \d+',f'sid: {sid}',cfg,flags=re.M)
        cfg=cfg.replace('max_bytes: 8589934592','max_bytes: 67108864').replace('session_secret: CHANGE_ME','session_secret: '+secret)
        if kind=='game':
            if re.search(r'^scene:\s*(?:#.*)?$',cfg,re.M):
                # Merge into the generated scene block (which can already contain other scene settings).
                if re.search(r'^  partition:\s*(?:#.*)?$',cfg,re.M):raise ValueError('scene.partition already configured: review config merge')
                cfg=re.sub(r'^scene:\s*$',lambda m:scene.rstrip(),cfg,count=1,flags=re.M)
            else:cfg+=scene
        file=out/(name+'.yaml');file.write_text(cfg);file.chmod(0o600)
        key=(baseops,sid) if kind=='game' and sid!=1000 else baseops
        start(name,[out/'app',kind,'--sid',sid,'--config',file],demo)
        def ready():
            with urllib.request.urlopen(http(key)+'/readyz',timeout=1) as rsp:return rsp.status==200
        wait(ready,80)
    groups=demo/'configs/activity_groups.yaml'
    text,n=re.subn(r'game_sids: \[[\d, ]+\]','game_sids: [1000, 1001, 1002]',groups.read_text())
    if not n:raise RuntimeError('no activity group game_sids in generated demo')
    groups.write_text(text)
    for i,kind in enumerate(SERVICES):boot(kind,1000,9101+i)
    for sid in (1001,1002,1000):boot('game',sid,9100)
    run('register-role-server',[out/'accountctl','-redis',addr(6379),'-prefix','roost:cwdemo:account','upsert-server','-sid','1000'],demo)
    (out/'topology.json').write_text(json.dumps({'world':world,'ports':{str(k):v for k,v in listeners.items()},'pids':{k:p.pid for k,p in processes},'grace_seconds':args.grace_seconds},indent=2)+'\n')
    print('all 13 demo processes ready; starting both TCP players',flush=True)
    trace=run('drill',[out/'probe','-account-nats','nats://'+addr(4222),'-prefix','cwdemo','-endpoints','1000='+addr(7000),'-ops','1000='+addr(9100)+',1001='+addr((9100,1001))+',1002='+addr((9100,1002)),'-redis',addr(6379),'-world',world,'-grace',f'{args.grace_seconds}s','-wire-trace',out/'frames.jsonl','-out',out/'evidence.json'],demo,120)
    if 'STEP expiry_return ok' not in trace:raise RuntimeError('no expiry_return evidence')
    print(trace,flush=True)

def interrupted(sig,frame):raise KeyboardInterrupt
signal.signal(signal.SIGTERM,interrupted)
try:main()
finally:
    # Only children launched here, stop game processes before dependencies. Leave all evidence.
    for name,p in reversed(processes):
        if p.poll() is None:
            p.terminate()
            try:p.wait(12)
            except sp.TimeoutExpired:p.kill();p.wait()
    print('private run stopped; diagnostics:',out,flush=True)
