"""Local synthetic subprocesses and Docker-output fixtures; no daemon access."""

import copy
import io
import json
import os
from pathlib import Path
import stat
import subprocess
import sys
import tempfile
import unittest
from unittest.mock import Mock, patch

from lab import host


BUILDER_ID = "sha256:" + "1" * 64
BUILDER_VOLUME = "isthmus-s1b-synthetic-cache"
BUILDER_OWNER = "isthmus-s1b-synthetic"
BUILDER_COMMAND = ["-c", "exec buildkitd --config /etc/buildkit/buildkitd.toml"]


def builder_fixture():
    return {
        "Image": BUILDER_ID,
        "HostConfig": {
            "Memory": 2147483648, "MemorySwap": 2147483648,
            "CpuQuota": 200000, "CpuPeriod": 100000,
            "RestartPolicy": {"Name": "no"}, "Privileged": True,
            "PidsLimit": 512, "Binds": None, "PortBindings": {},
            "NetworkMode": "bridge", "PidMode": "", "IpcMode": "private",
            "CgroupnsMode": "private",
        },
        "Mounts": [{"Type": "volume", "Destination": "/var/lib/buildkit", "Name": BUILDER_VOLUME}],
        "Config": {"Env": ["PATH=/usr/bin:/bin"], "Labels": {host.LABEL: BUILDER_OWNER},
                   "Cmd": list(BUILDER_COMMAND), "Entrypoint": ["/bin/sh"]},
    }


def validate_builder(document):
    return host.validate_builder(document, image_id=BUILDER_ID, volume=BUILDER_VOLUME,
                                 owner=BUILDER_OWNER, command=BUILDER_COMMAND)


class BoundedProcessTests(unittest.TestCase):
    def invoke(self, program, *, limit=1024, timeout=2, sink=None,
               check_budget=None, already_exited=False):
        real_popen = subprocess.Popen
        self.processes = []

        def capture(*args, **kwargs):
            process = real_popen(*args, **kwargs)
            self.processes.append(process)
            if already_exited:
                # Outputs in these cases fit in a pipe. Guarantee the CLI has
                # exited before the reader starts, instead of a timing guess.
                process.wait(timeout=2)
            return process

        with patch.object(host.subprocess, "Popen", side_effect=capture):
            return host.bounded_process(
                [sys.executable, "-c", program], host.environment(Path("/synthetic/config")),
                timeout, limit, sink, check_budget)

    def assert_reaped_and_closed(self):
        self.assertEqual(len(self.processes), 1)
        process = self.processes[0]
        self.assertIsNotNone(process.returncode)
        self.assertTrue(process.stdout.closed)
        self.assertTrue(process.stderr.closed)
        with self.assertRaises(ChildProcessError):
            os.waitpid(process.pid, os.WNOHANG)

    def test_success_keeps_streams_separate_and_reaps(self):
        result = self.invoke("import os; os.write(1, b'out'); os.write(2, b'err')", limit=6)
        self.assertEqual((result.returncode, result.stdout, result.stderr), (0, "out", "err"))
        self.assert_reaped_and_closed()

    def test_nonzero_status_is_returned_and_reaped(self):
        result = self.invoke("import sys; sys.exit(7)")
        self.assertEqual(result.returncode, 7)
        self.assert_reaped_and_closed()

    def test_stdout_or_stderr_limit_is_enforced_before_complete_buffering(self):
        for descriptor in (1, 2):
            with self.subTest(descriptor=descriptor), self.assertRaisesRegex(ValueError, "output_limit"):
                self.invoke(f"import os, time; os.write({descriptor}, b'x' * 4096); time.sleep(30)", limit=64)
            self.assert_reaped_and_closed()

    def test_combined_stream_budget_cannot_be_spent_twice(self):
        with self.assertRaisesRegex(ValueError, "output_limit"):
            self.invoke("import os; os.write(1, b'a' * 64); os.write(2, b'b' * 64)", limit=100)
        self.assert_reaped_and_closed()

    def test_fast_exit_still_checks_output_budget(self):
        sink = io.BytesIO()
        with self.assertRaisesRegex(ValueError, "output_limit"):
            self.invoke("import os; os.write(1, b'x' * 4096)",
                        limit=32, sink=sink, already_exited=True)
        self.assertLessEqual(len(sink.getvalue()), 32)
        self.assert_reaped_and_closed()

    def test_sink_preserves_binary_bytes_without_retaining_output(self):
        sink = io.BytesIO()
        result = self.invoke("import os; os.write(1, bytes([0, 255, 10]))", limit=3, sink=sink)
        self.assertEqual(sink.getvalue(), bytes([0, 255, 10]))
        self.assertEqual((result.stdout, result.stderr, result.returncode), ("", "", 0))
        self.assert_reaped_and_closed()

    def test_quiet_and_closed_pipe_children_cannot_outlive_timeout(self):
        for program in ("import time; time.sleep(30)",
                        "import os, time; os.close(1); os.close(2); time.sleep(30)"):
            with self.subTest(program=program), self.assertRaises(TimeoutError):
                self.invoke(program, timeout=0.05)
            self.assert_reaped_and_closed()

    def test_capacity_failure_kills_and_reaps_child(self):
        check = Mock(side_effect=ValueError("disk_reserve_exhausted"))
        with self.assertRaisesRegex(ValueError, "disk_reserve_exhausted"):
            self.invoke("import time; time.sleep(30)", check_budget=check)
        check.assert_called_once_with()
        self.assert_reaped_and_closed()

    def test_sink_failure_kills_and_reaps_child(self):
        sink = Mock()
        sink.write.side_effect = OSError("synthetic sink failure")
        with self.assertRaises(OSError):
            self.invoke("import os, time; os.write(1, b'x'); time.sleep(30)", sink=sink)
        self.assert_reaped_and_closed()

    def test_final_deadline_is_rechecked_after_budget_io(self):
        current = [10.0]

        def advance_clock():
            current[0] = 13.0

        with patch.object(host.time, "monotonic", side_effect=lambda: current[0]), \
             self.assertRaises(TimeoutError):
            self.invoke("pass", timeout=2, check_budget=advance_clock, already_exited=True)
        self.assert_reaped_and_closed()


