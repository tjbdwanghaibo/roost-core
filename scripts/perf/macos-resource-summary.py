#!/usr/bin/env python3
"""按探针启动、Mongo启动/结束划分稳定窗口；保留每秒指标，累计计数均做差。"""
import argparse
import datetime as dt
import json
import re
import statistics
from pathlib import Path

parser=argparse.ArgumentParser(description=__doc__)
parser.add_argument('directory',type=Path)
parser.add_argument('--output',type=Path,required=True)
a=parser.parse_args(); p=a.directory

def timestamp(s): return dt.datetime.fromisoformat(s.replace('Z','+00:00')).timestamp()
def readlines(name): return [json.loads(s) for s in (p/name).read_text().splitlines()]
def cputime(s):
    parts=s.split(':');return sum(float(v)*60**i for i,v in enumerate(reversed(parts)))
def processes(s):
    result={}
    for line in s.splitlines()[1:]:
        v=line.strip().split(None,3)
        if len(v)==4: result[v[0]]={'cpu':cputime(v[1]),'rss':int(v[2]),'name':v[3]}
    return result

def vm(s):return {k.strip(' "'):int(v) for k,v in re.findall(r'^([^:\n]+):\s+(\d+)\.',s,re.M)}
def network(s):
    result={}
    for line in s.splitlines():
        v=line.split()
        if len(v)>=10 and v[2].startswith('<Link#'):
            result[v[0]]={'in':int(v[-5]),'out':int(v[-2]),'errors':int(v[-6])+int(v[-3])}
    return result

probe=json.loads((p/'probe.json').read_text());begin=timestamp(probe['StartedAt'])
phases={k:timestamp(v) for k,v in [line.split() for line in (p/'phases.txt').read_text().splitlines()]}
windows={'before':(begin+5,phases['mongo_start']-2),'mixed':(phases['mongo_start']+5,phases['mongo_end']-5),'after':(phases['mongo_end']+5,begin+probe['Seconds']-2)}
raw=readlines('resources.jsonl'); errors=[r for r in raw if 'error'in r]
gaps=[timestamp(y['at'])-timestamp(x['at']) for x,y in zip(raw,raw[1:])]
if not gaps or min(gaps)<=0 or max(gaps)>3:raise ValueError('clock/sampling gap: phase alignment is unreliable')
if errors:raise ValueError(f'{len(errors)} sampling errors; inspect raw evidence')
samples=[]
for prev,cur in zip(raw,raw[1:]):
    at=timestamp(cur['at']);seconds=at-timestamp(prev['at']);v0,v1=vm(prev['vm']),vm(cur['vm'])
    page=int(re.search(r'page size of (\d+)',cur['vm']).group(1))
    row={'at':at,'sample_seconds':cur['sample_seconds']}
    for key in ['Swapins','Swapouts','Pageins','Pageouts','Compressions','Decompressions']:
        row[key+'_MBps']=(v1[key]-v0[key])*page/1048576/seconds
    row['compressor_MB']=v1['Pages occupied by compressor']*page/1048576
    n0,n1=network(prev['network']),network(cur['network'])
    for name in ['lo0','en0']:
        if name in n1 and name in n0:
            for key in ['in','out','errors']: row[name+'_'+key+('_MBps' if key!='errors' else '_per_sec')]=(n1[name][key]-n0[name][key])/seconds/(1048576 if key!='errors' else 1)
    ds0={d['id']:d['statistics'] for d in prev['disks']};ds1={d['id']:d['statistics'] for d in cur['disks']}
    for kind in ['Read','Write']:
        delta={key:sum(d[key]-ds0[i][key] for i,d in ds1.items() if i in ds0) for key in [f'Bytes ({kind})',f'Operations ({kind})',f'Total Time ({kind})']}
        ops=delta[f'Operations ({kind})'];row['disk_'+kind+'_MBps']=delta[f'Bytes ({kind})']/1048576/seconds;row['disk_'+kind+'_IOPS']=ops/seconds
        row['disk_'+kind+'_mean_ms']=delta[f'Total Time ({kind})']/1e6/ops if ops else 0
    ps0,ps1=processes(prev['processes']),processes(cur['processes']);row['processes']={}
    for pid,proc in ps1.items():
        if pid in ps0 and proc['name']==ps0[pid]['name']:
            row['processes'][pid]={'name':proc['name'],'cpu_percent':max(0,proc['cpu']-ps0[pid]['cpu'])*100/seconds,'rss_MB':proc['rss']/1024}
    for group,needle in [('mongo','mongod'),('nats','nats-server')]:
        members=[v for v in row['processes'].values() if Path(v['name']).name==needle]
        row[group+'_cpu_percent']=sum(v['cpu_percent'] for v in members);row[group+'_rss_MB']=sum(v['rss_MB'] for v in members)
    samples.append(row)

io=[]
for r in readlines('iostat.jsonl'):
    fields=r['line'].split()
    if len(fields)==9:
        try: vals=list(map(float,fields))
        except ValueError:continue
        io.append({'at':timestamp(r['at']),'cpu_busy_percent':100-vals[5],'iostat_MBps':vals[2],'iostat_IOPS':vals[1]})
io=io[1:] # 丢弃开机累计均值。
summary={'Windows':windows,'Phases':phases,'SamplingErrors':len(errors),'Stages':{},'Samples':samples,'IOStat':io}
def stats(values): return {'mean':statistics.mean(values),'max':max(values)}
for phase,(lo,hi) in windows.items():
    selected=[r for r in samples if lo<=r['at']<=hi];stage={'Samples':len(selected)}
    if not selected:raise ValueError('empty stable window')
    for key in selected[0]:
        if key not in ['at','processes']:stage[key]=stats([r[key] for r in selected])
    for key in ['cpu_busy_percent','iostat_MBps','iostat_IOPS']:stage[key]=stats([r[key] for r in io if lo<=r['at']<=hi])
    timing=[r for r in probe['PerSecond'] if lo<=begin+r['Second'] and begin+r['Second']+1<=hi]
    stage['ProbeCount']=sum(r['Count'] for r in timing)
    for kind in ['Delivery','PubAck']:
        stage[kind+'_weighted_mean_ms']=sum(r[kind]['MeanMS']*r['Count'] for r in timing)/stage['ProbeCount']
        stage[kind+'_max_second_p99_ms']=max(r[kind]['P99MS'] for r in timing)
    per_process={}
    for r in selected:
        for pid,proc in r['processes'].items():
            item=per_process.setdefault(pid,{'name':proc['name'],'cpu':[],'rss':[]});item['cpu'].append(proc['cpu_percent']);item['rss'].append(proc['rss_MB'])
    stage['TopProcesses']=sorted([{'pid':pid,'name':v['name'],'cpu_percent':sum(v['cpu'])/len(selected),'max_rss_MB':max(v['rss'])} for pid,v in per_process.items()],key=lambda r:r['cpu_percent'],reverse=True)[:12]
    summary['Stages'][phase]=stage
# 原始逐秒进程清单只留本地产物；可携带摘要不包含其它应用路径。
summary.pop('Samples');summary.pop('IOStat')
a.output.write_text(json.dumps(summary,indent=2)+'\n')
for phase,s in summary['Stages'].items():
    print(phase,'delivery',round(s['Delivery_weighted_mean_ms'],2),'cpu',round(s['cpu_busy_percent']['mean'],1),'disk write MB/s',round(s['disk_Write_MBps']['mean'],2),'write ms',round(s['disk_Write_mean_ms']['mean'],3),'swapout',s['Swapouts_MBps']['mean'])
