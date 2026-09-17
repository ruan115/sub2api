"""Cleanup contract with in-memory records; no filesystem removal or Docker."""

import copy
import io
import json
from pathlib import Path
import subprocess
import unittest
from unittest.mock import patch

from lab import cleanup as module
from lab.host import LABEL, Lab


class FakeLab:
    # Exercise the production exact-ID and ownership validation, not a stub
    # which would silently accept container names or foreign labels.
    owned = Lab.owned

    def __init__(self):
        self.root = Path("/synthetic/private-lab")
        self.name = "isthmus-s1b-synthetic"
        self.volume = self.name + "-buildkit-cache"
        self.ids = [character * 64 for character in "abcd"]
        self.records = {name + "-container.json": {"id": cid} for name, cid in zip(
            ("prepare", "builder", "smoke", "default"), self.ids)}
        self.original_baseline = [{"id": "e" * 64, "name": "/existing-business",
                                   "image": "sha256:" + "f" * 64, "status": "running"}]
        self.records.update({"builder-volume.json": {"name": self.volume},
                             "baseline.json": self.original_baseline,
                             "built-image.json": {"id": "sha256:" + "1" * 64},
                             "public-evidence.json": {"preserve": True}})
        self.containers = {cid: {"Id": cid, "Config": {"Labels": {LABEL: self.name}}}
                           for cid in self.ids}
        self.volume_record = {"Name": self.volume, "Labels": {LABEL: self.name}}
        self.current_baseline = copy.deepcopy(self.original_baseline)
        self.events = []
        self.saved = {}
        self.volume_inspections = 0
        self.second_volume_owner = None
        self.change_owner_after_stop = None
        self.fail_operation = None

    def exists(self, path):
        if path.parent != self.root:
            raise AssertionError("unexpected record directory")
        return path.name in self.records

    def read_text(self, path):
        if path.parent != self.root:
            raise AssertionError("unexpected evidence path")
        return json.dumps(self.records[path.name])

    def json(self, *args):
        self.events.append(("inspect", args))
        if len(args) == 2 and args[0] == "inspect":
            return [copy.deepcopy(self.containers[args[1]])]
        if args == ("volume", "inspect", self.volume):
            self.volume_inspections += 1
            item = copy.deepcopy(self.volume_record)
            if self.volume_inspections > 1 and self.second_volume_owner is not None:
                item["Labels"][LABEL] = self.second_volume_owner
            return [item]
        raise AssertionError("unexpected synthetic inspect")

    def run(self, *args):
        self.events.append(("mutation", args))
        if self.fail_operation == args[0]:
            raise RuntimeError("synthetic_cleanup_operation_failed")
        if len(args) == 4 and args[:3] == ("stop", "--time", "5"):
            if args[3] == self.change_owner_after_stop:
                self.containers[args[3]]["Config"]["Labels"][LABEL] = "foreign-run"
        elif len(args) == 2 and args[0] == "rm" and args[1] in self.containers:
            del self.containers[args[1]]
        elif args == ("volume", "rm", self.volume):
            pass
        else:
            # Any image removal, prune, unknown target or extra flag fails.
            raise AssertionError("unexpected cleanup mutation")
        return subprocess.CompletedProcess(args, 0, "", "")

    def baseline(self):
        self.events.append(("baseline",))
        return copy.deepcopy(self.current_baseline)

    def save(self, name, value):
        self.events.append(("save", name))
        self.saved[name] = copy.deepcopy(value)


