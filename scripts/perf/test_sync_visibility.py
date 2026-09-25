import importlib.util
from pathlib import Path
import unittest

spec = importlib.util.spec_from_file_location("visibility", Path(__file__).with_name("sync-visibility.py"))
module = importlib.util.module_from_spec(spec)
spec.loader.exec_module(module)


def event(stage, at, subject=0, epoch=2, recovery=False):
    return dict(Stage=stage, At=at * 1_000_000, Session=1, Epoch=epoch,
                Lifetime=1, SubjectID=subject, Snapshot=recovery)


def receipt(at, subject=1, epoch=2, operation=1):
    return dict(At=at * 1_000_000, Session=1, Epoch=epoch, SubjectID=subject, Operation=operation)


class VisibilityTests(unittest.TestCase):
    def fixture(self):
        return [event("measurement_started", 1), event("recovery_started", 10),
                event("session_reset", 11), event("baseline_requested", 12, 1, recovery=True),
                event("baseline_requested", 13, 2, recovery=True)]

    def test_all_received_uses_last_client_not_admission(self):
        r = module.summarize(self.fixture(), [receipt(20), receipt(45, 2)])
        self.assertTrue(r["complete_evidence"])
        self.assertEqual(r["recovery_cohort_all_received_ms"], 35)
        self.assertEqual(r["recovery_baselines"]["request_to_client_ms"]["max"], 32)

    def test_cancelled_is_not_received_and_reentry_is_new_visibility(self):
        events = self.fixture() + [event("baseline_cancelled", 18, 2),
                                  event("baseline_requested", 22, 2),
                                  event("baseline_requested", 25, 2)]
        r = module.summarize(events, [receipt(20), receipt(30, 2)])
        self.assertIsNone(r["recovery_cohort_all_received_ms"])
        self.assertEqual(r["recovery_cohort_settled_ms"], 10)
        self.assertEqual(r["new_visibility"]["request_to_client_ms"]["max"], 8)
        self.assertEqual(r["recovery_baselines"]["cancelled"], 1)

    def test_missing_or_dropped_evidence_never_reports_complete(self):
        for overwritten, omitted in ((1, 0), (0, 1)):
            r = module.summarize(self.fixture(), [receipt(20), receipt(30, 2)], overwritten, omitted)
            self.assertFalse(r["complete_evidence"])
            self.assertIsNone(r["recovery_cohort_all_received_ms"])
        r = module.summarize(self.fixture(), [receipt(20)])
        self.assertEqual(r["recovery_baselines"]["pending"], 1)
        self.assertIsNone(r["recovery_cohort_settled_ms"])

    def test_inflight_create_after_cancellation_is_not_missing_or_recovered(self):
        events = self.fixture() + [event("baseline_cancelled", 18, 2)]
        r = module.summarize(events, [receipt(20), receipt(30, 2)])
        self.assertTrue(r["complete_evidence"])
        self.assertEqual(r["creates_received_after_cancellation"], 1)
        self.assertEqual(r["recovery_baselines"]["received"], 1)
        self.assertEqual(r["recovery_baselines"]["cancelled"], 1)
        self.assertIsNone(r["recovery_cohort_all_received_ms"])

    def test_old_epoch_cannot_satisfy_new_recovery(self):
        r = module.summarize(self.fixture(), [receipt(20, epoch=1)])
        self.assertEqual(r["recovery_baselines"]["pending"], 2)
        self.assertEqual(r["unmatched_creates"], 1)
        self.assertFalse(r["complete_evidence"])

    def test_remove_reentry_creates_second_measurement(self):
        events = [event("measurement_started", 1), event("baseline_requested", 2, 1),
                  event("baseline_requested", 6, 1)]
        r = module.summarize(events, [receipt(3), receipt(4, operation=3), receipt(8)])
        self.assertEqual(r["new_visibility"]["received"], 2)
        self.assertEqual(r["new_visibility"]["request_to_client_ms"]["max"], 2)


if __name__ == "__main__":
    unittest.main()
