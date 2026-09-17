"""Pure contracts only: never contacts Docker, creates a VM or runs a worker."""
import base64
import hashlib
import io
import json
import os
import selectors
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest import TestCase
from unittest.mock import Mock, patch

from lab.livebootstrap import inputs, policy
from lab.livebootstrap.driver import Driver, FALSE_FIELDS, TRUE_FIELDS, summary
from lab.livebootstrap.probe import kernel_script, probe
from lab.toolchain import BASE_ID, LABEL
from lab.host import Lab
from lab.identity import _cleanup
from test.test_lab_toolchain import fixture as old_fixture

PUBLIC = {"trust_sha256": "a" * 64, "ticket_public_key": base64.b64encode(b"x" * 32).decode().rstrip("=")}
RESULT = dict.fromkeys(TRUE_FIELDS, True) | dict.fromkeys(FALSE_FIELDS, False)


def fixture(lab, label="a"):
    v = old_fixture()
    name = lab.name + "-live-" + label
    v.update(Id=label * 64, Name="/" + name, State={"Running": True, "OOMKilled": False})
    v["Config"].update(Image=BASE_ID, Hostname=name, Env=policy.envs(label, PUBLIC),
                       Labels={LABEL: lab.name}, Cmd=["--kill-after=5", "90", "/worker"])
    v["HostConfig"].update(Memory=1024**3, MemorySwap=1024**3, Tmpfs=dict(policy.TMPFS),
                          Binds=[str(lab.root / "live-bin/worker") + ":/worker:ro"])
    v["Mounts"] = [{"Type": "bind", "Source": str(lab.root / "live-bin/worker"), "Destination": "/worker", "RW": False}]
    return v


class LivePolicyTests(TestCase):
    def setUp(self):
        self.lab = SimpleNamespace(root=Path("/var/tmp/isthmus-s1b.Abcd1234"), name="isthmus-s1b-abcd1234")

    def test_fixed_arguments_and_positive_profile(self):
        policy.validate(fixture(self.lab), self.lab, "a", PUBLIC)
        args = policy.args(self.lab, "a", PUBLIC)
        self.assertEqual(args[args.index("--volume") + 1], "/var/tmp/isthmus-s1b.Abcd1234/live-bin/worker:/worker:ro")
        self.assertEqual(args[-4:], [BASE_ID, "--kill-after=5", "90", "/worker"])
        self.assertNotIn("--privileged", args)
        self.assertIn("1073741824", kernel_script("a" * 64))

    def test_drift_is_denied(self):
        for field, value in (("NetworkMode", "host"), ("MemorySwap", -1), ("Memory", 0), ("PidsLimit", 0),
                             ("Binds", []), ("Mounts", [{}]), ("Devices", [{}]), ("CapAdd", ["SYS_ADMIN"]),
                             ("ReadonlyRootfs", False), ("Privileged", True), ("Tmpfs", {}), ("Ulimits", [])):
            with self.subTest(field=field):
                state = fixture(self.lab)
                state["HostConfig"][field] = value
                with self.assertRaises(ValueError):
                    policy.validate(state, self.lab, "a", PUBLIC)
        for change in (lambda v: v["Config"]["Env"].append("HTTPS_PROXY=http://untrusted.invalid"),
                       lambda v: v["Config"].update(User="0:0"), lambda v: v["Mounts"][0].update(RW=True),
                       lambda v: v["Mounts"][0].update(Source="/etc/passwd"), lambda v: v.update(Image="latest")):
            state = fixture(self.lab)
            change(state)
            with self.assertRaises(ValueError):
                policy.validate(state, self.lab, "a", PUBLIC)

    def test_public_configuration_and_success_cannot_promote_capabilities(self):
        self.assertEqual(policy.public_config(PUBLIC), PUBLIC)
        self.assertEqual(summary(RESULT), RESULT)
        for value in (PUBLIC | {"private_key": "not-allowed"}, PUBLIC | {"trust_sha256": "bad"},
                      PUBLIC | {"ticket_public_key": "***"}):
            with self.assertRaises(ValueError):
                policy.public_config(value)
        for value in (RESULT | {"tcp_ready": 1}, RESULT | {"production_ready": True},
                      RESULT | {"secret": "not-allowed"}, {}):
            with self.assertRaises(ValueError):
                summary(value)


