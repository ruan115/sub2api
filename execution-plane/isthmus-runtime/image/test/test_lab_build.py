"""Synthetic Docker responses and /proc text; no daemon or host process reads."""

import copy
from contextlib import contextmanager
import hashlib
import io
from pathlib import Path
import subprocess
import unittest
from unittest.mock import Mock, patch

from lab import build as module
from lab.host import LABEL


CID = "a" * 64
IMAGE_ID = "sha256:" + "b" * 64
RECIPE = b"synthetic reviewed recipe, never built"


class FakeLab:
    def __init__(self):
        self.root = Path("/synthetic/private-lab")
        self.name = "isthmus-s1b-synthetic"
        self.saved = {"pull-builder-image.json": {"Id": IMAGE_ID}}
        self.events = []
        self.start_error = None
        self.stop_error = None
        self.negative = "rejected"
        self.negative_artifact_size = None
        self.loaded_paths = []
        self.command = None
        self.negative_log = 'error: target stage "missing-s1b-negative" could not be found'

    def save(self, name, value):
        if name in self.saved:
            raise FileExistsError("synthetic_evidence_exists")
        self.events.append(("save", name))
        self.saved[name] = copy.deepcopy(value)

    def owned(self, cid):
        if cid != CID:
            raise AssertionError("unexpected container")
        self.events.append(("owned", cid))
        return {
            "Image": IMAGE_ID,
            "HostConfig": {
                "Memory": 2147483648, "MemorySwap": 2147483648,
                "CpuQuota": 200000, "CpuPeriod": 100000,
                "RestartPolicy": {"Name": "no"}, "Privileged": True,
                "PidsLimit": 512, "Binds": None, "PortBindings": {},
                "NetworkMode": "bridge", "PidMode": "", "IpcMode": "private",
                "CgroupnsMode": "private",
            },
            "Mounts": [{"Type": "volume", "Name": self.name + "-buildkit-cache",
                        "Destination": "/var/lib/buildkit"}],
            "Config": {"Env": ["PATH=/usr/bin:/bin"], "Labels": {LABEL: self.name},
                       "Cmd": self.command, "Entrypoint": ["/bin/sh"]},
            "State": {"Pid": 101},
        }

    def run(self, *args, **kwargs):
        self.events.append(("run", args))
        output = ""
        if args[:2] in (("volume", "ls"), ("volume", "create"), ("image", "ls")):
            pass
        elif args[0] == "create":
            # Retain the actual build command for real validate_builder checks.
            self.command = list(args[args.index(module.BUILDKIT) + 1:])
            output = CID
        elif args[0] == "cp":
            pass
        elif args == ("start", CID):
            if self.start_error:
                raise self.start_error
        elif args == ("stop", "--time", "5", CID):
            if self.stop_error:
                raise self.stop_error
        elif args[:4] == ("exec", CID, "sh", "-ec"):
            if "memory.max" not in args[4]:
                raise AssertionError("unexpected shell command")
            output = "2147483648\n0\n200000 100000\n"
        elif args == ("exec", CID, "buildctl", "debug", "workers"):
            output = "synthetic worker linux/amd64\n"
        elif args == ("exec", CID, "sha256sum", "/tmp/isthmus-context/Dockerfile"):
            output = hashlib.sha256(RECIPE).hexdigest() + "  Dockerfile\n"
        elif args == ("exec", CID, "test", "!", "-s", "/tmp/isthmus-negative.tar"):
            if self.negative_artifact_size is not None and self.negative_artifact_size > 0:
                raise RuntimeError("synthetic_negative_artifact_nonempty")
        else:
            raise AssertionError("unexpected synthetic Docker call")
        return subprocess.CompletedProcess(args, 0, output, "")

    def json(self, *args):
        self.events.append(("json", args))
        if args == ("volume", "inspect", self.name + "-buildkit-cache"):
            return [{"Labels": {LABEL: self.name}}]
        if args == ("image", "inspect", "isthmus-vm-base-lab:" + self.name):
            return [{"Id": IMAGE_ID, "Architecture": "amd64",
                     "Config": {"User": "1000:1000", "Cmd": ["/bin/false"],
                                "Env": ["PATH=/usr/bin:/bin", "HOME=/home/claude",
                                        "USER=claude", "LOGNAME=claude"]}}]
        raise AssertionError("unexpected synthetic inspect")

    def logged(self, name, args, **kwargs):
        self.events.append(("logged", name))
        if name == "base-load":
            self.loaded_paths.append(args[-1])
        if name == "build-negative":
            if self.negative == "timeout":
                raise TimeoutError("synthetic_negative_timeout")
            if self.negative == "missing_receipt":
                raise RuntimeError("synthetic_negative_transport_failed")
            if self.negative == "rejected":
                self.save(name + ".json", {"exit_code": 1})
                raise RuntimeError("logged_command_failed:build-negative")
        self.save(name + ".json", {"exit_code": 0})


