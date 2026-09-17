"""Mock-only exact-resource cleanup and upload contract; never Docker or CLI."""
from pathlib import Path
from tempfile import TemporaryDirectory
from types import SimpleNamespace
from unittest import TestCase
from unittest.mock import Mock, patch

from lab import cli
from test.test_lab_toolchain import fixture


class CliLabTests(TestCase):
    def run_case(self, *, fail_upload=False, fail_run=False, fail_remove=False, drift=False):
        with TemporaryDirectory() as tmp:
            lab = Mock(root=Path(tmp), config=Path(tmp) / "config",
                       prefix=["synthetic-docker"])
            lab.name = "isthmus-s1b-synthetic"
            lab.baseline.side_effect = [["baseline"], ["drift"] if drift else ["baseline"]]
            state = fixture()
            state["State"] = {"Running": True, "OOMKilled": False}
            lab.owned.return_value = state
            def run(*args, **kwargs):
                if args[0] == "rm" and fail_remove:
                    raise RuntimeError("synthetic_remove_failed")
                return SimpleNamespace(stdout="a" * 64)
            lab.run.side_effect = run
            def logged(*args, **kwargs):
                if fail_run:
                    raise RuntimeError("synthetic_probe_failed")
            lab.logged.side_effect = logged
            def contents(path):
                return {"cpuinfo": "avx2", "meminfo": "MemAvailable: 4194304 kB\n",
                    "cli-roundtrip.log": "cli-roundtrip smoke PASS: synthetic only, real_model_requests=0, owned listeners stopped\n"}[path.name]
            with patch.object(cli, "inputs", return_value=[]), \
                 patch.object(Path, "read_text", contents), \
                 patch.object(cli, "bounded_process", return_value=SimpleNamespace(
                     returncode=1 if fail_upload else 0, stdout="root-upload-capless-pass\n")) as upload:
                if fail_upload or fail_run or fail_remove or drift:
                    with self.assertRaises((ValueError, RuntimeError)):
                        cli.probe(lab)
                else:
                    cli.probe(lab)
                self.assertEqual(upload.call_args.args[0][1:5], ["exec", "--user", "0:0", "-i"])
                self.assertIn("/bin/tar", upload.call_args.args[0][-1])
                self.assertNotIn("bun", upload.call_args.args[0][-1])
            if fail_upload:
                lab.logged.assert_not_called()
            else:
                args = lab.logged.call_args.args[1]
                self.assertEqual(args[:5], ["exec", "a" * 64, "/usr/bin/timeout", "--kill-after=5", "150"])
                self.assertIn("test \"$(ls /sys/class/net)\" = lo", args[-1])
                self.assertIn("exec bin/bun-1.4.2 app/test/cli/roundtrip.smoke.ts", args[-1])
            self.assertIn((("rm", "a" * 64),), [(call.args,) for call in lab.run.call_args_list])
            return [call.args[0] for call in lab.save.call_args_list]

    def test_success_only_after_owned_cleanup_and_unchanged_business(self):
        names = self.run_case()
        self.assertLess(names.index("cli-cleanup.json"), names.index("cli-result.json"))

    def test_upload_and_execution_failures_clean_up_without_success(self):
        for options in ({"fail_upload": True}, {"fail_run": True}):
            with self.subTest(options=options):
                self.assertNotIn("cli-result.json", self.run_case(**options))

    def test_cleanup_failure_or_business_drift_cannot_publish_success(self):
        for options in ({"fail_remove": True}, {"drift": True}):
            with self.subTest(options=options):
                self.assertNotIn("cli-result.json", self.run_case(**options))

    def test_input_allowlist_includes_every_test_import_but_no_old_wip_or_credentials(self):
        self.assertEqual(len(cli.SOURCE_FILES), 10)
        self.assertIn("src/app/shutdown.ts", cli.SOURCE_FILES)
        self.assertTrue(all(p.startswith(("src/", "test/cli/")) for p in cli.SOURCE_FILES))
        self.assertFalse(any("image/" in p or ".env" in p or "home/" in p for p in cli.SOURCE_FILES))
