"""Synthetic files/engine only: no Docker, SSH, real helper or CLI execution."""
import hashlib
import json
import os
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest import TestCase
from unittest.mock import Mock, patch

from lab import cli, identity
from test.test_lab_toolchain import fixture

CSR = "-----BEGIN CERTIFICATE REQUEST-----\nMAA=\n-----END CERTIFICATE REQUEST-----\n"


def public(label):
    return {"binding": identity._binding(label), "machine_id": ("c" if label == "a" else "d") * 32,
            "public_key_sha256": ("3" if label == "a" else "4") * 64}


class IdentityInputTests(TestCase):
    def files(self, root):
        binary = root / "identity-bin/instance-identity"
        binary.parent.mkdir(mode=0o700)
        content = b"\x7fELF\x02\x01" + b"\0" * 12 + b"\x3e\0" + b"synthetic-not-executable"
        binary.write_bytes(content)
        record = {"size": len(content), "sha256": hashlib.sha256(content).hexdigest()}
        (root / "identity-binary.json").write_text(json.dumps(record))
        return binary, record

    def test_bounded_amd64_helper_appends_only_one_reviewed_input(self):
        with TemporaryDirectory() as tmp, patch.object(cli, "inputs", return_value=[]):
            root = Path(tmp).resolve()
            binary, record = self.files(root)
            self.assertEqual(identity.inputs(SimpleNamespace(root=root)),
                             [(binary, "bin/instance-identity", record, 0o755)])

    def test_binary_record_has_exact_fields_types_and_caps(self):
        for replacement in ({"size": True}, {"size": 0}, {"size": 16 * 1024**2 + 1},
                            {"sha256": "F" * 64}, {"sha256": 1}, {"private": "not-allowed"}):
            with self.subTest(replacement=replacement), TemporaryDirectory() as tmp, patch.object(cli, "inputs", return_value=[]):
                root = Path(tmp).resolve()
                _, record = self.files(root)
                (root / "identity-binary.json").write_text(json.dumps({**record, **replacement}))
                with self.assertRaises(ValueError):
                    identity.inputs(SimpleNamespace(root=root))

    def test_binary_hash_architecture_links_and_duplicate_json_are_rejected(self):
        for mode in ("hash", "architecture", "symlink", "hardlink", "duplicate-json", "large-record"):
            with self.subTest(mode=mode), TemporaryDirectory() as tmp, patch.object(cli, "inputs", return_value=[]):
                root = Path(tmp).resolve()
                binary, record = self.files(root)
                if mode in ("hash", "architecture"):
                    data = bytearray(binary.read_bytes())
                    data[-1 if mode == "hash" else 18] ^= 1
                    binary.write_bytes(data)
                elif mode == "symlink":
                    binary.rename(binary.with_name("other"))
                    binary.symlink_to("other")
                elif mode == "hardlink":
                    os.link(binary, binary.with_name("other"))
                elif mode == "duplicate-json":
                    (root / "identity-binary.json").write_text('{"size":1,"size":1,"sha256":"' + "a" * 64 + '"}')
                else:
                    (root / "identity-binary.json").write_text(" " * 4097)
                with self.assertRaises((ValueError, OSError)):
                    identity.inputs(SimpleNamespace(root=root))

    def test_public_schema_never_retains_private_or_unexpected_fields(self):
        valid = {"public": public("a"), "csr_pem": CSR}
        self.assertEqual(identity.public_result(json.dumps(valid), identity._binding("a"), request=True), valid)
        for mutate in (
            lambda v: v.update(private_key_pkcs8="synthetic"),
            lambda v: v["public"].update(private="synthetic"),
            lambda v: v["public"]["binding"].update(epoch=True),
            lambda v: v["public"].update(machine_id="not-a-machine-id"),
            lambda v: v["public"].update(public_key_sha256="F" * 64),
            lambda v: v.update(csr_pem="-----BEGIN PRIVATE KEY-----\nMAA=\n-----END PRIVATE KEY-----\n"),
            lambda v: v.update(csr_pem=CSR + CSR),
        ):
            value = json.loads(json.dumps(valid))
            mutate(value)
            with self.assertRaises(ValueError):
                identity.public_result(json.dumps(value), identity._binding("a"), request=True)

    def test_shared_cli_script_keeps_all_gates_except_explicit_nonempty_home(self):
        default = cli.probe_script([])
        initialized = cli.probe_script([], require_empty_home=False)
        empty_check = 'test -z "$(find /home/claude -mindepth 1 -print -quit)"\n'
        self.assertEqual(default.replace(empty_check, ""), initialized)
        self.assertIn("memory.max", initialized)
        self.assertIn("find /opt/isthmus-probe -writable", initialized)
        self.assertIn("exec bin/bun-1.4.2 app/test/cli/roundtrip.smoke.ts", initialized)
        self.assertNotIn("roundtrip.smoke.ts", cli.probe_script([], smoke=False))
        with self.assertRaises(ValueError):
            cli.probe_script([(None, "bin/other; unexpected", {"sha256": "a" * 64}, 0o755)])

    def test_one_gib_contract_is_explicit_and_legacy_default_unchanged(self):
        from lab.toolchain import probe_args, validate_probe
        from lab.payload import ROOT_UPLOAD, root_upload
        self.assertEqual(root_upload(), ROOT_UPLOAD)
        self.assertEqual(root_upload(memory_bytes=1024**3), ROOT_UPLOAD.replace("2147483648", "1073741824"))
        self.assertIn('test "$(cat /sys/fs/cgroup/memory.max)" = 2147483648\n', ROOT_UPLOAD)
        for size in (1, 2):
            args = probe_args("isthmus-s1b-synthetic", memory_gib=size)
            self.assertEqual(args[args.index("--memory") + 1], str(size) + "g")
            self.assertEqual(args[args.index("--memory-swap") + 1], str(size) + "g")
            state = fixture()
            state["HostConfig"]["Memory"] = state["HostConfig"]["MemorySwap"] = size * 1024**3
            validate_probe(state, "isthmus-s1b-synthetic", memory_gib=size)
            self.assertIn('memory.max)" = ' + str(size * 1024**3), cli.probe_script([], memory_bytes=size * 1024**3))
        self.assertEqual(probe_args("synthetic"), probe_args("synthetic", memory_gib=2))
        for size in (True, 0, 3, "1", 1.0):
            with self.subTest(size=size):
                with self.assertRaises(ValueError):
                    probe_args("synthetic", memory_gib=size)
                with self.assertRaises(ValueError):
                    validate_probe(fixture(), "synthetic", memory_gib=size)
                with self.assertRaises(ValueError):
                    cli.probe_script([], memory_bytes=size)
                with self.assertRaises(ValueError):
                    root_upload(memory_bytes=size)


