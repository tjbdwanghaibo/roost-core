"""Offline contract for post-run readback. Run: python3 -m unittest discover -s scripts/testdata/demo-robot."""
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

spec = importlib.util.spec_from_file_location('trace_gifts', Path(__file__).with_name('trace-gifts.py'))
trace = importlib.util.module_from_spec(spec)
spec.loader.exec_module(trace)


class TraceGiftsTest(unittest.TestCase):
    def test_follows_the_logged_record_not_a_provisional_player_id(self):
        ident = 'a' * 32
        log = f'WARN robot runner: scenario failed robot=35 player=900035 err="robot action gift_status: gift saga still waiting (step 1) saga_record={ident}"\n'
        calls = []
        record = dict(id=ident, type='gift_item', phase='forward', step=1, status='waiting', data='{"from":100082,"to":1}', completed_steps=1, attempt=1, updated_at='now', last_error='')

        def response(req, timeout):
            cmd = json.loads(req.data)
            calls.append(cmd['name'])
            if cmd['name'] == 'gm.saga.list':
                data = {'sagas':[record.copy()]}
            else:
                self.assertEqual(cmd['payload'], {'id':ident})
                if calls.count('gm.saga.get') == 2:
                    record.update(status='compensated', phase='compensate')
                data = record
            return io.BytesIO(json.dumps({'data':data}).encode())

        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            (path / 'robot.log').write_text(log * 2)
            with mock.patch.object(trace.sys, 'argv', ['', tmp, '9100']), mock.patch.object(trace, 'build_opener') as opener, mock.patch.object(trace.time, 'sleep'):
                opener.return_value.open.side_effect = response
                trace.main()
            rows = [json.loads(row) for row in (path / 'gift-followup.jsonl').read_text().splitlines()]
            self.assertEqual(calls, ['gm.saga.list', 'gm.saga.get', 'gm.saga.get'])
            self.assertEqual([r['status'] for r in rows], ['waiting', 'compensated'])
            self.assertEqual((rows[-1]['provisional_player'], rows[-1]['sender']), (900035, 100082))
            self.assertEqual((path / 'robot.log').read_text(), log * 2)

    def test_old_or_unrelated_failures_have_no_invented_id(self):
        self.assertEqual(trace.failed_polls('scenario failed robot=35 player=900035 err="gift saga still waiting (step 1)"'), {})
