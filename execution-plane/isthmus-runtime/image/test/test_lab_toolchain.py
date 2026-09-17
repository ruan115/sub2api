"""Pure Docker-contract tests; no Docker, network, account or CLI invocation."""
import unittest
from pathlib import Path
from unittest.mock import Mock, patch
from types import SimpleNamespace
from lab.toolchain import probe
from lab.toolchain import BASE_ID, ENVS, LABEL, TMPFS, probe_args, script_for, validate_probe


def fixture():
    return {"Image": BASE_ID, "Config": {"User": "1000:1000", "Env": list(ENVS),
        "Labels": {LABEL: "isthmus-s1b-synthetic"}, "WorkingDir": "/",
        "Entrypoint": ["/usr/bin/timeout"], "Cmd": ["--kill-after=5", "240", "/bin/sleep", "240"]},
        "Mounts": [], "HostConfig": {"Privileged": False, "ReadonlyRootfs": True,
        "NetworkMode": "none", "Memory": 2147483648, "MemorySwap": 2147483648,
        "NanoCpus": 1000000000, "PidsLimit": 128, "CapAdd": None, "CapDrop": ["ALL"],
        "SecurityOpt": ["no-new-privileges"], "Binds": None, "PortBindings": {},
        "Devices": [], "DeviceRequests": None, "PidMode": "", "IpcMode": "private",
        "CgroupnsMode": "private", "RestartPolicy": {"Name": "no"}, "Tmpfs": dict(TMPFS),
        "Ulimits": [{"Name": "core", "Hard": 0, "Soft": 0}]}}


class ToolchainProbeTests(unittest.TestCase):
    def probe_fixture(self, *, fail_remove=False, drift=False, fail_upload=False):
        lab = Mock()
        lab.name, lab.root = "isthmus-s1b-synthetic", Path("/synthetic")
        lab.baseline.side_effect = [["baseline"], ["drift"] if drift else ["baseline"]]
        state = fixture()
        state["State"] = {"Running": True, "ExitCode": 0, "OOMKilled": False}
        lab.owned.return_value = state
        def run(*args, **kwargs):
            if args[0] == "rm" and fail_remove:
                raise RuntimeError("synthetic_cleanup_failure")
            return SimpleNamespace(stdout="a" * 64)
        lab.run.side_effect = run
        def contents(path):
            return {"cpuinfo": "avx2", "meminfo": "MemAvailable: 4194304 kB\n",
                    "toolchain-probe.log": "native-toolchain-probe-pass\n"}[path.name]
        with patch("lab.toolchain.checksums", return_value=[]), \
                patch("lab.payload.upload_payload", side_effect=ValueError("synthetic_upload_failure") if fail_upload else None), \
                patch("lab.toolchain.script_for", return_value="synthetic"), \
                patch.object(Path, "read_text", contents):
            if fail_remove or drift or fail_upload:
                with self.assertRaises((ValueError, RuntimeError)):
                    probe(lab)
            else:
                probe(lab)
        if fail_upload:
            lab.logged.assert_not_called()
        return [call.args[0] for call in lab.save.call_args_list]

    def test_success_receipt_only_after_cleanup(self):
        names = self.probe_fixture()
        self.assertLess(names.index("toolchain-cleanup.json"), names.index("toolchain-result.json"))

    def test_cleanup_failure_or_business_drift_never_publishes_success(self):
        self.assertNotIn("toolchain-result.json", self.probe_fixture(fail_remove=True))
        self.assertNotIn("toolchain-result.json", self.probe_fixture(drift=True))

    def test_upload_failure_cleans_up_without_executing_tools(self):
        names = self.probe_fixture(fail_upload=True)
        self.assertIn("toolchain-cleanup.json", names)
        self.assertNotIn("toolchain-result.json", names)

    def test_positive_contract(self):
        validate_probe(fixture(), "isthmus-s1b-synthetic")

    def test_drift_denied(self):
        for field, replacement in (("Privileged", True), ("ReadonlyRootfs", False),
                ("NetworkMode", "host"), ("Memory", 0), ("MemorySwap", -1), ("NanoCpus", 0),
                ("PidsLimit", 0), ("CapAdd", ["SYS_ADMIN"]), ("CapDrop", []),
                ("SecurityOpt", []), ("Binds", ["/synthetic:/synthetic"]),
                ("PortBindings", {"80/tcp": []}), ("Devices", [{}]), ("DeviceRequests", [{}]),
                ("PidMode", "host"), ("IpcMode", "host"), ("CgroupnsMode", "host"),
                ("RestartPolicy", {"Name": "always"}), ("Tmpfs", {}), ("Ulimits", [])):
            with self.subTest(field=field):
                value = fixture()
                value["HostConfig"][field] = replacement
                with self.assertRaises(ValueError):
                    validate_probe(value, "isthmus-s1b-synthetic")

    def test_env_injection_and_image_user_mount_mismatch_denied(self):
        for change in (lambda v: v["Config"]["Env"].append("HTTPS_PROXY=http://synthetic.invalid"),
                       lambda v: v["Config"].update(User="0"),
                       lambda v: v.update(Image="latest"),
                       lambda v: v.update(Mounts=[{"Type": "bind"}])):
            value = fixture()
            change(value)
            with self.assertRaises(ValueError):
                validate_probe(value, "isthmus-s1b-synthetic")

    def test_creation_has_fixed_image_no_bind_and_internal_watchdog(self):
        args = probe_args("isthmus-s1b-synthetic")
        self.assertNotIn("--privileged", args)
        self.assertNotIn("--volume", args)
        self.assertNotIn("--mount", args)
        self.assertEqual(args[-5:], [BASE_ID, "--kill-after=5", "240", "/bin/sleep", "240"])

    def test_candidate_gate_and_control_are_not_swallowed(self):
        script = script_for([])
        self.assertIn("bun-1.4.2 test", script)
        self.assertIn("bun-1.3.9 test", script)
        self.assertIn("while test $i -lt 5", script)
        self.assertIn('test "$control" = 1', script)
        self.assertIn("grep -Fx 'shutdown gate FAIL:", script)
        self.assertIn('test "$(wc -l < /tmp/control-shutdown.log)" = 1', script)
        self.assertIn("test \"$(cat /sys/fs/cgroup/memory.swap.max)\" = 0", script)
        self.assertNotIn("|| true", script)

    def test_traversal_and_home_paths_denied(self):
        for path in ("app/src/../../secrets", "/home/claude", "app/.env", "bin/other"):
            with self.assertRaises(ValueError):
                script_for([{"path": path, "sha256": "0" * 64}])
