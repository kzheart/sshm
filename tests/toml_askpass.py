#!/usr/bin/env python3
"""Test-only bridge: give OpenSSH one selected TOML credential, never log it."""
import os
from pathlib import Path
import sys
import tomllib


def main():
    if os.environ.get("SSH_ASKPASS_PROMPT") == "confirm":
        return 1
    prompt = " ".join(sys.argv[1:]).lower()
    if "password" not in prompt and "passphrase" not in prompt:
        return 1
    try:
        config = tomllib.loads(Path(os.environ["SSHM_TEST_TOML"]).read_text())
        server = config["ssh_servers"][os.environ["SSHM_TEST_TARGET"]]
        key = "passphrase" if "passphrase" in prompt else "password"
        secret = server.get(key)
        if not isinstance(secret, str) or not secret or "\n" in secret or "\r" in secret:
            return 1
        # Only OpenSSH reads this pipe. Do not print prompts, config, or errors.
        sys.stdout.write(secret + "\n")
        return 0
    except Exception:
        return 1


if __name__ == "__main__":
    sys.exit(main())