class LabCleanupTests(unittest.TestCase):
    def setUp(self):
        self.lab = FakeLab()
        self.output = io.StringIO()

    def cleanup(self):
        with patch.object(Path, "exists", autospec=True, side_effect=self.lab.exists), \
             patch.object(Path, "read_text", autospec=True, side_effect=self.lab.read_text), \
             patch("sys.stdout", self.output):
            module.cleanup(self.lab)

    def mutations(self):
        return [event[1] for event in self.lab.events if event[0] == "mutation"]

    def assert_no_success(self):
        self.assertNotIn("cleanup.json", self.lab.saved)
        self.assertNotIn("owned_resources_removed", self.output.getvalue())

    def test_every_target_is_verified_before_first_mutation_and_only_recorded_resources_removed(self):
        evidence = copy.deepcopy(self.lab.records)
        self.cleanup()
        first_mutation = next(index for index, event in enumerate(self.lab.events)
                              if event[0] == "mutation")
        preflight = self.lab.events[:first_mutation]
        for cid in self.lab.ids:
            self.assertIn(("inspect", ("inspect", cid)), preflight)
        self.assertIn(("inspect", ("volume", "inspect", self.lab.volume)), preflight)
        self.assertIn(("save", "cleanup-targets.json"), preflight)
        expected = []
        for cid in self.lab.ids:
            expected.extend([("stop", "--time", "5", cid), ("rm", cid)])
        expected.append(("volume", "rm", self.lab.volume))
        self.assertEqual(self.mutations(), expected)
        self.assertEqual(self.lab.records, evidence)
        self.assertEqual(self.lab.saved["cleanup.json"], {
            "removed_containers": 4, "removed_cache_volumes": 1,
            "existing_containers_unchanged": True, "images_and_evidence_preserved": True})

    def test_last_foreign_container_prevents_removing_any_earlier_target(self):
        self.lab.containers[self.lab.ids[-1]]["Config"]["Labels"][LABEL] = "foreign-run"
        with self.assertRaisesRegex(ValueError, "container_ownership_mismatch"):
            self.cleanup()
        self.assertEqual(self.mutations(), [])
        self.assertNotIn("cleanup-targets.json", self.lab.saved)
        self.assert_no_success()

    def test_record_must_have_exact_container_id_not_name_or_short_id(self):
        for value in ("synthetic-container", "a" * 12, "A" * 64):
            with self.subTest(value=value):
                self.lab = FakeLab()
                self.lab.records["default-container.json"]["id"] = value
                with self.assertRaisesRegex(ValueError, "exact_container_id_required"):
                    self.cleanup()
                self.assertEqual(self.mutations(), [])
                self.assert_no_success()

    def test_foreign_volume_name_inspected_name_or_label_prevents_any_mutation(self):
        for change in ("record_name", "inspect_name", "owner"):
            with self.subTest(change=change):
                self.lab = FakeLab()
                if change == "record_name":
                    self.lab.records["builder-volume.json"]["name"] = "business-data"
                elif change == "inspect_name":
                    self.lab.volume_record["Name"] = "business-data"
                else:
                    self.lab.volume_record["Labels"][LABEL] = "foreign-run"
                with self.assertRaisesRegex(ValueError, "cleanup_volume_.*mismatch"):
                    self.cleanup()
                self.assertEqual(self.mutations(), [])
                self.assert_no_success()

    def test_owner_changed_after_stop_is_rechecked_before_container_removal(self):
        self.lab.change_owner_after_stop = self.lab.ids[0]
        with self.assertRaisesRegex(ValueError, "container_ownership_mismatch"):
            self.cleanup()
        self.assertEqual(self.mutations(), [("stop", "--time", "5", self.lab.ids[0])])
        self.assert_no_success()

    def test_volume_owner_change_before_removal_cannot_remove_volume_or_report_success(self):
        self.lab.second_volume_owner = "foreign-run"
        with self.assertRaisesRegex(ValueError, "cleanup_volume_owner_changed"):
            self.cleanup()
        self.assertNotIn(("volume", "rm", self.lab.volume), self.mutations())
        self.assertEqual(len(self.mutations()), 8)
        self.assert_no_success()

    def test_cleanup_operation_failure_stops_without_success_or_unrelated_fallback(self):
        self.lab.fail_operation = "stop"
        with self.assertRaisesRegex(RuntimeError, "synthetic_cleanup_operation_failed"):
            self.cleanup()
        self.assertEqual(self.mutations(), [("stop", "--time", "5", self.lab.ids[0])])
        self.assert_no_success()

    def test_changed_existing_business_baseline_never_reports_pass(self):
        self.lab.current_baseline[0]["status"] = "exited"
        with self.assertRaisesRegex(ValueError, "existing_container_baseline_changed"):
            self.cleanup()
        self.assertEqual(len(self.mutations()), 9)
        self.assert_no_success()

    def test_absent_resource_records_do_not_expand_cleanup_scope(self):
        for name in ("prepare", "builder", "smoke", "default"):
            del self.lab.records[name + "-container.json"]
        del self.lab.records["builder-volume.json"]
        self.cleanup()
        self.assertEqual(self.mutations(), [])
        self.assertEqual(self.lab.saved["cleanup-targets.json"], {"containers": [], "volume": None})
        self.assertEqual(self.lab.saved["cleanup.json"]["removed_containers"], 0)
        self.assertEqual(self.lab.saved["cleanup.json"]["removed_cache_volumes"], 0)


if __name__ == "__main__":
    unittest.main()
