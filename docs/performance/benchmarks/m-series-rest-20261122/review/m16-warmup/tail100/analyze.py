"""Same-process M16 tail attribution. Python stdlib, standalone; no repo or service imports.

Source contracts: m16GapClientRow.MarshalJSON (client row[5] = original per-update
stock endpoint, [10] = Receiver.Decode return); M16GapSnapshot; original timings and
syncDeliveryHist. Never infer identity by adjacent timestamps, or SID by client index.
Use: analyze(trace_dict, report_dict), or python3 -B analyze.py TRACE[.gz] REPORT[.gz].
"""
from bisect import bisect_left, bisect_right
from collections import Counter, defaultdict
from math import ceil
import gzip
import json
from pathlib import Path
import sys

MS = 1_000_000
PHASES = ('All', 'ThroughWindowEnd', 'AfterWindowEnd')
STAGES = ('commit_to_first_observed_eligible', 'first_observed_eligible_to_selection',
          'selection_to_this_release', 'this_release_to_prepare', 'prepare_to_encode_start',
          'encode_start_to_client_stock_endpoint')
REST = ('session_encode', 'encode_end_to_frame_push_start', 'push_start_to_client_apply',
        'apply_to_locked', 'locked_to_receiver_decode_return', 'decode_return_to_stock_endpoint')
PRE = ('commit_to_ready_observed_if_later', 'max_commit_ready_to_eligible_observed')
_AMBIG = object()


def require(ok, text):
    if not ok:
        raise ValueError(text)


def integer(x):
    return type(x) is int


def ints(row, n, tag):
    require(isinstance(row, (tuple, list)) and len(row) == n and all(map(integer, row)),
            f'{tag}: expected {n} exact integers')
    return row


def uniq(d, k, v):
    d[k] = _AMBIG if k in d else v


def rank(n, pct):
    return (n * pct + 99) // 100   # nearest rank, 1-based


def dist(x):
    a = sorted(x)
    return {'n': len(a), 'min': a[0] if a else None,
            **{name: a[rank(len(a), q)-1] if a else None for name, q in
               [('p50',50), ('p95',95), ('p99',99), ('max',100)]}}


