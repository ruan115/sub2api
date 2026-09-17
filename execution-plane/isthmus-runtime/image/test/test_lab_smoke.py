import unittest
from imagekit.lock import ImageError, load_lock
from lab.smoke import smoke_script
from pathlib import Path
from test.test_lock import fixture


class SmokeScriptTests(unittest.TestCase):
    def test_full_lock_is_checked_before_success_and_audit_must_be_empty(self):
        lock = load_lock(Path(__file__).resolve().parents[1] / "locks/base-linux-amd64-2026-09-17.json")
        script = smoke_script(lock)
        self.assertEqual(script.count("${Version} ${Architecture} ${Status}"), 9)
        self.assertIn("2:4.0.4-9 amd64 install ok installed", script)
        self.assertIn('audit="$(dpkg --audit)"\ntest -z "$audit"', script)
        self.assertTrue(script.endswith("echo base-only-smoke-pass"))
        self.assertEqual(script.count("base-only-smoke-pass"), 1)

    def test_invalid_lock_cannot_inject_a_shell_command(self):
        lock = fixture()
        lock["packages"][0]["name"] = "synthetic; exit 0"
        with self.assertRaises(ImageError):
            smoke_script(lock)