class LabHostTests(unittest.TestCase):
    def setUp(self):
        temporary = tempfile.TemporaryDirectory()
        self.addCleanup(temporary.cleanup)
        # Bypass __init__: these tests must never inspect a real Docker daemon.
        self.lab = host.Lab.__new__(host.Lab)
        self.lab.root = Path(temporary.name).resolve()
        self.lab.name = "isthmus-s1b-synthetic"
        self.lab.config = self.lab.root / "docker-config"
        self.lab.prefix = ["/nonexistent-synthetic-docker"]
        self.lab.capacity = Mock()

    def test_environment_is_a_fixed_allowlist_not_an_inherited_environment(self):
        inherited = {key: "synthetic-private-value" for key in (
            "HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy",
            "DOCKER_HOST", "DOCKER_CONTEXT", "DOCKER_TLS_VERIFY", "DOCKER_API_VERSION",
            "BUILDKIT_HOST", "SSH_AUTH_SOCK", "AWS_SECRET_ACCESS_KEY", "PATH")}
        with patch.dict(os.environ, inherited, clear=True):
            result = host.environment(self.lab.config)
        self.assertEqual(result, {
            "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8", "LC_ALL": "C.UTF-8",
            "DOCKER_CONFIG": str(self.lab.config), "BUILDX_NO_DEFAULT_ATTESTATIONS": "1",
        })
        self.assertNotIn("synthetic-private-value", json.dumps(result))

    def test_run_routes_through_bounded_reader_and_fixed_environment(self):
        completed = subprocess.CompletedProcess([], 0, "safe", "")
        with patch.object(host, "bounded_process", return_value=completed) as runner:
            self.assertIs(self.lab.run("info", timeout=5), completed)
        runner.assert_called_once_with(
            self.lab.prefix + ["info"], host.environment(self.lab.config), 5, 4 * 1024**2)

    def test_run_failure_does_not_echo_docker_stderr(self):
        completed = subprocess.CompletedProcess([], 7, "", "synthetic-private-value")
        with patch.object(host, "bounded_process", return_value=completed):
            with self.assertRaisesRegex(RuntimeError, "^docker_command_failed:inspect$"):
                self.lab.run("inspect", "synthetic-id")
            self.assertIs(self.lab.run("inspect", check=False), completed)

    def test_baseline_projects_inside_cli_before_python_sees_business_data(self):
        ids = ["b" * 64, "a" * 64]
        records = [
            {"Id": ident, "Name": "/business-" + ident[0], "Image": "sha256:" + ident,
             "RestartCount": 2, "State": {"StartedAt": "synthetic-time", "Status": "running"},
             "Mounts": [{"Type": "volume", "Name": "business-data", "Destination": "/data",
                         "RW": True, "Source": "/synthetic-not-returned"}]}
            for ident in ids
        ]

        def run(*args):
            if args[0] == "ps":
                self.assertEqual(args, ("ps", "-aq", "--no-trunc"))
                return subprocess.CompletedProcess([], 0, "\n".join(ids), "")
            self.assertEqual(args[0:2], ("inspect", "--format"))
            self.assertEqual(list(args[3:]), ids)
            template = args[2]
            self.assertNotIn("{{json .}}", template)
            self.assertNotIn(".Config", template)
            self.assertNotIn(".Env", template)
            self.assertNotIn(".Cmd", template)
            self.assertNotIn(".Args", template)
            for field in (".Id", ".Name", ".Image", ".RestartCount", ".State.StartedAt",
                          ".State.Status", ".Mounts"):
                self.assertIn("{{json " + field + "}}", template)
            return subprocess.CompletedProcess([], 0, "\n".join(json.dumps(v) for v in records), "")

        self.lab.run = Mock(side_effect=run)
        result = self.lab.baseline()
        self.assertEqual([v["id"] for v in result], sorted(ids))
        self.assertEqual(self.lab.run.call_count, 2)
        self.assertNotIn("synthetic-not-returned", json.dumps(result))
        self.assertEqual(result[0]["mounts"], [{
            "Type": "volume", "Name": "business-data", "Destination": "/data", "RW": True}])

    def test_empty_baseline_never_runs_unscoped_inspect(self):
        self.lab.run = Mock(return_value=subprocess.CompletedProcess([], 0, "", ""))
        self.assertEqual(self.lab.baseline(), [])
        self.lab.run.assert_called_once_with("ps", "-aq", "--no-trunc")

    def test_logged_abort_stops_owned_builder_even_after_reader_has_reaped_client(self):
        for index, failure in enumerate((TimeoutError("deadline"), ValueError("output_limit"),
                                         OSError("synthetic I/O failure"))):
            with self.subTest(failure=type(failure).__name__):
                events = []
                container_id = "a" * 64
                self.lab.owned = Mock(side_effect=lambda value: events.append(("owned", value)))
                self.lab.run = Mock(side_effect=lambda *args: events.append(("run", args)))
                with patch.object(host, "bounded_process", side_effect=failure), \
                     self.assertRaises(type(failure)):
                    self.lab.logged("abort-" + str(index), ["exec", container_id], stop_on_abort=container_id)
                self.assertEqual(events, [
                    ("owned", container_id), ("run", ("stop", "--time", "5", container_id))])
                self.assertFalse((self.lab.root / ("abort-" + str(index) + ".json")).exists())

    def test_abort_cannot_stop_container_whose_ownership_check_fails(self):
        self.lab.owned = Mock(side_effect=ValueError("container_ownership_mismatch"))
        self.lab.run = Mock()
        with patch.object(host, "bounded_process", side_effect=TimeoutError("deadline")), \
             self.assertRaisesRegex(ValueError, "ownership_mismatch"):
            self.lab.logged("foreign", ["exec"], stop_on_abort="a" * 64)
        self.lab.run.assert_not_called()

    def test_logged_success_uses_budget_and_private_files_without_stopping_builder(self):
        self.lab.owned = Mock()
        self.lab.run = Mock()

        def completed(argv, env, timeout, limit, sink, check_budget):
            self.assertEqual(argv, self.lab.prefix + ["exec", "synthetic-id"])
            self.assertEqual(env, host.environment(self.lab.config))
            self.assertEqual((timeout, limit), (9, 16 * 1024**2))
            self.assertIs(check_budget, self.lab.capacity)
            sink.write(b"public synthetic build output\n")
            return subprocess.CompletedProcess(argv, 0, "", "")

        with patch.object(host, "bounded_process", side_effect=completed):
            self.lab.logged("success", ["exec", "synthetic-id"], timeout=9, stop_on_abort="a" * 64)
        self.lab.owned.assert_not_called()
        self.lab.run.assert_not_called()
        receipt = json.loads((self.lab.root / "success.json").read_text())
        self.assertEqual(receipt["exit_code"], 0)
        for name in ("success.log", "success.json"):
            self.assertEqual(stat.S_IMODE((self.lab.root / name).stat().st_mode), 0o600)


