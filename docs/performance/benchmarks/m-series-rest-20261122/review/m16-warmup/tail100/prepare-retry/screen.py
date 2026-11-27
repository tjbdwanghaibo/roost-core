"""Pre-timing screen for lazy revision-map candidate only. A hint selects representation, never an early failure."""
from collections import Counter
from bisect import bisect_right

def eligibility(t):
 lo,hi=t['formal_ns'],t['end_ns'];rs={x[0]:x for x in t['server']['rounds']};groups={k:Counter() for k in ['all','formal','first','second']};by_last={};prev_reason={};mid=(lo+hi)//2
 for w in t['server']['round_work']:
  rid=w['round'];_,a,b,empty,ok,force=rs[rid];sc=['all']+(['formal']+(['first'] if b<=mid else ['second'] if a>=mid else []) if ok and not force and lo<=a<=b<=hi else []);up={};cs=w.get('meter_costs') or [];cnt=Counter();revs=Counter();fit=Counter();fittedwall=Counter();worstentries=Counter();
  for (id,x,y,called,n),v in zip(w['objects'] or [],w.get('meter_codes') or []):
   k=v&15;rr=(v>>5)&3
   if called:
    if by_last.get(id,False):
     fit[k]+=1;fittedwall[k]+=y-x;worstentries[k]+=n
     for g in sc:
      h=groups[g];h['hint_calls']+=1;h['hint_dense_calls']+=n>=65;h['hint_pending_again']+=k in (1,2);h['hint_success_dirty_or_clean']+=k in (7,8);h['hint_all_input_subs']+=n;h['hint_pending_dense_calls']+=n>=65 and k in (1,2)
    # Real mark only after this Prepare; no-Prepare work doesn't invent a failure or success.
    by_last[id]=rr==1
   cnt[k]+=1
  for row in cs:
   k,n,revs,depth,mx,body,api,pre,dec,copy,*rest=row;assert n==cnt[k]
   for g in sc:
    h=groups[g];h['all_object_ns']+=body;h['all_revision_copy_ns']+=copy
    if k in (1,2):h['pending_object_ns']+=body;h['pending_revision_copy_ns']+=copy;h['pending_calls']+=n
    # No per-call revision clocks: if mixed, bound by observed total class copy AND eligible object wall.
    if k in (1,2) and fit[k]:
     upper=min(fittedwall[k],copy);h['rejected_hint_revision_copy_upper_ns']+=upper
     if fit[k]==n:h['rejected_hint_revision_copy_exact_lower_ns']+=copy
 for h in groups.values():
  h['upper_share_all_object']=h['rejected_hint_revision_copy_upper_ns']/h['all_object_ns'] if h['all_object_ns'] else 0;h['lower_share_all_object']=h['rejected_hint_revision_copy_exact_lower_ns']/h['all_object_ns'] if h['all_object_ns'] else 0
 # Gate for running a local prototype, NOT an end-to-end guarantee: do not pursue a tiny isolated target.
 # Enough repeated high-fanout rejects, some in both halves; aggregate upper below 5% of actual capture-body time rules out this direction on the measured schedule.
 f=groups['formal'];checks={'both_halves_repeated_dense':all(groups[g]['hint_pending_dense_calls']>0 for g in ('first','second')),'at_least_5pct_frozen_body_upper':f['upper_share_all_object']>=.05,'dominant_outcome_on_hint_is_pending':f['hint_pending_again']>f['hint_calls']/2};return dict(groups={k:dict(v) for k,v in groups.items()},conditions=checks,worth_fixed=all(checks.values()),meaning='Hint is from the last actual Prepare commit_pending and a current Prepare is mandatory; subject ID stable within this fixture. Upper assumes entire revision-copy stage of every eligible rejected call disappears and ignores cost of new snapshots; only a same-schedule screen, not end-to-end savings.')
