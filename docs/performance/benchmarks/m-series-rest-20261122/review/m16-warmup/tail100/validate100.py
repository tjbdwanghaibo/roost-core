"""Offline verification of the original gate / c4 experiment. Stdlib; no services."""
import argparse
import hashlib
import json
import math
import re
from pathlib import Path

HEAD = '5f668a26308443f90dd33e07ea34c58125a7f172'
PROVENANCE_SHA = 'd91f244c14901858ac4d36f115e6f57953ca384ca4a1c833c778a3dd43c3c475'
ORDER = ('a1', 'b1', 'b2', 'a2')
CONFIG = dict(Entities=10000, Players=1000, MessageTargets=1000, MessagesPerPlayer=10,
              HeartbeatHz=1, DirtyPercent=1, Workers=8, IOWorkers=8, Queue=65536,
              Duration=30_000_000_000, Warmup=30_000_000_000, Settle=60_000_000_000,
              Frontend='gate', SceneSync=True, SelfSync=False, HeartbeatDriver='ticker',
              HeartbeatDelivery='', Layout='concentrated', ObservedBlocks=4)


def need(test, msg):
    if not test:
        raise ValueError(msg)


def sha(path):
    with path.open('rb') as f:
        return hashlib.file_digest(f, 'sha256').hexdigest()


def load(path):
    return json.loads(path.read_bytes())


def digest(s):
    return isinstance(s, str) and re.fullmatch('[0-9a-f]{64}', s) is not None


def timing(t, samples, events, label):
    need(t['Samples'] == samples and 0 <= t['SamplesAtCeiling'] <= samples and
         0 <= t['Over50MS'] <= events and 0 <= t['NegativeDurations'] <= events,
         label + ' counts')
    need(0 <= t['P50MS'] <= t['P95MS'] <= t['P99MS'] and
         math.isfinite(t['P99MS']) and math.isfinite(t['MaxMS']) and t['MaxMS'] >= 0,
         label + ' latencies')


def passes(t):
    return t['Samples'] > 0 and t['NegativeDurations'] == 0 and t['P99MS'] <= 50


def wide(h):
    n, over50, overflow = (h[k] for k in ('Samples', 'Over50MS', 'Over60S'))
    need(0 <= overflow <= over50 <= n, 'wide histogram counts')
    previous = -1
    for p in (50, 95, 99):
        v = h[f'P{p}LEMS']; rank = (n*p + 99)//100
        need((v == -1) == (n == 0 or rank > n-overflow), 'empty or overflowing wide quantile')
        if v != -1:
            need(previous <= v <= 60000 and v >= 0 and v % 10 == 0 and
                 (v > 50) == (rank > n-over50), 'wide quantile vs exact over-50 count')
            previous = v


