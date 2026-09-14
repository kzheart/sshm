#!/usr/bin/env python3
"""Optional OpenSSH askpass bridge for an existing SSH Manager TOML (Python 3.11+).

OpenSSH invokes this program; stdout is a private credential pipe, not a CLI UI.
Matches the requested endpoint, including ProxyJump authentication, instead of
returning the same password for every prompt. Does not accept host-key prompts.
"""
import os
from pathlib import Path
import re
import stat
import sys
import tomllib


def credential(servers, prompt):
    match = re.fullmatch(r"([^@\r\n]+)@([^\r\n]+)'s [Pp]assword: ?", prompt)
    if match is None:
        match = re.fullmatch(r"\(([^@\r\n]+)@([^\r\n]+)\) [Pp]assword: ?", prompt)
    candidates = set()
    if match:
        user, host = match.groups()
        for alias, server in servers.items():
            names = {alias, server.get("host")}
            if server.get("host"):
                names.add(f"[{server['host']}]:{server.get('port', 22)}")
            if user == (server.get("user") or server.get("username")) and host in names:
                candidates.add(server.get("password"))
    else:
        match = re.fullmatch(r"Enter passphrase for key '([^\r\n]+)': ?", prompt)
        if match:
            path = os.path.abspath(os.path.expanduser(match[1]))
            for server in servers.values():
                key = server.get("key_path") or server.get("keypath") or server.get("ssh_key")
                if key and os.path.abspath(os.path.expanduser(key)) == path:
                    candidates.add(server.get("passphrase"))
    # Conflicting entries and missing credentials fail closed, even when a
    # different entry has a password for the same endpoint.
    if len(candidates) != 1:
        return None
    value = candidates.pop()
    if not isinstance(value, str) or not value or any(c in value for c in "\r\n\x00"):
        return None
    return value


def main():
    if len(sys.argv) != 2 or os.environ.get("SSH_ASKPASS_PROMPT") == "confirm":
        return 1
    config = Path(os.environ.get("SSHM_CREDENTIALS", "~/.ssh/sshm.toml")).expanduser()
    try:
        with config.open("rb") as stream:
            info = os.fstat(stream.fileno())
            if not stat.S_ISREG(info.st_mode) or info.st_uid != os.getuid() or info.st_mode & 0o077:
                return 1
            servers = tomllib.load(stream)["ssh_servers"]
        value = credential(servers, sys.argv[1])
        if value is None:
            return 1
        sys.stdout.write(value + "\n")
        return 0
    except Exception:
        # Never include config values, prompts, or exception contents in logs.
        return 1


if __name__ == "__main__":
    sys.exit(main())