class LiveInputTests(TestCase):
    def test_exact_public_binaries_and_tampering(self):
        for mode in ("valid", "hash", "writeable", "size", "extra", "link", "symlink", "duplicate"):
            with self.subTest(mode=mode), TemporaryDirectory() as tmp:
                root = Path(tmp).resolve()
                (root / "live-bin").mkdir()
                data = b"\x7fELF\x02\x01" + b"\0" * 12 + b"\x3e\0" + b"not-executable-fixture"
                records = []
                for name in inputs.NAMES:
                    path = root / "live-bin" / name
                    path.write_bytes(data)
                    path.chmod(0o555)
                    records.append({"name": name, "size": len(data), "sha256": hashlib.sha256(data).hexdigest()})
                path = root / "live-bin/worker"
                if mode == "hash": records[0]["sha256"] = "0" * 64
                elif mode == "writeable": path.chmod(0o755)
                elif mode == "size": records[0]["size"] = True
                elif mode == "extra": records[0]["untrusted"] = "value"
                elif mode == "link": os.link(path, root / "other")
                elif mode == "symlink": path.rename(root / "other"); path.symlink_to(root / "other")
                raw = json.dumps(records)
                if mode == "duplicate": raw = raw.replace('"name":', '"name":"duplicate","name":', 1)
                (root / "live-binaries.json").write_text(raw)
                if mode == "valid": self.assertEqual(inputs.verify(SimpleNamespace(root=root)), records)
                else:
                    with self.assertRaises((ValueError, OSError)):
                        inputs.verify(SimpleNamespace(root=root))


class LiveIPCTests(TestCase):
    def test_late_pipe_or_buffered_json_is_not_accepted(self):
        for phase in ("capacity", "select", "read", "parse"):
            with self.subTest(phase=phase):
                now = [0.0]
                def advance():
                    now[0] = 2.0
                out_r, out_w = os.pipe()
                os.write(out_w, (json.dumps(PUBLIC) + "\n").encode())
                os.close(out_w)
                stream = os.fdopen(out_r, "rb", buffering=0)
                lab = Mock()
                if phase == "capacity": lab.capacity.side_effect = advance
                driver = Driver(lab)
                driver.selector.register(stream, selectors.EVENT_READ, "stdout")
                select, read, parse = driver.selector.select, os.read, json.loads
                def late_select(timeout):
                    value = select(timeout)
                    if phase == "select": advance()
                    return value
                def late_read(*args):
                    value = read(*args)
                    if phase == "read": advance()
                    return value
                def late_parse(*args, **kwargs):
                    value = parse(*args, **kwargs)
                    if phase == "parse": advance()
                    return value
                if phase == "parse": driver.pending.extend((json.dumps(PUBLIC) + "\n").encode())
                try:
                    with patch("lab.livebootstrap.driver.time.monotonic", side_effect=lambda: now[0]), \
                         patch.object(driver.selector, "select", side_effect=late_select), \
                         patch("lab.livebootstrap.driver.os.read", side_effect=late_read), \
                         patch("lab.livebootstrap.driver.json.loads", side_effect=late_parse):
                        with self.assertRaises(ValueError): driver.line(1)
                finally:
                    driver.selector.close()
                    stream.close()

    def test_bounded_real_pipe_protocol_without_a_subprocess(self):
        for mode in ("valid", "extra", "stderr", "large", "duplicate", "exit"):
            with self.subTest(mode=mode):
                out_r, out_w = os.pipe()
                err_r, err_w = os.pipe()
                first = json.dumps(PUBLIC)
                if mode == "duplicate": first = first.replace('"trust_sha256":', '"trust_sha256":"b","trust_sha256":')
                body = first + "\n" + json.dumps(RESULT) + "\n"
                if mode == "extra": body += "unexpected\n"
                if mode == "large": body = "x" * 4097
                os.write(out_w, body.encode()); os.close(out_w)
                if mode == "stderr": os.write(err_w, b"never echo this")
                os.close(err_w)
                process = SimpleNamespace(stdin=io.BytesIO(), stdout=os.fdopen(out_r, "rb", buffering=0), stderr=os.fdopen(err_r, "rb", buffering=0),
                    poll=lambda: 0, wait=lambda timeout: 1 if mode == "exit" else 0)
                lab = Mock(root=Path("/var/tmp/isthmus-s1b.Abcd1234"))
                lab.name = "isthmus-s1b-abcd1234"
                with patch("lab.livebootstrap.driver.subprocess.Popen", return_value=process):
                    if mode == "valid":
                        with Driver(lab) as driver:
                            self.assertEqual(driver.finish(["a" * 64, "b" * 64]), RESULT)
                    else:
                        with self.assertRaises(ValueError):
                            with Driver(lab) as driver:
                                driver.finish(["a" * 64, "b" * 64])


