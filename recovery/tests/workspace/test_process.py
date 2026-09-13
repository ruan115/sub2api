"""Small synthetic subprocesses exercise stream budgets and process ownership."""

from __future__ import annotations

import os
from pathlib import Path
import signal
import subprocess
import sys
import time
import unittest
from unittest.mock import patch

from recoverykit.workspace.errors import WorkspaceError
from recoverykit.workspace import git as git_module
from recoverykit.workspace import process as implementation
from recoverykit.workspace.process import ProcessError, ProcessResult, run_bounded


@unittest.skipUnless(os.name == "posix", "POSIX process groups required")
class BoundedProcessTests(unittest.TestCase):
    def setUp(self):
        self.children = []
        original = subprocess.Popen

        def record(*args, **kwargs):
            process = original(*args, **kwargs)
            self.children.append(process)
            return process

        patcher = patch.object(implementation.subprocess, "Popen", side_effect=record)
        patcher.start()
        self.addCleanup(patcher.stop)

    def run_script(self, script, **options):
        settings = dict(environment=os.environ.copy(), stdout_limit=1024,
                        stderr_limit=1024, timeout_seconds=2.0)
        settings.update(options)
        return run_bounded([sys.executable, "-c", script], **settings)

    def assert_reaped_and_closed(self):
        for child in self.children:
            self.assertIsNotNone(child.returncode)
            self.assertTrue(child.stdout.closed)
            self.assertTrue(child.stderr.closed)
            with self.assertRaises(ChildProcessError):
                os.waitpid(child.pid, os.WNOHANG)

    def test_success_exact_budgets_and_stderr_is_not_retained(self):
        result = self.run_script("import os; os.write(1, b'a' * 1024); os.write(2, b'b' * 1024)")
        self.assertEqual(result, ProcessResult(0, b"a" * 1024))
        self.assertFalse(hasattr(result, "stderr"))
        self.assert_reaped_and_closed()

    def test_stdout_limit_stops_before_child_completion_and_reaps(self):
        started = time.monotonic()
        with self.assertRaisesRegex(ProcessError, "^output_limit$"):
            self.run_script("import os,time; os.write(1, b'a' * 2048); time.sleep(30)")
        self.assertLess(time.monotonic() - started, 2.0)
        self.assertEqual(self.children[0].returncode, -signal.SIGKILL)
        self.assert_reaped_and_closed()

    def test_stderr_limit_is_enforced_without_retaining_secret_output(self):
        with self.assertRaisesRegex(ProcessError, "^output_limit$") as raised:
            self.run_script("import os,time; os.write(2, b'private-fixture' * 256); time.sleep(30)")
        self.assertNotIn("private-fixture", str(raised.exception))
        self.assertEqual(self.children[0].returncode, -signal.SIGKILL)
        self.assert_reaped_and_closed()

    def test_timeout_kills_even_when_sigterm_is_ignored(self):
        with self.assertRaisesRegex(ProcessError, "^timeout$"):
            self.run_script("import signal,time; signal.signal(signal.SIGTERM, signal.SIG_IGN); time.sleep(30)",
                            timeout_seconds=0.2)
        self.assertEqual(self.children[0].returncode, -signal.SIGKILL)
        self.assert_reaped_and_closed()

    def test_timeout_applies_after_both_output_streams_are_closed(self):
        with self.assertRaisesRegex(ProcessError, "^timeout$"):
            self.run_script("import os,time; os.close(1); os.close(2); time.sleep(30)", timeout_seconds=0.2)
        self.assert_reaped_and_closed()

    def test_regular_output_does_not_restart_the_total_deadline(self):
        with self.assertRaisesRegex(ProcessError, "^timeout$"):
            self.run_script("import os,time\nfor _ in range(100):\n os.write(1, b'x'); time.sleep(0.04)",
                            timeout_seconds=0.2)
        self.assert_reaped_and_closed()

    def test_inherited_pipe_descendant_cannot_extend_deadline(self):
        # The leader exits while its child inherits both pipes. No communicate
        # call may wait for that child's 30-second sleep after our deadline.
        original_killpg = os.killpg
        with patch.object(implementation.os, "killpg", wraps=original_killpg) as killpg:
            started = time.monotonic()
            with self.assertRaisesRegex(ProcessError, "^timeout$"):
                self.run_script("import subprocess,sys; subprocess.Popen([sys.executable, '-c', 'import time; time.sleep(30)'])",
                                timeout_seconds=0.3)
            self.assertLess(time.monotonic() - started, 2.0)
            killpg.assert_called_once_with(self.children[0].pid, signal.SIGKILL)
            self.assertNotEqual(self.children[0].pid, os.getpgrp())
        self.assert_reaped_and_closed()

    def test_nonzero_exit_is_preserved_and_reaped(self):
        with patch.object(implementation.os, "killpg", wraps=os.killpg) as killpg:
            result = self.run_script("import sys; sys.stdout.write('partial'); sys.exit(7)")
            # Never signal a numeric group ID after the leader has been reaped.
            killpg.assert_not_called()
        self.assertEqual(result, ProcessResult(7, b"partial"))
        self.assert_reaped_and_closed()

    def test_invalid_limits_and_timeout_fail_before_launch(self):
        for options in ({"stdout_limit": True}, {"stderr_limit": -1},
                        {"timeout_seconds": 0}, {"timeout_seconds": float("inf")}):
            with self.subTest(options=options), self.assertRaisesRegex(ProcessError, "^invalid_options$"):
                self.run_script("raise AssertionError('must not run')", **options)
        self.assertEqual(self.children, [])

    def test_unavailable_command_has_a_fixed_error(self):
        with self.assertRaisesRegex(ProcessError, "^unavailable$"):
            run_bounded(["/recovery-synthetic-no-such-command"], environment=os.environ.copy(), stdout_limit=1)
        self.assertEqual(self.children, [])


class GitProcessAdapterTests(unittest.TestCase):
    def test_adapter_preserves_optional_code_one_and_rejects_other_failures(self):
        with patch.object(git_module, "run_bounded", return_value=ProcessResult(1, b"")) as run:
            self.assertEqual(git_module.git(Path.cwd(), "config", optional=True), b"")
            with self.assertRaises(WorkspaceError):
                git_module.git(Path.cwd(), "config")
            self.assertEqual(run.call_args.kwargs["stdout_limit"], 64 * 1024 * 1024)
            self.assertEqual(run.call_args.kwargs["timeout_seconds"], 60)

    def test_adapter_maps_output_limit_without_echoing_child_data(self):
        with patch.object(git_module, "run_bounded", side_effect=ProcessError("output_limit")):
            with self.assertRaisesRegex(WorkspaceError, "^Git output exceeds the snapshot size limit$"):
                git_module.git(Path.cwd(), "diff")