def validate(r, log, rc):
    """Require a fully accounted stock sample; a real latency FAIL is valid evidence."""
    need(r['Config'] == CONFIG, 'not the original fixed c4 config')
    need((r['Platform'], r['GoVersion'], r['GOMAXPROCS']) == ('darwin/arm64', 'go1.27.3', 10), 'different runtime')
    need(not r.get('ClientFirstFailure') and not r.get('Embedded') and not r.get('Beats'), 'frontend or client failure')
    need(r['GateEnabled'] is True and r['GateCompleted'] == 600000, 'gate complete')
    need(r['CountsVerified'] is True and r['EntityValuesVerified'] is True and
         r['FinalNest']['Work']['ProcessedMessages'] == 1200000, 'whole-server totals/values')
    s = r['SyncReport']; h = s['CommittedDeliveries']; stock = s['CommittedToClient']
    # This stock fixture sets ValuesVerified only after awaitValues checks Seen + pinned self,
    # membership and X/HP for *every* configured client. There is no per-client log in this build.
    need(s['Mode'] == 'on_change' and s['ValuesVerified'] is True and
         s['VisibleMin'] == s['VisibleMax'] == 100 and 0 <= s['SettleMS'] <= 60000,
         'all-player final view / values not verified')
    need(s['Frames'] > 0 and s['Updates'] > 0 and all(s[k] >= 0 for k in
         ('SettleFlushCalls', 'SettleFlushMS', 'SettleFlushAvgMS')), 'missing final flush/traffic')
    need(r['Minutes'] and r['Samples'] and r['ElapsedSeconds'] >= 60 and
         all(k in r for k in ('AllocBytes', 'Allocs', 'GCs', 'GCPauseMS')), 'incomplete raw telemetry')
    for key in ('All', 'ThroughWindowEnd', 'AfterWindowEnd'):
        wide(h[key])
    for key in ('Samples', 'Over50MS', 'Over60S'):
        need(h['ThroughWindowEnd'][key] + h['AfterWindowEnd'][key] == h['All'][key], 'split: ' + key)
    n = h['All']['Samples']
    need(n > 0 and stock['Samples'] == n and stock['Over50MS'] == h['All']['Over50MS'], 'wide vs stock')
    for key in ('CommittedToClient', 'PlannedToClient', 'ChangedToClient'):
        timing(s[key], n, n, key)
    need(s['CommittedP99LimitMS']==100, 'missing exact c4 sync limit')
    sync_ok = n > 0 and 0 <= h['All']['P99LEMS'] <= 100 and all(s[k]['NegativeDurations']==0 for k in ('CommittedToClient','PlannedToClient','ChangedToClient'))
    need(sync_ok == s['LatencyPassed'], 'stock sync verdict')
    gates = {'sync': sync_ok, 'gate_rtt': passes(r['GateRoundTrip'])}
    timing(r['GateRoundTrip'], 18750, 300000, 'GateRoundTrip')
    for name in ('Messages', 'Heartbeats'):
        t = r[name]
        need(t['Planned'] == t['Accepted'] == t['Completed'] == 600000 and
             t['MeasuredCompleted'] == 300000 and t['EntityChanges'] == 6000 and
             t['MeasuredEntityChanges'] == 3000 and t['Rejected'] == 0 and not t['FirstError'], name + ' totals')
        for k in ('QueueWait', 'Handler', 'Completion', 'ChangedEntityCompletion',
                  'Harness.AdmitLag', 'Harness.ClientQueue', 'Server.QueueWait', 'Server.Completion'):
            v = t
            for sub in k.split('.'):
                v = v[sub]
            changed = k == 'ChangedEntityCompletion'
            timing(v, 3000 if changed else 18750, 3000 if changed else 300000, name + '.' + k)
        gates[name] = (passes(t['Completion']) and passes(t['ChangedEntityCompletion']) and
                       all(v['NegativeDurations'] == 0 for v in (t['QueueWait'], t['Harness']['AdmitLag'],
                       t['Harness']['ClientQueue'], t['Server']['QueueWait'], t['Server']['Completion'])))
    ok = all(gates.values()); verdict = 'PASS' if ok else 'FAIL'
    need(r['LatencyPassed'] is ok, 'stock aggregate verdict')
    end = re.findall(r'^--- (PASS|FAIL): TestNestGameLoad \(([\d.]+)s\)$', log, re.M)
    need(len(end) == 1 and end[0][0] == verdict and log.splitlines()[-1] == verdict and
         rc == (0 if ok else 1) and log.count('=== RUN   TestNestGameLoad\n') == 1 and
         ('business load gate failed; full result retained' in log) == (not ok) and
         not re.search(r'(?m)^(?:panic:|fatal error:|SIG[A-Z]+:)', log), 'verdict/exit or execution failure')
    # All evidence remains available as the original bytes. Expose the full latency/split fields,
    # not just quantiles from the stock 100ms ceiling or a throughput-only comparison.
    return dict(original_verdict=verdict, original_exit=rc, go_test_s=float(end[0][1]),
                elapsed_s=r['ElapsedSeconds'], gates=gates, players=1000, visible_range=[s['VisibleMin'], s['VisibleMax']],
                all_player_final_values=s['ValuesVerified'], server_values=r['EntityValuesVerified'],
                client_first_failure=r.get('ClientFirstFailure'), counts_verified=r['CountsVerified'],
                gate_completed=r['GateCompleted'], gate_rtt=r['GateRoundTrip'], messages=r['Messages'],
                heartbeats=r['Heartbeats'], sync=s, runtime={k:r[k] for k in ('GoVersion','GOMAXPROCS','Platform')})


def private_localhost(log, nats_logs):
    """Actual local endpoints must match across three private node logs and the fixture."""
    clients, routes, pids = [], [], []
    for i, text in enumerate(nats_logs):
        need('Server is ready' in text and 'Server Exiting' in text and
             re.search(r'\bName:\s+mseries'+str(i)+r'\s*$', text, re.M) and
             'Cluster name is mseries' in text and 'Route connection created' in text, 'private NATS lifecycle')
        owners = set(re.findall(r'^\[(\d+)\]', text, re.M)); need(len(owners) == 1, 'mixed NATS PID'); pids += list(owners)
        for k, dest in (('client', clients), ('route', routes)):
            addrs = re.findall(r'Listening for '+k+r' connections on (\S+)', text)
            need(len(addrs) == 1 and re.fullmatch(r'127\.0\.0\.1:\d+', addrs[0]), 'nonprivate NATS ' + k)
            dest.append(addrs[0])
    need(len(nats_logs) == len(set(pids)) == 3 and len(set(clients + routes)) == 6, 'three private nodes / six distinct endpoints')
    urls = re.findall(r'\bnats: connected url=(\S+)', log)
    need(len(urls) >= 2 and all(u == ','.join('nats://' + a for a in clients) for u in urls), 'gate/backend vs private cluster')
    gate = re.findall(r'player tcp listener started addr=(\S+)', log)
    need(len(gate) == 1 and re.fullmatch(r'127\.0\.0\.1:\d+', gate[0]) and gate[0] not in clients + routes, 'gate listener')
    return dict(gate=gate[0], nats_clients=clients, nats_routes=routes)
