"""Offline contract for post-run readback. Run: python3 -m unittest discover -s scripts/testdata/demo-robot."""
import importlib.util
import io
import json
from pathlib import Path
import tempfile
import unittest
from unittest import mock

from poll_evidence import failed_polls, summarize

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
            return io.BytesIO(json.dumps({'name': cmd['name'], 'ok': True, 'data':data}).encode())

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
            outcome, = summarize(path)['failures']
            self.assertEqual((outcome['outcome'], outcome['observed_status']), ('terminal_expected', 'compensated'))

    def test_old_rows_missing_ids_and_bad_ids_are_explicitly_unknown(self):
        def failure(i, action, subject, suffix=''):
            return f'WARN robot runner: scenario failed robot={i} player=900035 scenario=demo err="robot action {action}: {subject} saga still waiting (step 1){suffix}"'
        lines = [failure(1, 'gift_status', 'gift'), failure(2, 'alliance_op_status', 'alliance'),
                 failure(3, 'gift_status', 'gift', ' saga_record=' + 'a'*33),
                 failure(4, 'gift_status', 'gift', ' saga_record=' + 'b'*32),
                 failure(5, 'gift_status', 'gift', ' saga_record=' + 'b'*32)]
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            self.assertIsNone(summarize(path)['failures'])
            (path/'robot.log').write_text('\n'.join(lines + lines[:1]))
            self.assertEqual([r['reason'] for r in summarize(path)['failures']],
                             ['missing_record_id']*3 + ['ambiguous_failure_identity']*2)
        self.assertFalse(failed_polls('INFO unrelated gift saga still pending'))

    def test_readback_outcomes_and_cutoff(self):
        ident = 'b'*32
        log = f'WARN robot runner: scenario failed robot=2 player=900002 scenario=demo err="robot action gift_status: gift saga still waiting (step 1) saga_record={ident}"'
        base = dict(saga_record=ident, robot=2, sender=100050, recipient=1)
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            (path/'robot.log').write_text(log)
            (path/'gift-snapshot.json').write_text(json.dumps([dict(id='a'*32, status='completed', recipient=1)]))
            def actual():
                return summarize(path)['failures'][0]
            self.assertEqual(actual()['reason'], 'no_correlated_readback')
            file = path/'gift-followup.jsonl'
            for changes, field, value in [({'status':'pending'}, 'reason', 'last_observed_nonterminal'),
                    ({'status':'completed'}, 'outcome', 'terminal_unexpected'),
                    ({'status':'manual_required'}, 'outcome', 'terminal_unexpected'),
                    ({'status':'failed'}, 'outcome', 'terminal_unexpected'),
                    ({'status':'completed', 'recipient':100050}, 'outcome', 'terminal_expected'),
                    ({'status':'unexpected'}, 'reason', 'invalid_readback'),
                    ({'status':'completed', 'saga_type':'alliance_found'}, 'reason', 'invalid_readback'),
                    ({'status':'compensated', 'robot':9}, 'reason', 'read_error'),
                    ({'read_error':'http_500'}, 'reason', 'read_error')]:
                with self.subTest(changes=changes):
                    file.write_text(json.dumps(dict(base, **changes))+'\n')
                    self.assertEqual(actual()[field], value)
            file.write_text(json.dumps(dict(base, status='completed'))+'\n{')
            self.assertEqual(actual()['outcome'], 'unknown')
            self.assertTrue(summarize(path)['issues'])

    def test_matching_terminal_snapshot_and_no_overwrite(self):
        ident = 'a'*32
        log = f'WARN robot runner: scenario failed robot=7 player=900007 scenario=demo err="robot action gift_status: gift saga still pending (step 1) saga_record={ident}"'
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            (path/'robot.log').write_text(log)
            record = dict(id=ident, type='gift_item', status='compensated', data='{"from":100200,"to":1}', business_key='private')
            reply = {'name':'gm.saga.list', 'ok':True, 'data':{'sagas':[record]}}
            with mock.patch.object(trace.sys, 'argv', ['', tmp, '9100']), mock.patch.object(trace, 'build_opener') as opener:
                opener.return_value.open.return_value = io.BytesIO(json.dumps(reply).encode())
                trace.main()
                opener.return_value.open.assert_called_once()
                saved = (path/'gift-snapshot.json').read_bytes()
                self.assertNotIn(b'private', saved)
                self.assertEqual(summarize(path)['counts'], {'terminal_expected':1})
                with self.assertRaises(FileExistsError):
                    trace.main()
                self.assertEqual((path/'gift-snapshot.json').read_bytes(), saved)
                opener.return_value.open.assert_called_once()

    def test_missing_alliance_id_does_not_borrow_another_operations_result(self):
        ident = 'b'*32
        def warning(i, suffix):
            return f'WARN robot runner: scenario failed robot={i} player={900000+i} scenario=demo err="robot action alliance_op_status: alliance saga still waiting (step 1){suffix}"'
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            (path/'robot.log').write_text(warning(3, ' saga_record='+ident+' saga_type=alliance_donate saga_player=100017')+'\n'+warning(4, ''))
            (path/'gift-followup.jsonl').write_text(json.dumps(dict(saga_record=ident, robot=3,
                saga_type='alliance_donate', status='completed', owner=100017)))
            rows = summarize(path)['failures']
            self.assertEqual([r['outcome'] for r in rows], ['terminal_expected','unknown'])
            self.assertEqual(rows[1]['reason'], 'missing_record_id')
            for kind, owner in [('alliance_found', 100017), ('alliance_donate', 900003)]:
                (path/'gift-followup.jsonl').write_text(json.dumps(dict(saga_record=ident, robot=3,
                    saga_type=kind, status='completed', owner=owner)))
                self.assertEqual(summarize(path)['failures'][0]['outcome'], 'unknown')

    def test_get_window_is_bounded_and_read_failure_not_terminal(self):
        ident = 'a'*32
        log = f'WARN robot runner: scenario failed robot=7 player=900007 scenario=demo err="robot action alliance_op_status: alliance saga still waiting (step 1) saga_record={ident} saga_type=alliance_donate saga_player=100017"'
        with tempfile.TemporaryDirectory() as tmp:
            path = Path(tmp)
            (path/'robot.log').write_text(log)
            calls = []
            def response(req, timeout):
                cmd = json.loads(req.data)
                calls.append(cmd['name'])
                self.assertEqual(req.full_url, 'http://127.0.0.1:9100/admin/execute')
                if cmd['name'] == 'gm.saga.list':
                    data = {'sagas': []}
                else:
                    self.assertEqual(cmd['payload'], {'id':ident})
                    # A gift with identical ID is NOT this alliance op.
                    data = dict(id=ident, type='gift_item', status='completed', data='{"from":1,"to":1}')
                return io.BytesIO(json.dumps({'ok':True,'name':cmd['name'],'data':data}).encode())
            with mock.patch.object(trace.sys, 'argv', ['', tmp, '9100']), mock.patch.object(trace, 'build_opener') as opener, mock.patch.object(trace.time, 'sleep'):
                opener.return_value.open.side_effect = response
                trace.main()
            self.assertEqual(calls, ['gm.saga.list'] + ['gm.saga.get']*3)
            self.assertEqual(summarize(path)['failures'][0]['reason'], 'read_error')