def stock(a):
    # Go math.Ceil(float64(samples)*p); final 10 us bin is saturating, not a bound.
    n = len(a)
    def q(p):
        x = max(0, a[ceil(n*p)-1]) if n else 0
        return min((x + 9999)//10000, 10000)/100
    return {'Samples': n, 'NegativeDurations': bisect_left(a, 0), 'Over50MS': n-bisect_right(a, 50*MS),
            'SamplesAtCeiling': n-bisect_right(a, 99_990_000), 'MaxMS': max(0, a[-1])/MS if n else 0,
            'P50MS': q(.5), 'P95MS': q(.95), 'P99MS': q(.99)}


def wide(a):
    n = len(a)
    def q(p):
        if not n: return -1
        v = a[rank(n,p)-1]
        return 10*((max(0, v) + 10*MS-1)//(10*MS)) if v <= 60_000*MS else -1
    return {'Samples': n, 'Over50MS': n-bisect_right(a, 50*MS), 'Over60S': n-bisect_right(a, 60_000*MS),
            **{f'P{p}LEMS': q(p) for p in (50,95,99)}}


def check(expected, actual):
    actual = actual if isinstance(actual, dict) else {}
    # Do not tolerate a missing zero-valued field, or a bool masquerading as a number.
    diff = {k: {'computed': v, 'frozen': actual.get(k)} for k,v in expected.items()
            if k not in actual or actual[k] != v or isinstance(actual[k], bool) != isinstance(v, bool)}
    return {'ok': not diff, 'computed': expected, 'differences': diff}


def reconcile(groups, report):
    sr = report.get('SyncReport',report)
    w = sr.get('CommittedDeliveries') or {}
    wc, audit = {}, {}
    for name in PHASES:
        a = sorted(p.ns for p in groups[name])
        wc[name] = check(wide(a), w.get(name))
        audit[name] = {**dist(a), 'negative': bisect_left(a,0), 'above_100ms': len(a)-bisect_right(a,100*MS),
                       'at_100ms': bisect_right(a,100*MS)-bisect_left(a,100*MS)}
    sc = {}
    for name,col in (('PlannedToClient',3), ('ChangedToClient',11), ('CommittedToClient',4)):
        sc[name] = check(stock(sorted(p.r[5]-p.r[col] for p in groups['All'])), sr.get(name))
    all_w, c = wc['All']['computed'], sc['CommittedToClient']['computed']
    negatives = {k:v['computed']['NegativeDurations'] for k,v in sc.items()}
    gatebase = bool(c['Samples'] and not any(negatives.values()))
    widegate = bool(gatebase and all_w['Samples']==c['Samples'] and 0 <= all_w['P99LEMS'] <= 100)
    limit = sr.get('CommittedP99LimitMS',50)  # archived pre-limit fixture used 50 ms
    gate = widegate if limit==100 else bool(gatebase and c['P99MS']<=50) if limit==50 else False
    cmpgate = check({'LatencyPassed':gate}, sr)
    return {'ok': all(x['ok'] for x in [*wc.values(),*sc.values(),cmpgate]),
            'stock_all': sc, 'wide_commit_by_endpoint': wc, 'raw_commit_ns': audit,
            'strict_100ms_wide_all_pass': widegate, 'stock_sync_limit_ms':limit, 'stock_sync_verdict':cmpgate}


class Delivery:
    __slots__ = ('r','sid','idx','id','fk','pk','fr','ns','legs','post','first','status','reasons','rid','cross','f','other_release_count','other_release_ns')
    def __init__(self, r, idx, sid, ordinal):
        self.r, self.idx, self.sid = r,idx,sid
        self.fk = (r[0],r[3],r[11]); self.pk=tuple(r[:3]); self.fr=(sid,r[6],r[7]); self.ns=r[5]-r[4]
        self.id=(idx,r[6],r[7],*self.pk,r[3],r[11],r[4],r[5],ordinal)
        self.f = self.legs = self.post = self.first = self.rid = self.cross = None
        self.status, self.reasons = 'valid', ()
        self.other_release_count = self.other_release_ns = 0


def status(rows):
    counts = Counter(p.status for p in rows)
    return {s:counts[s] for s in ('valid','ambiguous','missing','noncausal','exceptional')}


def multiplicity(rows):
    def info(keys):
        c = Counter(keys)
        return {'distinct':len(c), 'max_repetitions':max(c.values(),default=0)}
    return {'source_changes': info(p.fk for p in rows), 'prepared_versions':info(p.pk for p in rows),
            'successful_frames':info(p.fr for p in rows),'flush_rounds':info(p.rid for p in rows),
            'policy_round_relation':dict(Counter(p.cross for p in rows))}


def totals(rows, bounds, mid):
    valid = [p for p in rows if p.status=='valid']
    allsum = sum(p.ns for p in rows); denom = sum(p.ns for p in valid)
    def durations(attr, names):
        cols = map(sum,zip(*(getattr(p,attr) for p in valid))) if valid else [0]*len(names)
        return dict(zip(names,cols))
    sums=durations('legs',STAGES); post=durations('post',REST); first=durations('first',PRE)
    require(sum(sums.values())==denom and sum(post.values())==sums[STAGES[-1]] and sum(first.values())==sums[STAGES[0]], 'stage accounting')
    return {**bounds,'selected':len(rows), 'selected_status':status(rows), 'all_selected_sum_e2e_ns':allsum,
            'valid_denominator_e2e_ns':denom, 'excluded_sum_e2e_ns':allsum-denom,
            'stage_sums_ns':sums,'stage_share':{k:v/denom if denom else None for k,v in sums.items()},
            'within_encode_start_to_endpoint_ns':post,'within_commit_to_eligible_ns':first,
            'within_release_to_prepare_observations':{
                'valid_rows_with_strictly_later_same_subject_release_by_prepare':sum(p.other_release_count>0 for p in valid),
                'max_later_releases_for_one_row':max((p.other_release_count for p in valid),default=0),
                'this_release_to_latest_observed_same_subject_release_ns':sum(p.other_release_ns for p in valid),
                'latest_observed_same_subject_release_to_prepare_ns':sums[STAGES[3]]-sum(p.other_release_ns for p in valid)},
            'valid_multiplicity':multiplicity(valid), 'valid_planned_half':dict(Counter('first' if p.r[3]<=mid else 'second' for p in valid))}


def cohorts(rows, mid, by_fact=False):
    # ascending physical duration; a single order for both slices, exactly ceil(1%) at high end
    if by_fact: ordered=sorted(rows,key=lambda p:(p.ns,p.fk,p.id))
    else: ordered=sorted(rows,key=lambda p:(p.ns,p.id))
    n=len(ordered); ns=[p.ns for p in ordered]
    def band(lo,hi):  # 0-based half-open
        desc = {'rank_lo_inclusive':lo+1 if hi else None,'rank_hi_inclusive':hi or None}
        for key,i in [('low',lo),('high',hi-1)]:
            x=ns[i] if hi else None
            a,b=(bisect_left(ns,x),bisect_right(ns,x)) if hi else (0,0)
            desc[key]={'duration_ns':x,'all_tied':b-a,'selected_tied':max(0,min(hi,b)-max(lo,a))}
        return totals(ordered[lo:hi],desc,mid)
    return {'rank_population':n,'endpoint_latency_ns':dist(ns),
            'near_median_p45_p55':band(max(0,rank(n,45)-1),rank(n,55)),
            'slowest_ceil_1pct':band(n-(n+99)//100,n)}


def analyze(trace, report):
    """Return a JSON-compatible result. ok is analytical integrity, NOT the SLA verdict."""
    fs,end=trace['formal_ns'],trace['end_ns']; require(integer(fs) and integer(end) and 0<=fs<end,'invalid window')
    mid=(fs+end)//2; cli=trace['client']; srv=trace['server']
    require(cli.get('schema')==2 and isinstance(cli['clients'],list),'expected client schema 2 with changed_ns at index 11')
    fx={}; ids=Counter(); formal=[]; facts=[]; curs={}; callback_bad=Counter()
    necessary=('ID','Subject','Planned','Changed','Registered','Ready','Eligible','Selected','Released','Exception',
               'EligibleLane','SelectedLane','ReleaseLane','EligibleRound','SelectedRound')
    for owner,it in enumerate(trace['interests']):
        for c in it['Settles'] or ():
            for k in ('ID','Locked','End','Selected'):
                require(integer(c[k]),'invalid settle '+k)
            uniq(curs,(owner,c['ID']),c)
        for f in it['Facts'] or ():
            for k in necessary:
                require(integer(f.get(k)), 'missing / noninteger fact.'+k)
            key=(f['Subject'],f['Planned'],f['Changed']);ids[f['ID']]+=1;uniq(fx,key,(owner,f)); facts.append((key,owner,f))
            if fs<key[1]<=end: formal.append((key,owner,f))
    observed_releases=defaultdict(list)
    for key,_,f in facts:
        if f['Released']>=0:observed_releases[key[0]].append(f['Released'])
    for times in observed_releases.values():times.sort()
    fissues={}; tally=Counter(); counts_sel=Counter(); reasons_all=Counter()
    for key,o,f in facts:
        a=[];m=[];bad=[];exc=[]
        if fx[key] is _AMBIG: a.append('duplicate_fact_source_key')
        if ids[f['ID']]!=1 or f['ID']<=0: a.append('duplicate_or_invalid_fact_id')
        marks=('Registered','Ready','Eligible','Selected','Released')
        for n in marks:
            if f[n]<0:m.append('missing_fact_'+n.lower())
        for left,right in zip(marks,marks[1:]):
            if min(f[left],f[right])>=0 and f[left]>f[right]:bad.append('fact_'+left.lower()+'_after_'+right.lower())
        if f['Changed']<=0 or f['Planned']<=0:m.append('missing_fact_source_stamp')
        if 0<f['Changed']<f['Planned']:bad.append('change_before_plan')
        if f['Registered']>=0 and f['Changed']>f['Registered']:bad.append('change_after_registration')
        if f['Exception']:exc.append('exceptional_policy_fact')
        if f['ReleaseLane']==5:exc.append('close_release')
        elif f['Released']>=0 and f['ReleaseLane'] not in (1,4):m.append('invalid_release_lane')
        for n in ('Eligible','Selected'):
            lane,rr,t=f[n+'Lane'],f[n+'Round'],f[n]
            if t<0:continue
            if lane==1:
                cb=curs.get((o,rr))
                if cb is _AMBIG:a.append('ambiguous_'+n.lower()+'_round')
                elif cb is None:m.append('missing_'+n.lower()+'_round')
                elif not cb['Locked']<=t<=cb['End']:bad.append(n.lower()+'_outside_own_callback')
                if n=='Selected': counts_sel[o,rr]+=1
            elif lane not in (2,3) or rr!=0:m.append('invalid_'+n.lower()+'_lane_round')
        if f['EligibleLane']==f['SelectedLane']==1 and f['SelectedRound']<f['EligibleRound']:bad.append('negative_policy_round_delta')
        issue=(a,m,bad,exc);fissues[id(f)]=issue
        reasons_all.update(a+m+bad+exc)
        if fs<key[1]<=end:tally.update(a+m+bad+exc)
    for k,cb in curs.items():
        if cb is _AMBIG:callback_bad['duplicate_round']+=1
        elif counts_sel[k]!=cb['Selected']:callback_bad['selected_count_mismatch']+=1

    prepared={};frames={};server=Counter()
    for rr in srv['prepared'] or ():
        s,b,v,t=ints(rr,4,'prepared');uniq(prepared,(s,b,v),t);server['prepared_rows']+=1
    for rr in srv['frames'] or ():
        require(len(rr)==9,'server frame width'); rid,sid,ep,tick,enc,done=ints(rr[:6],6,'frame prefix')
        push,ret,ok=rr[6:]
        if push is ret is ok is None: kind='unattempted'
        else:
            require(integer(push) and integer(ret) and type(ok) in (int,bool) and ok in (0,1),'bad push status/times')
            kind='success' if ok else 'failed'
        uniq(frames,(sid,ep,tick),(kind,rid,enc,done,push,ret));server[kind+'_frames']+=1
    server['duplicate_prepared_keys']=sum(v is _AMBIG for v in prepared.values());server['duplicate_frame_keys']=sum(v is _AMBIG for v in frames.values())
    opens={};owners=defaultdict(set); bindings=Counter(); bind={}
    for c in cli['clients']:
        i=c['index'];require(integer(i) and i>=0 and i not in opens,'duplicate/invalid client index')
        opens[i]=c['opens'] or []
        for row in opens[i]:
            sid,ok=ints(row,2,'session open');require(ok in (0,1),'bad open status');owners[sid].add(i)
    for i,op in opens.items():
        sid=op[0][0] if op else None
        if len(op)>1:k='repeated_open'
        elif not op:k='missing_open'
        elif len(owners[sid])>1:k='shared_sid'
        elif not op[0][1]:k='unsuccessful_open'
        else:k='unique'
        bind[i]=(k,sid);bindings[k]+=1
    pop=[];local=Counter(); prep_values=defaultdict(set); commits=defaultdict(set); cited=Counter();rejected_plans=0;all_rows=0
    for c in cli['clients']:
        i=c['index'];_,sid=bind[i]
        for j,r in enumerate(c['rows'] or ()):
            ints(r,12,'client.rows');all_rows+=1
            ck=(i,r[6],r[7],r[0],r[1],r[2]);local[ck]+=1
            if not fs<r[3]<=end:rejected_plans+=1;continue
            p=Delivery(r,i,sid,j);pop.append(p);prep_values[p.pk].add((p.fk,r[4]));commits[p.fk].add(r[4]);cited[p.fk]+=1
    examples=defaultdict(list); order_count=Counter()
    for p in pop:
        a=[];m=[];bad=[];exc=[];r=p.r; binding,_=bind[p.idx]
        if binding in ('repeated_open','shared_sid'):a.append(binding)
        elif binding!='unique':m.append(binding)
        if local[(p.idx,r[6],r[7],*p.pk)]!=1:a.append('duplicate_client_delivery_key')
        if r[2]<=r[1]:bad.append('nonadvancing_component_version')
        if len(prep_values[p.pk])!=1:a.append('conflicting_payload_for_prepared_version')
        if len(commits[p.fk])!=1:a.append('conflicting_commit_for_source_change')
        fact=fx.get(p.fk);prep=prepared.get(p.pk);fr=frames.get(p.fr) if binding=='unique' else None
        if fact is _AMBIG:a.append('duplicate_fact_source_key')
        elif fact is None:m.append('missing_fact_source_key')
        else:
            _,f=fact;p.f=f
            ai,mi,bi,ei=fissues[id(f)];a+=ai;m+=mi;bad+=bi;exc+=ei
            if r[11]>r[4]:bad.append('change_after_commit')
            if f['Eligible']>=0 and r[4]>f['Eligible']:bad.append('eligible_observed_before_commit')
            if f['Ready']>=0 and f['Eligible']>=0:
                order=('commit_before_ready' if r[4]<f['Ready'] else 'commit_equals_ready' if r[4]==f['Ready'] else
                       'ready_before_commit_at_or_before_eligible' if r[4]<=f['Eligible'] else 'eligible_before_commit')
                order_count[order]+=1
            if f['EligibleLane']==f['SelectedLane']==1:
                p.cross='same_normal_round' if f['EligibleRound']==f['SelectedRound'] else 'cross_normal_round'
            else:p.cross='not_both_normal'
        if prep is _AMBIG:a.append('duplicate_prepared_key')
        elif prep is None:m.append('missing_prepared_key')
        if fr is _AMBIG:a.append('duplicate_frame_key')
        elif binding=='unique' and fr is None:m.append('missing_frame_key')
        elif fr is not None and fr[0]!='success':m.append(fr[0]+'_push')
        if prep is not None and prep is not _AMBIG:
            if prep<r[4]:bad.append('prepare_before_commit')
            if p.f and p.f['Released']>=0 and prep<p.f['Released']:bad.append('prepare_before_this_release_observation')
        if fr is not None and fr is not _AMBIG and fr[0]=='success':
            _,p.rid,enc,done,push,ret=fr
            if ret<push:bad.append('push_return_before_start')
            if prep is not None and prep is not _AMBIG and enc<prep:bad.append('encode_start_before_prepare')
            ends=(enc,done,push,r[8],r[9],r[10],r[5]);post=tuple(y-x for x,y in zip(ends,ends[1:]));bad.extend(k for k,v in zip(REST,post) if v<0)
        if r[5]<r[4]:bad.append('negative_commit_to_endpoint')
        p.reasons=tuple(sorted(set(a+m+bad+exc)))
        p.status='ambiguous' if a else 'missing' if m else 'exceptional' if exc else 'noncausal' if bad else 'valid'
        if p.status!='valid':
            for issue in p.reasons:
                if len(examples[issue])<2:examples[issue].append({'client_index':p.idx,'sid_epoch_tick':list(p.fr),'prepared_key':list(p.pk),'source_key':list(p.fk),'commit_ns':r[4],'endpoint_ns':r[5]})
            continue
        f=p.f;ends=(r[4],f['Eligible'],f['Selected'],f['Released'],prep,enc,r[5]);p.legs=tuple(y-x for x,y in zip(ends,ends[1:]));p.post=post
        h=max(r[4],f['Ready']);p.first=(h-r[4],f['Eligible']-h)
        releases=observed_releases[r[0]];lo=bisect_right(releases,f['Released']);hi=bisect_right(releases,prep)
        p.other_release_count=hi-lo;p.other_release_ns=releases[hi-1]-f['Released'] if hi>lo else 0
        require(min(*p.legs,*post,*p.first)>=0 and sum(p.legs)==p.ns and sum(post)==p.legs[-1] and sum(p.first)==p.legs[0], 'per-delivery partition')
    groups={'All':pop,'ThroughWindowEnd':[p for p in pop if p.r[5]<=end],'AfterWindowEnd':[p for p in pop if p.r[5]>end]}
    stock_result=reconcile(groups,report)
    require(all_rows==rejected_plans+len(pop) and len(pop)==sum(len(groups[k]) for k in PHASES[1:]),'raw census mismatch')
    delivery={}
    for k,rows in groups.items():
        delivery[k]={'status':status(rows),'reasons_nonexclusive':dict(Counter(x for p in rows for x in p.reasons)),
                     'whole_population':totals(rows,{},mid),**cohorts(rows,mid)}
    halves={k:cohorts([p for p in pop if (p.r[3]<=mid)==first],mid) for first,k in [(True,'first'),(False,'second')]}
    formal_by_key={k:f for k,o,f in formal if fx[k] is not _AMBIG and ids[f['ID']]==1 and f['ID']>0}
    agg={k:defaultdict(list) for k in PHASES};observed=defaultdict(list)
    for p in pop:
        if p.fk in formal_by_key:
            observed[p.fk].append(p)
            agg['All'][p.fk].append(p);agg[PHASES[1+int(p.r[5]>end)]][p.fk].append(p)
    sources={}; source_issues={}; source_represented={}
    for name,by in agg.items():
        reps_med=[];reps_max=[];fan=[];issues=Counter();removed={};rpt=0;vcounts=Counter()
        for k,rr in by.items():
            unique_rcv={p.sid for p in rr}; nr=len(rr)-len(unique_rcv);rpt+=nr
            why=[]
            if nr:why.append('repeated_or_unbound_recipient')
            if any(p.status!='valid' for p in rr):why.append('incomplete_or_invalid_delivery_join')
            if why:
                issues.update(why);removed[formal_by_key[k]['ID']]=why;continue
            sr=sorted(rr,key=lambda p:(p.ns,p.id));reps_med.append(sr[rank(len(sr),50)-1]);reps_max.append(sr[-1]);fan.append(len(sr));vcounts[len({p.pk for p in sr})]+=1
        sources[name]={'unique_changes_with_client_rows':len(by),'eligible_complete_changes':len(fan),
                       'excluded_source_reason_counts_nonexclusive':dict(issues),'excess_rows_over_distinct_sid':rpt,
                       'fanout_distinct_recipient_count':dist(fan),'sum_fanout':sum(fan),
                       'fanout_histogram':{str(i):n for i,n in sorted(Counter(fan).items())},
                       'distinct_prepared_versions_per_change_histogram':{str(i):n for i,n in sorted(vcounts.items())},
                       'by_median_recipient':cohorts(reps_med,mid,True), 'by_slowest_recipient':cohorts(reps_max,mid,True)}
        source_issues[name]=removed;source_represented[name]=set(by)
    undel=[(k,f) for k,f in formal_by_key.items() if k not in observed];undel.sort(key=lambda x:x[1]['ID'])
    # Same-subject later delivered state is only a compatibility bound, not proof of coalescing.
    # No reliable initial subscription population or per-recipient supersession is in this trace.
    per_subject=defaultdict(list)
    for k in source_represented['All']:
        if k in formal_by_key and formal_by_key[k]['ID'] not in source_issues['All']:
            per_subject[k[0]].append((k[2],k))
    for seq in per_subject.values():seq.sort()
    next_counts=Counter(); next_examples=[]
    for k,f in undel:
        seq=per_subject.get(k[0],());j=bisect_right(seq,k[2],key=lambda x:x[0]);nxt=seq[j][1] if j<len(seq) else None
        category='no_later_observed_change_in_formal_source_window' if nxt is None else 'later_observed_change_same_subject'
        next_counts[category]+=1
        if len(next_examples)<8:next_examples.append({'undelivered_fact_id':f['ID'],'source_key':list(k),'next_later_delivered_source_key':list(nxt) if nxt else None})
    counts_by_plan=Counter(k[1] for k in formal_by_key);subjects_per_plan=defaultdict(set)
    changes_by_subject_plan=Counter(k[:2] for k in formal_by_key)
    for k in formal_by_key:subjects_per_plan[k[1]].add(k[0])
    expected=None
    if all(isinstance(report.get(k),dict) and integer(report[k].get('MeasuredEntityChanges')) for k in ('Messages','Heartbeats')):
        expected=sum(report[k]['MeasuredEntityChanges'] for k in ('Messages','Heartbeats'))
    formal_n=len(formal);complete=len(formal_by_key);represented=len(observed)
    require(complete==len(undel)+represented and represented==sources['All']['eligible_complete_changes']+len(source_issues['All']),'source census mismatch')
    good=(bool(pop) and rejected_plans==0 and delivery['All']['status']['valid']==len(pop) and stock_result['ok'] and
          not reasons_all and not callback_bad and not source_issues['All'] and (expected is None or expected==formal_n))
    return {'schema':'m16-tail100/v1','ok':bool(good),'window_ns':{'formal_exclusive':fs,'mid_inclusive':mid,'end_inclusive':end},
            'rules':{'unit':'same loadClockOrigin monotonic integer ns; not wall/CPU time',
                     'population':'formal planned in (formal,end], including every raw recorded non-full update through final verification; endpoint >end is AfterWindowEnd, not necessarily settle time',
                     'rank':'sort raw signed commit→stock endpoint ns ascending before join rejection. Near median includes nearest ranks ceil(.45n)..ceil(.55n); slowest = highest ceil(.01n) ranks. Ties break ascending (client index,epoch,tick,subject,base,version,plan,changed,commit,endpoint,row ordinal); fact ranks use source key first. No expansion at ties.',
                     'numerator':'per-path adjacent clock differences summed; shares / valid_denominator_e2e_ns; excluded signed sums shown. Nested partitions do not add them again. Ready may precede commit and is NOT added unconstrained.',
                     'meaning':'eligible is FIRST policy observation of selectable, not intrinsic readiness; selection/release belong only to THIS fact. Subject may have other barriers. Release→prepare is an elapsed interval, not proof of final barrier ownership. Push return may overlap client processing; not an additive leg.',
                     'source':'within each endpoint population give each unique (Subject,Planned,Changed) one vote; choose actual recipient lower nearest-rank median and maximum separately (receive durations then delivery tie key), preserving additive legs. Exclude entire source group with any incomplete or repeated-SID delivery instead of imputing. No zero for missing. Phases may share facts; not additive.',
                     'other_releases':'optional elapsed-time split at latest recorded release on this subject in (this_release,prepare], or this release if none. Includes all observed facts, not proof the later release was a blocker or the last actual hold.',
                     'coalescing':'full/baseline and unobserved intermediate states are not client deliveries. Later represented state on same subject is compatible with conflation, not evidence of a particular receiver or cause. No expected fanout can be inferred from absent rows.'},
            'input':{'raw_client_rows':all_rows,'nonformal_rows':rejected_plans,'formal_raw_rows':len(pop),'client_bindings':dict(bindings),'server':dict(server)},
            'stock_reconciliation':stock_result,'delivery':delivery,'delivery_by_planned_half_all_endpoints':halves,
            'formal_source_census':{'all_trace_fact_rows':len(facts),'formal_fact_rows':formal_n,'reported_formal_changes':expected,
                                    'unique_identifiable_formal_changes':complete,'unidentifiable_formal_fact_rows':formal_n-complete,
                                    'represented_unique_changes':represented,'unique_changes_without_any_client_row':len(undel),
                                    'no_client_row_fact_ids':[f['ID'] for k,f in undel], 'undelivered_observational_bounds':dict(next_counts),
                                    'bound_examples':next_examples,'excluded_represented_facts_by_phase':{p:{str(i):v for i,v in m.items()} for p,m in source_issues.items()},
                                    'facts_with_rows_both_sides_cutoff':len(source_represented[PHASES[1]] & source_represented[PHASES[2]]),
                                    'distinct_plans':len(counts_by_plan),'plans_shared_by_multiple_subjects':sum(len(s)>1 for s in subjects_per_plan.values()),
                                    'max_source_changes_per_plan':max(counts_by_plan.values(),default=0),
                                    'subject_plan_pairs_with_multiple_distinct_changed_stamps':sum(n>1 for n in changes_by_subject_plan.values()),
                                    'formal_fact_issues_nonexclusive':dict(tally),'whole_trace_fact_issues_nonexclusive':dict(reasons_all),
                                    'callback_issues':dict(callback_bad)},
            'source_equal_weight':sources, 'observed_commit_ready_order_per_uniquely_keyed_row':dict(order_count),
            'join_issue_examples':dict(examples)}


def load(path):
    with (gzip.open(path,'rt') if str(path).endswith('.gz') else open(path,encoding='utf8')) as f:return json.load(f)


if __name__=='__main__':
    if len(sys.argv)!=3:raise SystemExit('usage: python3 analyze.py TRACE[.gz] REPORT[.gz]')
    out=analyze(load(Path(sys.argv[1])),load(Path(sys.argv[2])))
    json.dump(out,sys.stdout,sort_keys=True,separators=(',',':'));print();sys.exit(0 if out['ok'] else 2)