class BuilderLimitTests(unittest.TestCase):
    def test_reviewed_limits_and_only_buildkit_volume_are_accepted(self):
        self.assertIsNone(validate_builder(builder_fixture()))
        document = builder_fixture()
        document["HostConfig"]["PidMode"] = "private"
        self.assertIsNone(validate_builder(document))

    def test_limit_privilege_network_and_host_mount_drift_is_rejected(self):
        changes = {
            "Memory": 2147483649, "MemorySwap": -1, "CpuQuota": -1, "CpuPeriod": 200000,
            "RestartPolicy": {"Name": "unless-stopped"}, "Privileged": False,
            "PidsLimit": -1, "Binds": ["/synthetic:/host"],
            "PortBindings": {"1234/tcp": [{"HostPort": "1234"}]}, "NetworkMode": "host",
        }
        for field, value in changes.items():
            document = builder_fixture()
            document["HostConfig"][field] = value
            with self.subTest(field=field), self.assertRaisesRegex(ValueError, "limits_or_scope"):
                validate_builder(document)

    def test_mount_shape_and_destination_drift_is_rejected(self):
        valid = builder_fixture()["Mounts"]
        for mounts in ([], valid * 2, [{"Type": "bind", "Destination": "/var/lib/buildkit"}],
                       [{"Type": "volume", "Destination": "/other"}],
                       [{"Type": "volume", "Destination": "/var/lib/buildkit", "Name": "other-cache"}]):
            document = builder_fixture()
            document["Mounts"] = copy.deepcopy(mounts)
            with self.subTest(mounts=mounts), self.assertRaisesRegex(ValueError, "mount_mismatch"):
                validate_builder(document)

    def test_image_owner_command_and_namespace_drift_is_rejected(self):
        changes = (
            (("Image",), "sha256:" + "2" * 64),
            (("Config", "Labels"), {}),
            (("Config", "Labels", host.LABEL), "isthmus-s1b-other"),
            (("Config", "Cmd"), BUILDER_COMMAND + ["--allow-insecure-entitlement=network.host"]),
            (("Config", "Entrypoint"), ["/usr/bin/other"]),
            (("HostConfig", "PidMode"), "host"),
            (("HostConfig", "IpcMode"), "host"),
            (("HostConfig", "CgroupnsMode"), "host"),
        )
        for path, value in changes:
            document = builder_fixture()
            parent = document
            for field in path[:-1]:
                parent = parent[field]
            parent[path[-1]] = value
            with self.subTest(path=path), self.assertRaisesRegex(ValueError, "identity_mismatch"):
                validate_builder(document)

    def test_proxy_variables_are_rejected_even_empty_or_different_case(self):
        for name in ("HTTP_PROXY", "HTTPS_PROXY", "ALL_PROXY", "NO_PROXY", "http_proxy", "Https_Proxy"):
            document = builder_fixture()
            document["Config"]["Env"].append(name + "=")
            with self.subTest(variable=name), self.assertRaisesRegex(ValueError, "proxy_injected"):
                validate_builder(document)
