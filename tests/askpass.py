#!/usr/bin/env python3
"""Exercise the optional production credential bridge with synthetic secrets."""
import os
from pathlib import Path
import subprocess
import sys
import tempfile
import unittest

HELPER = Path(__file__).resolve().parents[1] / "scripts/ssh-manager-askpass.py"


class Askpass(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.config = Path(self.temp.name) / "credentials.toml"
        self.config.write_text('''[ssh_servers.jump]
host = "192.0.2.1"
user = "jumpuser"
password = "jump-test-secret"
[ssh_servers.target]
host = "192.0.2.2"
user = "targetuser"
password = "target-test-secret"
key_path = "/tmp/test key"
passphrase = "key-test-secret"
''')
        self.config.chmod(0o600)

    def call(self, prompt, expected=None, **env):
        p = subprocess.run([sys.executable, str(HELPER), prompt], capture_output=True,
                           env=dict(os.environ, SSHM_CREDENTIALS=str(self.config), **env), timeout=3)
        self.assertEqual(p.stderr, b"")
        self.assertEqual(p.returncode, 0 if expected else 1)
        self.assertEqual(p.stdout, (expected + "\n").encode() if expected else b"")

    def test_jump_and_target_select_different_passwords(self):
        self.call("jumpuser@192.0.2.1's password: ", "jump-test-secret")
        self.call("targetuser@192.0.2.2's password: ", "target-test-secret")

    def test_interactive_password_and_passphrase(self):
        self.call("(targetuser@192.0.2.2) Password:", "target-test-secret")
        self.call("Enter passphrase for key '/tmp/test key': ", "key-test-secret")

    def test_unknown_endpoint_user_key_and_mfa_rejected(self):
        for prompt in ("targetuser@192.0.2.3's password: ", "other@192.0.2.2's password: ",
                       "Password:", "Verification code:", "Enter passphrase for key '/tmp/other': "):
            self.call(prompt)

    def test_confirm_rejected_even_for_password_shaped_prompt(self):
        self.call("targetuser@192.0.2.2's password: ", SSH_ASKPASS_PROMPT="confirm")

    def test_conflicting_entries_rejected(self):
        with self.config.open("a") as f:
            f.write('\n[ssh_servers.duplicate]\nhost="192.0.2.2"\nuser="targetuser"\npassword="different"\n')
        self.call("targetuser@192.0.2.2's password: ")

    def test_readable_credentials_and_invalid_toml_rejected(self):
        self.config.chmod(0o644)
        self.call("targetuser@192.0.2.2's password: ")
        self.config.chmod(0o600)
        self.config.write_text("invalid TOML")
        self.call("targetuser@192.0.2.2's password: ")


if __name__ == "__main__":
    unittest.main(verbosity=2)