class IdentityProbeTests(TestCase):
    def run_case(self, *, fail_create=False, fail_start=False, fail_upload=False, fail_cli=False,
                 fail_remove=False, drift=False, wrong_accepted=False, changed=False, shared=False,
                 incomplete=False, oom=False, uncertain_create=None):
        with TemporaryDirectory() as tmp:
            root = Path(tmp).resolve()
            lab = Mock(root=root, config=root / "docker-config", prefix=["synthetic-docker"])
            lab.name = "isthmus-s1b-synthetic"
            lab.baseline.side_effect = [["baseline"], ["changed"] if drift else ["baseline"]]
            created, removed, commands, pending_wrong = [], [], [], set()
            started = set()
            states = {}

            def run(*args, **kwargs):
                if args[0] == "create":
                    label = "a" if not created else "b"
                    if label == "b" and fail_create:
                        raise RuntimeError("synthetic_create_failure")
                    self.assertEqual(args[args.index("--name") + 1], lab.name + "-identity-" + label)
                    self.assertIn("none", args)
                    self.assertNotIn("--volume", args)
                    cid = label * 64
                    created.append(cid)
                    state = fixture()
                    state["HostConfig"]["Memory"] = state["HostConfig"]["MemorySwap"] = 1024**3
                    state["State"] = {"Running": False, "OOMKilled": False}
                    states[cid] = state
                    if label == "b" and uncertain_create:
                        if uncertain_create == "timeout":
                            raise TimeoutError("synthetic_unacknowledged_create")
                        return SimpleNamespace(stdout="synthetic_invalid_id")
                    return SimpleNamespace(stdout=cid)
                if args[0] == "start":
                    if args[1].startswith("b") and fail_start:
                        raise RuntimeError("synthetic_start_failure")
                    states[args[1]]["State"]["Running"] = True
                    started.add(args[1])
                if args[0] == "stop":
                    states[args[-1]]["State"]["Running"] = False
                if args[0] == "rm":
                    if args[1].startswith("a") and fail_remove:
                        raise RuntimeError("synthetic_remove_failure")
                    removed.append(args[1])
                if args[0] == "exec":
                    self.assertEqual(args[1:3], ("--user", "1000:1000"))
                    self.assertNotIn("instance-identity.json", " ".join(args))
                return SimpleNamespace(stdout="")

            lab.run.side_effect = run
            lab.owned.side_effect = lambda cid: states[cid]

            def process(argv, env, timeout, limit, **kwargs):
                self.assertLessEqual(timeout, 200)
                if argv[2:4] == ["--user", "0:0"]:
                    self.assertEqual(started, {"a" * 64, "b" * 64})
                    self.assertEqual(argv[-1], identity.root_upload(memory_bytes=1024**3))
                    self.assertNotIn("bun", argv[-1])
                    self.assertNotIn("instance-identity", argv[-1])
                    return SimpleNamespace(returncode=1 if fail_upload else 0, stdout="root-upload-capless-pass\n", stderr="")
                self.assertEqual(argv[2:5], ["--user", "1000:1000", "-i"])
                cid, operation = argv[5], argv[-2]
                label = cid[0]
                binding = json.loads(os.pread(kwargs["input_fd"], 4096, 0))
                commands.append((label, operation, binding))
                if binding != identity._binding(label):
                    pending_wrong.add(label)
                    if not wrong_accepted:
                        return SimpleNamespace(returncode=1, stdout="", stderr=identity.DENIED)
                value = {"public": public("a" if shared else label)}
                value["public"]["binding"] = binding
                if changed and label in pending_wrong:
                    value["public"]["machine_id"] = "e" * 32
                if operation == "request":
                    value["csr_pem"] = CSR
                return SimpleNamespace(returncode=0, stdout=json.dumps(value), stderr="")

            def logged(name, args, **kwargs):
                if fail_cli:
                    raise RuntimeError("synthetic_cli_failure")
                self.assertEqual(args[1:3], ["--user", "1000:1000"])
                self.assertIn("exec bin/bun-1.4.2 app/test/cli/roundtrip.smoke.ts", args[-1])
                self.assertNotIn('find /home/claude -mindepth 1', args[-1])
                (root / (name + ".log")).write_text("incomplete" if incomplete else identity.CLI_PASS + "\n")
                if oom:
                    states[args[3]]["State"]["OOMKilled"] = True

            lab.logged.side_effect = logged
            original_read = Path.read_text

            def contents(path, *args, **kwargs):
                if str(path) == "/proc/cpuinfo":
                    return "avx2"
                if str(path) == "/proc/meminfo":
                    return "MemAvailable: 4194304 kB\n"
                return original_read(path, *args, **kwargs)

            failed = any((fail_create, fail_start, fail_upload, fail_cli, fail_remove, drift,
                          wrong_accepted, changed, shared, incomplete, oom, uncertain_create))
            with patch.object(identity, "inputs", return_value=[]), patch.object(identity, "bounded_process", side_effect=process), patch.object(Path, "read_text", contents):
                if failed:
                    with self.assertRaises((ValueError, RuntimeError, TimeoutError)):
                        identity.probe(lab)
                else:
                    identity.probe(lab)
            names = [call.args[0] for call in lab.save.call_args_list]
            if failed:
                self.assertNotIn("identity-result.json", names)
                if uncertain_create or fail_create:
                    cleanup = next(call.args[1] for call in lab.save.call_args_list if call.args[0] == "identity-cleanup.json")
                    self.assertTrue(cleanup["unresolved_create"])
                    self.assertFalse(cleanup["cleanup_complete"])
            else:
                self.assertLess(names.index("identity-cleanup.json"), names.index("identity-result.json"))
                self.assertEqual(lab.logged.call_count, 2)
                result = next(call.args[1] for call in lab.save.call_args_list if call.args[0] == "identity-result.json")
                self.assertFalse(result["mtls_installed"])
                self.assertFalse(result["restricted_egress_verified"])
                self.assertEqual(result["real_model_requests"], 0)
                self.assertEqual(len([c for c in commands if c[2] != identity._binding(c[0])]), 10)
                self.assertEqual(len([c for c in commands if c[1] == "request"]), 2)
            self.assertEqual(removed, [cid for cid in created if not (fail_remove and cid.startswith("a"))
                                      and not (uncertain_create and cid.startswith("b"))])
            return lab

    def test_two_simultaneous_instances_stable_identities_and_cli_regressions(self):
        self.run_case()

    def test_partial_create_and_start_failures_clean_up_every_created_id(self):
        self.run_case(fail_create=True)
        self.run_case(fail_start=True)

    def test_unacknowledged_create_never_claims_complete_cleanup(self):
        # Model a daemon-created second container whose ID was never received.
        # Clean the known first ID, leave the unknown target explicitly unresolved.
        self.run_case(uncertain_create="timeout")
        self.run_case(uncertain_create="invalid-id")

    def test_upload_and_cli_failures_clean_both_and_never_succeed(self):
        self.run_case(fail_upload=True)
        self.run_case(fail_cli=True)

    def test_bad_identity_outcomes_cannot_pass(self):
        for options in ({"wrong_accepted": True}, {"changed": True}, {"shared": True}):
            with self.subTest(options=options):
                self.run_case(**options)

    def test_incomplete_cli_or_oom_cannot_pass(self):
        self.run_case(incomplete=True)
        self.run_case(oom=True)

    def test_cleanup_failure_still_attempts_other_container_and_no_success(self):
        self.run_case(fail_remove=True)

    def test_original_business_drift_never_publishes_success(self):
        self.run_case(drift=True)

    def test_foreign_container_is_not_removed_but_other_owned_target_is_cleaned(self):
        lab = Mock()
        owned = fixture()
        owned["State"] = {"Running": False}
        lab.owned.side_effect = lambda cid: (_ for _ in ()).throw(ValueError("foreign")) if cid == "a" * 64 else owned
        lab.baseline.return_value = ["baseline"]
        with self.assertRaisesRegex(ValueError, "identity_cleanup_failed"):
            identity._cleanup(lab, {"a": "a" * 64, "b": "b" * 64}, ["baseline"])
        self.assertEqual([call.args for call in lab.run.call_args_list], [("rm", "b" * 64)])

    def test_expired_work_budget_fails_before_an_operation(self):
        with self.assertRaises(TimeoutError):
            identity._remaining(0)