class LiveCleanupTests(TestCase):
    def test_live_cleanup_preserves_existing_identity_receipt(self):
        with TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            (root / "identity-cleanup.json").write_text("previous-stage-receipt\n")
            lab = Mock(root=root)
            lab.baseline.return_value = []
            lab.save.side_effect = lambda name, value: Lab.save(lab, name, value)
            _cleanup(lab, {}, [], evidence_name="live-cleanup.json")
            self.assertEqual((root / "identity-cleanup.json").read_text(), "previous-stage-receipt\n")
            self.assertTrue(json.loads((root / "live-cleanup.json").read_text())["cleanup_complete"])
            with self.assertRaises(ValueError):
                _cleanup(lab, {}, [], evidence_name="../unexpected.json")

    def test_only_complete_and_clean_run_publishes_success(self):
        for mode in ("pass", "uncertain", "uncertain-b", "driver-fail", "cleanup-fail", "business-drift"):
            with self.subTest(mode=mode):
                lab = Mock(root=Path("/var/tmp/isthmus-s1b.Abcd1234"))
                lab.name = "isthmus-s1b-abcd1234"
                lab.baseline.side_effect = [["unchanged"], ["drift"] if mode == "business-drift" else ["unchanged"]]
                creates = []
                def run(*args, **kwargs):
                    if args[0] == "create":
                        if mode == "uncertain" or mode == "uncertain-b" and creates: raise TimeoutError("uncertain create")
                        cid = ("a" if not creates else "b") * 64
                        creates.append(cid)
                        return SimpleNamespace(stdout=cid)
                    if args[0] == "exec": return SimpleNamespace(stdout="live-worker-kernel-pass\n")
                    if args[0] == "rm" and mode == "cleanup-fail": raise ValueError("synthetic failure")
                    return SimpleNamespace(stdout="")
                lab.run.side_effect = run
                lab.owned.side_effect = lambda cid: fixture(lab, cid[0])
                driver = Mock(public=PUBLIC)
                driver.finish.return_value = RESULT.copy()
                if mode == "driver-fail": driver.finish.side_effect = ValueError("synthetic failure")
                manager = Mock()
                manager.__enter__ = Mock(return_value=driver)
                manager.__exit__ = Mock(return_value=False)
                with patch("lab.livebootstrap.probe.Driver", return_value=manager), \
                     patch("lab.livebootstrap.probe.inputs.verify", return_value=[{"sha256": "a" * 64}]), \
                     patch.object(Path, "read_text", return_value="MemAvailable: 4194304 kB\n"):
                    if mode == "pass": probe(lab)
                    else:
                        with self.assertRaises((ValueError, TimeoutError)): probe(lab)
                names = [call.args[0] for call in lab.save.call_args_list]
                self.assertEqual("live-result.json" in names, mode == "pass")
                self.assertIn("live-cleanup.json", names)
                self.assertNotIn("identity-cleanup.json", names)
                removed = [call.args[1] for call in lab.run.call_args_list if call.args[0] == "rm"]
                self.assertEqual(removed, [] if mode == "uncertain" else ["a" * 64] if mode == "uncertain-b" else ["a" * 64, "b" * 64])
