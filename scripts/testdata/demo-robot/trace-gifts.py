"""After the loadtest and metrics snapshot only: read from this fresh run's local GM.
Never send a business action. Retain the gift filenames for existing tooling.
"""
import json
import os
from pathlib import Path
import sys
import time
from http.client import HTTPException
from urllib.error import HTTPError
from urllib.request import Request, build_opener, ProxyHandler, HTTPRedirectHandler

from poll_evidence import ID, STATES, TERMINAL, TYPES, failed_polls, positive


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def compact(record, allowed_types, ident=None):
    if (not ID.fullmatch(record['id']) or (ident and record['id'] != ident) or
            record['type'] not in allowed_types or record['status'] not in STATES):
        raise ValueError('invalid coordinator record')
    state = json.loads(record['data'])
    fields = {'sender': state['from'], 'recipient': state['to']} if record['type'] == 'gift_item' else {'owner': state['player_id']}
    if not all(positive(v) for v in fields.values()):
        raise ValueError('invalid participant')
    return dict(fields, saga_type=record['type'], status=record['status'])


READ_ERRORS = (HTTPException, OSError, KeyError, ValueError, TypeError)


def error_label(err):
    return f'http_{err.code}' if isinstance(err, HTTPError) else type(err).__name__


def main():
    out, port = Path(sys.argv[1]), int(sys.argv[2])
    failures = failed_polls((out / 'robot.log').read_text(errors='replace'))
    active = {r['saga_record']: r for r in failures if r['saga_record'] and not r.get('ambiguous')}
    opener = build_opener(ProxyHandler({}), NoRedirect())
    start = time.monotonic()
    deadline = start + 125

    def read(name, payload):
        req = Request(f'http://127.0.0.1:{port}/admin/execute', data=json.dumps({
            'name': name, 'operator': 'local-robot-readback', 'payload': payload}).encode(),
            headers={'Content-Type': 'application/json', 'X-Admin-Token': os.getenv('DEMO_ROBOT_ADMIN_TOKEN', 'dev-gm-token')})
        remaining = deadline - time.monotonic()
        if remaining <= 0:
            raise TimeoutError('read window ended')
        with opener.open(req, timeout=min(3, remaining)) as resp:
            envelope = json.load(resp)
        if envelope['ok'] is not True or envelope['name'] != name:
            raise ValueError('invalid GM response')
        return envelope['data']

    paths = (out / 'gift-snapshot.json', out / 'gift-followup.jsonl')
    if any(p.exists() for p in paths):
        raise FileExistsError('refusing to replace readback evidence')
    with paths[0].open('x') as snap, paths[1].open('x') as stream:
        # One bounded list, never an exhaustive inventory at 200 (two gifts/robot).
        # Reuse exact matches; a capped list miss requires a GET, not a verdict.
        try:
            listed = read('gm.saga.list', {'type': 'gift_item', 'limit': 200})['sagas']
            snapshot = [dict(compact(r, {'gift_item'}), id=r['id']) for r in listed]
            if len(snapshot) > 200 or len({r['id'] for r in snapshot}) != len(snapshot):
                raise ValueError('invalid list')
            for record in snapshot:
                row = active.get(record['id'])
                if row and row['action'] == 'gift_status' and record['status'] in TERMINAL:
                    del active[record['id']]
        except READ_ERRORS as err:
            snapshot = {'read_error': error_label(err)}
        print(json.dumps(snapshot), file=snap, flush=True)
        if isinstance(snapshot, dict) and snapshot['read_error'] in ('http_401', 'http_403'):
            return
        # Serial, at most three GETs per unresolved ID. Leave time for a late
        # transition without hammering the partition owner during recovery.
        for offset in (0, 30, 90):
            if not active or time.monotonic() >= deadline:
                break
            time.sleep(max(0, start + offset - time.monotonic()))
            for ident, row in list(active.items()):
                if time.monotonic() >= deadline:
                    break
                item = dict(row)
                try:
                    record = read('gm.saga.get', {'id': ident})
                    fields = compact(record, {row['expected_type']} if 'expected_type' in row else TYPES[row['action']], ident)
                    if 'authenticated_player' in row and fields['owner'] != row['authenticated_player']:
                        raise ValueError('record owner mismatch')
                    item.update(fields, **{k: record[k] for k in ('phase', 'step', 'completed_steps', 'attempt', 'updated_at')})
                    if fields['status'] in TERMINAL:
                        del active[ident]
                except READ_ERRORS as err:
                    item['read_error'] = error_label(err)
                item['elapsed_s'] = round(time.monotonic() - start, 3)
                print(json.dumps(item), file=stream, flush=True)
                if item.get('read_error') in ('http_401', 'http_403'):
                    return
                if active:
                    time.sleep(0.1)


if __name__ == '__main__':
    main()