class BuildCleanupTests(unittest.TestCase):
    def setUp(self):
        self.lab = FakeLab()
        self.output = io.StringIO()
        self.observer = Mock(parent="/docker/synthetic", groups={"/docker/synthetic/child"},
                             commands={"dpkg"})

    def build(self):
        # Only context bytes and metadata are mocked; the workflow, builder
        # validation, negative branch, and finally ordering remain real code.
        with patch.object(module, "verify_context"), \
             patch.object(module, "read", side_effect=lambda lab, name: lab.saved[name]), \
             patch.object(module.Path, "read_bytes", return_value=RECIPE), \
             patch.object(module.Path, "read_text", side_effect=lambda: self.lab.negative_log), \
             patch.object(module, "CgroupObserver", return_value=self.observer), \
             patch("sys.stdout", self.output):
            module.build(self.lab)

    def assert_stopped_without_completion(self):
        stop = ("run", ("stop", "--time", "5", CID))
        self.assertIn(stop, self.lab.events)
        index = self.lab.events.index(stop)
        self.assertEqual(self.lab.events[index - 1], ("owned", CID))
        self.assertNotIn("built-image.json", self.lab.saved)
        self.assertNotIn("base_only_image_built", self.output.getvalue())

    def test_start_response_failure_still_stops_exact_owned_builder(self):
        self.lab.start_error = TimeoutError("synthetic_start_reply_lost")
        with self.assertRaises(TimeoutError):
            self.build()
        self.assert_stopped_without_completion()
        self.assertNotIn(("logged", "base-build"), self.lab.events)

    def test_negative_unexpected_success_never_writes_completion(self):
        self.lab.negative = "success"
        with self.assertRaisesRegex(ValueError, "negative_build_unexpected_success"):
            self.build()
        self.assert_stopped_without_completion()

    def test_negative_timeout_or_transport_failure_never_counts_as_expected_rejection(self):
        for state, error in (("timeout", TimeoutError), ("missing_receipt", KeyError)):
            with self.subTest(state=state):
                self.lab = FakeLab()
                self.lab.negative = state
                with self.assertRaises(error):
                    self.build()
                self.assert_stopped_without_completion()

    def test_failed_negative_leaving_nonempty_artifact_never_writes_completion(self):
        self.lab.negative_artifact_size = 1
        with self.assertRaisesRegex(RuntimeError, "negative_artifact_nonempty"):
            self.build()
        self.assert_stopped_without_completion()
        self.assertEqual(self.lab.loaded_paths, [str(self.lab.root / "base-image.tar")])

    def test_final_stop_failure_never_writes_completion(self):
        self.lab.stop_error = TimeoutError("synthetic_stop_reply_lost")
        with self.assertRaises(TimeoutError):
            self.build()
        self.assert_stopped_without_completion()
        self.assertEqual(self.lab.saved["build-negative.json"]["exit_code"], 1)

    def test_unrelated_nonzero_failure_is_not_an_expected_negative(self):
        self.lab.negative_log = "synthetic transport failure"
        with self.assertRaisesRegex(ValueError, "negative_build_wrong_failure"):
            self.build()
        self.assert_stopped_without_completion()

    def test_absent_or_empty_negative_output_is_not_loaded_and_completion_follows_stop(self):
        for size in (None, 0):
            with self.subTest(negative_output_size=size):
                self.lab = FakeLab()
                self.lab.negative_artifact_size = size
                self.build()
                events = self.lab.events
                negative_index = events.index(("logged", "build-negative"))
                stop_index = events.index(("run", ("stop", "--time", "5", CID)))
                completion_index = events.index(("save", "built-image.json"))
                self.assertLess(negative_index, stop_index)
                self.assertLess(stop_index, completion_index)
                self.assertEqual(self.lab.saved["built-image.json"]["id"], IMAGE_ID)
                self.assertTrue(self.lab.saved["built-image.json"]["base_only"])
                self.assertEqual(self.lab.loaded_paths, [str(self.lab.root / "base-image.tar")])
                self.assertIn("base_only_image_built", self.output.getvalue())


