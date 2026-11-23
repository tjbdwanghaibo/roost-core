"""Classify *logged* final demo status-poll failures, never the loadtest verdict.
Offline: python3 poll_evidence.py /path/to/one/fresh/run (stdout only).
"""
from collections import Counter
import json
from pathlib import Path
import re
import sys

TERMINAL = {'completed', 'compensated', 'failed', 'manual_required'}
STATES = TERMINAL | {'pending', 'waiting', 'compensating'}
TYPES = {'gift_status': {'gift_item'},
         'alliance_op_status': {'alliance_found', 'alliance_donate', 'alliance_reward'}}
ID = re.compile(r'[0-9a-f]{32}')
FAIL = re.compile(r'\bWARN robot runner: scenario failed robot=(\d+) player=(\d+) '
                  r'(?:scenario=demo )?err="robot action (gift_status|alliance_op_status): '
                  r'(gift|alliance) saga still (unknown|waiting|pending|compensating) \(step (\d+)\)([^"\n]*)"$')


def failed_polls(log):
    """One runner failure per robot, not N outcomes for N retries/log copies."""
    failures = {}
    for line, text in enumerate(log.splitlines(), 1):
        m = FAIL.search(text)
        if not m or (m[3] == 'gift_status') != (m[4] == 'gift'):
            continue
        ids = re.findall(r'(?:^| )saga_record=(\S+)', m[7])
        ident = ids[0] if len(ids) == 1 and ID.fullmatch(ids[0]) else None
        robot = int(m[1])
        row = dict(robot=robot, provisional_player=int(m[2]), action=m[3], last_status=m[5],
                   last_step=int(m[6]), saga_record=ident)
        if m[3] == 'alliance_op_status' and ident:
            # Only the attested loadtest format: exact request + authenticated owner.
            attested = re.fullmatch(r' saga_record=[0-9a-f]{32} saga_type=(alliance_(?:found|donate|reward)) saga_player=([1-9][0-9]{0,18})', m[7])
            if attested:
                row.update(expected_type=attested[1], authenticated_player=int(attested[2]))
            else:
                row['saga_record'] = None
        if robot not in failures:
            failures[robot] = dict(row, robot_log_line=line)
        elif any(failures[robot].get(k) != v for k, v in row.items()):
            failures[robot]['ambiguous'] = True
    counts = Counter(row['saga_record'] for row in failures.values())
    for row in failures.values():
        if row['saga_record'] and counts[row['saga_record']] != 1:
            row['ambiguous'] = True
    return list(failures.values())


def positive(value):
    return type(value) is int and value > 0


def classify(row, obs):
    """No joins on provisional_player, list order, timing, step or aggregate counts."""
    if row.get('ambiguous'):
        return dict(outcome='unknown', reason='ambiguous_failure_identity')
    if not row['saga_record']:
        return dict(outcome='unknown', reason='missing_record_id')
    if obs is None:
        return dict(outcome='unknown', reason='no_correlated_readback')
    if 'read_error' in obs:
        return dict(outcome='unknown', reason='read_error')
    kind = obs.get('saga_type')
    status = obs.get('status')
    if (not isinstance(kind, str) or kind not in TYPES[row['action']] or
            not isinstance(status, str) or status not in STATES or
            (kind == 'gift_item' and not positive(obs.get('recipient'))) or
            (kind != 'gift_item' and (kind != row.get('expected_type') or not positive(obs.get('owner')) or
                                      obs['owner'] != row.get('authenticated_player')))):
        return dict(outcome='unknown', reason='invalid_readback')
    common = dict(observed_status=status, saga_type=kind, evidence=obs['evidence'])
    if status not in TERMINAL:
        return dict(common, outcome='unknown', reason='last_observed_nonterminal')
    expected = None
    if kind != 'gift_item' or obs.get('sender') == obs['recipient']:
        expected = 'completed'
    elif obs['recipient'] == 1:  # The deliberately absent recipient of demo's second gift.
        expected = 'compensated'
    outcome = ('terminal_observed' if expected is None else
               'terminal_expected' if status == expected else 'terminal_unexpected')
    return dict(common, outcome=outcome, expected_status=expected)


def summarize(out):
    log = out / 'robot.log'
    if not log.is_file():
        return {'state': 'no_robot_log', 'failures': None}
    rows = failed_polls(log.read_text(errors='replace'))
    by_id = {r['saga_record']: r for r in rows if r['saga_record'] and not r.get('ambiguous')}
    observations = {}
    issues = []
    snapshot = out / 'gift-snapshot.json'
    if by_id and snapshot.is_file():
        try:
            values = json.loads(snapshot.read_text())
            if not isinstance(values, list):
                raise ValueError('snapshot unavailable')
            seen = set()
            for entry in values:
                ident = entry['id']
                if ident not in by_id or by_id[ident]['action'] != 'gift_status':
                    continue
                if ident in seen:
                    raise ValueError('duplicate snapshot id')
                seen.add(ident)
                observations[ident] = dict(entry, saga_type=entry.get('saga_type', 'gift_item'), evidence=snapshot.name)
        except (ValueError, KeyError, TypeError):
            issues.append('gift_snapshot_unusable')
            observations.clear()
    # The original collector wrote gift-only rows without a saga_type. It verified
    # get's exact ID/type before persisting status + sender/recipient. Its list
    # was a bounded gift_item list; a missing entry has no negative meaning.
    followup = out / 'gift-followup.jsonl'
    if by_id and followup.is_file():
        with followup.open() as stream:
            for line, text in enumerate(stream, 1):
                try:
                    obs = json.loads(text)
                    ident = obs['saga_record']
                    if ident not in by_id:
                        continue
                    if (obs['robot'] != by_id[ident]['robot'] or
                            obs.get('action', by_id[ident]['action']) != by_id[ident]['action']):
                        observations[ident] = {'read_error': 'identity_mismatch'}
                        continue
                    observations[ident] = dict(obs, saga_type=obs.get('saga_type', 'gift_item'),
                                               evidence=f'{followup.name}:{line}')
                except (ValueError, KeyError, TypeError):
                    # An unattributable later observation could supersede an earlier
                    # one. Do not silently accept a truncated/invalid evidence file.
                    issues.append(f'followup_unusable_at_line_{line}')
                    observations.clear()
                    break
    for row in rows:
        row.update(classify(row, observations.get(row['saga_record'])))
    return dict(state='reported', scope='Final demo still-status runner warnings only. Terminal means observed at '
                'readback, not success in budget, a permanent state, or an independent ledger audit.',
                counts=dict(Counter(r['outcome'] for r in rows)), issues=issues, failures=rows)


if __name__ == '__main__':
    print(json.dumps(summarize(Path(sys.argv[1])), indent=2))
