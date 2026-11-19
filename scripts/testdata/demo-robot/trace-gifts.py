"""After the measured window only: follow failed gift polls on this run's local GM.
Never retry a gift, never print request credentials, never change the runner verdict.
"""
import json
import os
from pathlib import Path
import re
import sys
import time
from urllib.request import Request, build_opener, ProxyHandler, HTTPRedirectHandler


class NoRedirect(HTTPRedirectHandler):
    def redirect_request(self, *args):
        return None


def failed_polls(log):
    pattern = r'scenario failed robot=(\d+) player=(\d+).*gift saga still (\w+) \(step (\d+)\) saga_record=([0-9a-f]{32})'
    return {m[5]: {'robot': int(m[1]), 'provisional_player': int(m[2]), 'last_status': m[3], 'last_step': int(m[4])}
            for m in re.finditer(pattern, log)}


def main():
    out, port = Path(sys.argv[1]), int(sys.argv[2])
    attempts = failed_polls((out / 'robot.log').read_text())
    opener = build_opener(ProxyHandler({}), NoRedirect())
    terminal = {'completed', 'compensated', 'failed', 'manual_required'}
    start = time.monotonic()
    def read(name, payload):
        req = Request(f'http://127.0.0.1:{port}/admin/execute', data=json.dumps({
            'name': name, 'operator': 'local-robot-readback', 'payload': payload}).encode(),
            headers={'Content-Type': 'application/json', 'X-Admin-Token': os.getenv('DEMO_ROBOT_ADMIN_TOKEN','dev-gm-token')})
        with opener.open(req, timeout=3) as resp:
            return json.load(resp)['data']

    # Even a fully passing run has useful terminal-state evidence. This snapshot
    # is bounded at 200 (not an exhaustive count if running larger scenarios).
    try:
        listed = read('gm.saga.list', {'type':'gift_item', 'limit':200})['sagas']
        snapshot = [{'id':r['id'], 'status':r['status'], 'recipient':json.loads(r['data'])['to']} for r in listed]
    except (OSError, KeyError, ValueError, TypeError) as err:
        snapshot = {'read_error':type(err).__name__}
    (out / 'gift-snapshot.json').write_text(json.dumps(snapshot))
    with (out / 'gift-followup.jsonl').open('x') as stream:
        active = set(attempts)
        # The scenario deadline is 2 minutes. A missing fact at the cutoff is unknown.
        while active and time.monotonic() - start < 125:
            for ident in sorted(active):
                if time.monotonic() - start >= 125:
                    break
                item = dict(attempts[ident], saga_record=ident, elapsed_s=round(time.monotonic()-start, 3))
                try:
                    record = read('gm.saga.get', {'id': ident})
                    if record['id'] != ident or record['type'] != 'gift_item':
                        raise ValueError('record identity mismatch')
                    state = json.loads(record['data'])
                    item.update(sender=state['from'], recipient=state['to'], **{key:record[key] for key in
                                ('status','phase','step','completed_steps','attempt','updated_at','last_error')})
                    if record['status'] in terminal:
                        active.remove(ident)
                except (OSError, KeyError, ValueError, TypeError) as err:
                    item['read_error'] = type(err).__name__
                print(json.dumps(item), file=stream, flush=True)
            if active:
                time.sleep(2)


if __name__ == '__main__':
    main()