class CgroupObserverTests(unittest.TestCase):
    def setUp(self):
        self.parent = "/system.slice/docker-" + CID + ".scope"
        self.limits = {"memory.max": "2147483648", "memory.swap.max": "0",
                       "cpu.max": "200000 100000", "pids.max": "512"}
        self.lab = Mock()
        self.lab.owned.return_value = {"State": {"Pid": 101}}
        self.lab.run.return_value = subprocess.CompletedProcess(
            [], 0, "PID COMMAND\n101 buildkitd\n102 dpkg\n", "")

    @contextmanager
    def observer(self, groups):
        def group(pid):
            result = groups[pid]
            if isinstance(result, Exception):
                raise result
            return result
        def limit_text(path):
            if path.parent != Path("/sys/fs/cgroup" + self.parent):
                raise AssertionError("unexpected cgroup limit path")
            return self.limits[path.name] + "\n"

        with patch.object(module.CgroupObserver, "group", side_effect=group), \
             patch.object(module.Path, "read_text", autospec=True, side_effect=limit_text):
            yield

    def test_same_or_nested_cgroups_are_recorded(self):
        groups = {101: self.parent, 102: self.parent + "/buildkit/child"}
        with self.observer(groups), patch.object(module.time, "monotonic", return_value=1):
            observer = module.CgroupObserver(self.lab, CID)
            observer()
        self.lab.owned.assert_called_once_with(CID)
        self.lab.run.assert_called_once_with("top", CID, "-eo", "pid,comm")
        self.assertEqual(observer.commands, {"buildkitd", "dpkg"})
        self.assertEqual(observer.groups, set(groups.values()))

    def test_init_migration_does_not_reclassify_resource_root_as_escape(self):
        groups = {101: self.parent + "/init", 102: self.parent,
                  103: self.parent + "/buildkit/synthetic-run"}
        self.lab.run.return_value.stdout = "PID COMMAND\n101 buildkitd\n102 buildctl\n103 dpkg\n"
        with self.observer(groups), patch.object(module.time, "monotonic", return_value=1):
            observer = module.CgroupObserver(self.lab, CID)
            observer()
        self.assertEqual(observer.parent, self.parent)
        self.assertEqual(observer.commands, {"buildkitd", "buildctl", "dpkg"})
        self.assertEqual(observer.groups, set(groups.values()))
        self.lab.save.assert_not_called()

    def test_each_host_resource_limit_must_match_at_exact_container_scope(self):
        changes = {"memory.max": "max", "memory.swap.max": "2147483648",
                   "cpu.max": "max 100000", "pids.max": "max"}
        for name, value in changes.items():
            with self.subTest(name=name), patch.dict(self.limits, {name: value}), \
                 self.observer({101: self.parent + "/init"}), \
                 self.assertRaisesRegex(ValueError, "builder_host_cgroup_limit_mismatch"):
                module.CgroupObserver(self.lab, CID)

    def test_short_uppercase_or_path_container_identity_is_rejected_before_sysfs_read(self):
        for cid in ("a" * 12, "A" * 64, "../" + CID, CID + "/init"):
            with self.subTest(cid=cid), patch.object(module.Path, "read_text") as reader, \
                 self.assertRaisesRegex(ValueError, "exact_container_id_required"):
                module.CgroupObserver.limit_root(cid)
            reader.assert_not_called()

    def test_missing_host_limit_does_not_fall_back_to_process_subgroup(self):
        with patch.object(module.Path, "read_text", side_effect=FileNotFoundError()), \
             self.assertRaises(FileNotFoundError):
            module.CgroupObserver.limit_root(CID)

    def test_root_or_relative_builder_cgroup_is_rejected(self):
        for value in ("/", "relative", ""):
            with self.subTest(value=value), self.observer({101: value}), \
                 self.assertRaisesRegex(ValueError, "unbounded_builder_cgroup"):
                module.CgroupObserver(self.lab, CID)

    def test_sibling_prefix_lookalike_or_other_root_is_rejected(self):
        for value in (self.parent + "-other", "/system.slice/other.scope", "/"):
            with self.subTest(value=value), self.observer({101: self.parent, 102: value}), \
                 patch.object(module.time, "monotonic", return_value=1):
                observer = module.CgroupObserver(self.lab, CID)
                with self.assertRaisesRegex(ValueError, "build_child_escaped_cgroup"):
                    observer()
                self.assertNotIn(value, observer.groups)
                self.assertNotIn("dpkg", observer.commands)
                self.lab.save.assert_called_with("cgroup-rejected.json", {
                    "parent": self.parent, "observed": value, "pid": 102, "command": "dpkg"})

    def test_child_exiting_before_proc_read_is_tolerated_but_not_claimed(self):
        with self.observer({101: self.parent, 102: FileNotFoundError()}), \
             patch.object(module.time, "monotonic", return_value=1):
            observer = module.CgroupObserver(self.lab, CID)
            observer()
        self.assertEqual(observer.commands, {"buildkitd"})
        self.assertEqual(observer.groups, {self.parent})

    def test_other_proc_read_errors_are_not_ignored(self):
        with self.observer({101: self.parent, 102: PermissionError()}), \
             patch.object(module.time, "monotonic", return_value=1):
            observer = module.CgroupObserver(self.lab, CID)
            with self.assertRaises(PermissionError):
                observer()

    def test_group_parser_requires_one_unified_entry_and_reads_exact_pid(self):
        with patch.object(module.Path, "read_text", autospec=True,
                          return_value="0::" + self.parent + "\n") as reader:
            self.assertEqual(module.CgroupObserver.group(101), self.parent)
        reader.assert_called_once_with(Path("/proc/101/cgroup"))
        for content in ("", "1:cpu:/legacy\n", "0::/one\n0::/two\n"):
            with self.subTest(content=content), \
                 patch.object(module.Path, "read_text", return_value=content), \
                 self.assertRaisesRegex(ValueError, "unified_cgroup_required"):
                module.CgroupObserver.group(101)


if __name__ == "__main__":
    unittest.main()
