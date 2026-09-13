#!/usr/bin/env python3
"""Opt-in live smoke/compatibility suite using explicitly selected SSH Manager hosts.

Writes only in a unique remote /tmp directory guarded by an ownership marker.
Original TOML, SSH config, known_hosts, server login settings remain untouched.
Python 3.11+, OpenSSH and a built sshm are needed; no third-party Python packages.
"""
import argparse
import concurrent.futures
import fcntl
import hashlib
import json
import os
from pathlib import Path
import pty
import re
import select
import shlex
import shutil
import subprocess
import tempfile
import termios
import time
import tomllib
import uuid

ROOT = Path(__file__).resolve().parents[1]
HELPER = ROOT / "tests/toml_askpass.py"


def quote_config(value):
    value = str(value)
    if "\n" in value or "\r" in value or "\x00" in value:
        raise ValueError("invalid config value")
    return '"' + value.replace("\\", "\\\\").replace('"', '\\"') + '"'


def captured(argv, *, env, input=None, timeout=30, stdout=subprocess.PIPE):
    # subprocess.run(timeout=...) uses SIGKILL. Give sshm a chance to reap its
    # SSH children instead of making the harness itself create orphan clients.
    p = subprocess.Popen(argv, stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,
                         stdout=stdout, stderr=subprocess.PIPE, env=env)
    try:
        out, err = p.communicate(input=input, timeout=timeout)
    except subprocess.TimeoutExpired:
        p.terminate()
        try:
            p.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill()
            p.communicate(timeout=5)
        raise TimeoutError(f"test deadline {timeout}s exceeded; stopped this test process") from None
    return subprocess.CompletedProcess(argv, p.returncode, out, err)


class LiveTarget:
    def __init__(self, name, server, config, root, binary, results):
        self.name, self.server, self.binary, self.results = name, server, binary, results
        self.local = root / name
        self.local.mkdir(mode=0o700)
        self.runtime = self.local / "run"
        self.runtime.mkdir(mode=0o700)
        self.env = dict(os.environ, SSHM_RUNTIME_DIR=str(self.runtime), SSHM_TEST_TOML=str(config),
                        SSHM_TEST_TARGET=name, SSH_ASKPASS=str(HELPER), SSH_ASKPASS_REQUIRE="force")
        self.config = self.local / "ssh config"
        if server.get("proxy_jump") or server.get("proxy_command"):
            raise ValueError("live test bridge does not migrate proxy configuration")
        if server.get("mode", "unrestricted") != "unrestricted":
            raise ValueError("live suite requires explicit scope compatible with temporary file writes")
        lines = [f"Host {name}", f" HostName {quote_config(server['host'])}",
                 f" User {quote_config(server.get('user') or server['username'])}",
                 f" Port {int(server.get('port', 22))}", " StrictHostKeyChecking yes",
                 f" UserKnownHostsFile {quote_config(Path.home() / '.ssh/known_hosts')}",
                 " NumberOfPasswordPrompts 1", " ConnectTimeout 10", " IdentitiesOnly yes"]
        key = server.get("key_path") or server.get("keypath") or server.get("ssh_key")
        if key:
            lines.append(f" IdentityFile {quote_config(Path(key).expanduser())}")
        else:
            lines += [" IdentityFile none", " PreferredAuthentications password,keyboard-interactive"]
        self.config.write_text("\n".join(lines) + "\n")
        self.config.chmod(0o600)
        self.auth = ["--askpass", str(HELPER)] if server.get("password") or server.get("passphrase") else []
        self.remote = None
        self.owner = uuid.uuid4().hex
        self.os = None
        self.children = []
        self.transfer_timeout = 90

    def sanitize(self, value):
        value = str(value)
        for key in ("password", "passphrase", "sudo_password", "host", "user", "username"):
            secret = self.server.get(key)
            if isinstance(secret, str) and secret:
                value = value.replace(secret, "[redacted]")
        return value

    def argv(self, cmd, *args):
        return [str(self.binary), "exec", self.name, "-F", str(self.config),
                "--command", cmd, *self.auth, *args]

    def run(self, cmd, *args, input=None, timeout=30):
        return captured(self.argv(cmd, *args), input=input, env=self.env, timeout=timeout)

    def native(self, argv, input=None):
        return captured(argv, input=input, env=self.env)

    def assert_result(self, p, out=None, code=0, empty_stderr=True):
        if p.returncode != code:
            raise AssertionError(self.sanitize(f"exit={p.returncode}; stderr={p.stderr.decode(errors='replace')[:2000]}"))
        if out is not None and p.stdout != out:
            raise AssertionError(f"stdout mismatch: expected {len(out)} bytes, got {len(p.stdout)}")
        if empty_stderr and p.stderr:
            raise AssertionError(self.sanitize(p.stderr.decode(errors="replace")[:2000]))

    def case(self, name, fn):
        started = time.monotonic()
        try:
            note = fn()
            result = dict(host=self.name, case=name, status="passed", seconds=round(time.monotonic()-started, 3))
            if note:
                result["note"] = self.sanitize(note)
        except Exception as e:
            result = dict(host=self.name, case=name, status="failed", seconds=round(time.monotonic()-started, 3),
                          error=self.sanitize(f"{type(e).__name__}: {e}"))
        self.results.append(result)
        print(f"{self.name}: {name}: {result['status']} ({result['seconds']}s)", flush=True)
        if result["status"] == "failed":
            print(result["error"], flush=True)
        return result["status"] == "passed"

    def smoke(self):
        p = self.run("uname -s", "--persist", "0", "--timeout", "15s")
        self.assert_result(p)
        self.os = p.stdout.decode().strip()
        if self.os not in ("Darwin", "Linux"):
            raise AssertionError("this suite's POSIX file tests require a known Darwin/Linux target")
        return self.os

    def create_workspace(self):
        p = self.run("mktemp -d /tmp/sshm-live-XXXXXXXX")
        self.assert_result(p)
        remote = p.stdout.decode().strip()
        if not re.fullmatch(r"/tmp/sshm-live-[A-Za-z0-9]+", remote):
            raise AssertionError("unexpected mktemp path; refusing to mutate it")
        self.remote = remote
        self.assert_result(self.run(f"printf '%s' {shlex.quote(self.owner)} > {shlex.quote(remote+'/.sshm-owner')}"))

    def raw_exit(self):
        self.assert_result(self.run("printf 'stdout'; printf 'stderr' >&2; exit 17"), b"stdout", 17, False)
        for code in (0, 1, 2, 124, 125, 130):
            self.assert_result(self.run(f"exit {code}"), b"", code)

    def quoting(self):
        text = "中文 ' \" $HOME `data` $(printf data) \\ line\nsecond line"
        self.assert_result(self.run("printf '%s' " + shlex.quote(text)), text.encode())

    def inputs(self):
        self.assert_result(self.run("cat"), b"")
        content = "认证与 stdin 分离\n'\"\\".encode() + b"\x00\xff"
        self.assert_result(self.run("cat", "--stdin", "-", input=content), content)
        path = self.local / "stdin 中文 file"
        path.write_bytes(content)
        self.assert_result(self.run("cat", "--stdin", str(path)), content)

    def script(self):
        script = b"set -eu\nprintf 'one\\n'\nprintf 'two\\n'\nexit 23\n"
        self.assert_result(self.run("sh -s", "--stdin", "-", input=script), b"one\ntwo\n", 23)

    def independent_shells(self):
        self.assert_result(self.run(f"cd {shlex.quote(self.remote)}; export SSHM_TEST_STATE=yes; pwd"), (self.remote+"\n").encode())
        p = self.run("printf '%s' \"${SSHM_TEST_STATE-unset}\"")
        self.assert_result(p, b"unset")

    def limited_output(self):
        p = self.run("head -c 1048576 /dev/zero; head -c 8192 /dev/zero >&2", "--max-output", "1024",
                     "--timeout", f"{self.transfer_timeout}s", timeout=self.transfer_timeout+10)
        self.assert_result(p, b"\0"*1024, empty_stderr=False)
        if b"1047552" not in p.stderr or b"7168" not in p.stderr:
            raise AssertionError("incorrect truncation accounting")

    def save_output(self):
        path = self.local / "full-output"
        with path.open("wb") as out:
            p = captured(self.argv("head -c 1048576 /dev/zero", "--max-output", "0",
                                   "--timeout", f"{self.transfer_timeout}s"),
                         stdout=out, env=self.env, timeout=self.transfer_timeout+10)
        self.assert_result(p)
        if path.stat().st_size != 1048576:
            raise AssertionError("full output was truncated")

    def sockets(self):
        return [p for p in self.runtime.glob("c-*") if p.is_socket()]

    def close_connections(self):
        for path in self.sockets():
            p = subprocess.run(["ssh", "-F", "/dev/null", "-S", str(path), "-O", "exit", "unused"],
                               capture_output=True, timeout=3)
            if p.returncode:
                raise AssertionError("failed to close a test control socket")

    def concurrent(self):
        self.close_connections()
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            values = list(pool.map(lambda n: self.run(f"sleep 0.2; printf 'job-{n}'"), range(4)))
        for n, p in enumerate(values):
            self.assert_result(p, f"job-{n}".encode())
        if len(self.sockets()) != 1:
            raise AssertionError("expected one managed control socket")
        return "4 concurrent commands, one managed control socket"

    def repeated(self):
        for i in range(20):
            self.assert_result(self.run(f"printf 'repeat-{i}'"), f"repeat-{i}".encode())
        if len(self.sockets()) != 1:
            raise AssertionError("control socket count grew after repeated calls")
        return "20 sequential calls, one managed control socket"

    def deadline(self):
        sibling = subprocess.Popen(self.argv("sleep 1; printf sibling"), stdout=subprocess.PIPE,
                                   stderr=subprocess.PIPE, stdin=subprocess.DEVNULL, env=self.env)
        self.children.append(sibling)
        time.sleep(0.15)
        p = self.run("sleep 2", "--timeout", "350ms")
        self.assert_result(p, code=124, empty_stderr=False)
        if "远端执行结果未知" not in p.stderr.decode():
            raise AssertionError("missing unknown-outcome diagnostic")
        out, err = sibling.communicate(timeout=10)
        if sibling.returncode != 0 or out != b"sibling" or err:
            raise AssertionError(self.sanitize(f"sibling interrupted: {sibling.returncode}: {err!r}"))

    def idle(self):
        self.close_connections()
        self.assert_result(self.run("printf ttl", "--persist", "2s"), b"ttl")
        paths = self.sockets()
        if len(paths) != 1:
            raise AssertionError("no short-lived control socket")
        deadline = time.monotonic() + 8
        while any(p.exists() for p in paths) and time.monotonic() < deadline:
            time.sleep(0.1)
        if any(p.exists() for p in paths):
            raise AssertionError("idle socket did not expire")

    def streamed_text(self):
        p = self.run("printf 'first\\n'; sleep 0.2; printf 'last\\n'")
        self.assert_result(p, b"first\nlast\n")

    def transfer(self):
        data = os.urandom(32768) + "文件往返\n".encode()
        src, dst = self.local / "local 中文 file.txt", self.local / "download 中文 file.txt"
        src.write_bytes(data)
        remote = self.remote + "/remote 中文 file.txt"
        base = ["scp", "-q", "-F", str(self.config)]
        self.assert_result(self.native(base + [str(src), f"{self.name}:{remote}"]))
        self.assert_result(self.native(base + [f"{self.name}:{remote}", str(dst)]))
        if dst.read_bytes() != data:
            raise AssertionError("scp hash mismatch")
        return "native scp/SFTP backend, 32 KiB binary plus Unicode, exact round trip"

    def sftp(self):
        data = b"sftp batch content\n"
        src, dst = self.local / "sftp-local", self.local / "sftp-download"
        src.write_bytes(data)
        remote = self.remote + "/sftp-remote"
        batch = f'put "{src}" "{remote}"\nget "{remote}" "{dst}"\n'.encode()
        p = self.native(["sftp", "-q", "-F", str(self.config), "-o", "BatchMode=no", "-b", "-", self.name], input=batch)
        self.assert_result(p)
        if dst.read_bytes() != data:
            raise AssertionError("sftp content mismatch")

    def rsync(self):
        if not shutil.which("rsync") or self.run("command -v rsync >/dev/null").returncode != 0:
            return "not available on both ends; native rsync scenario omitted"
        source = self.local / "sync-source"
        source.mkdir()
        (source / "alpha").write_text("initial\n")
        (source / "文件.txt").write_text("unicode\n")
        remote = self.remote + "/sync-target"
        self.assert_result(self.run(f"mkdir {shlex.quote(remote)}"))
        ssh = shlex.join(["ssh", "-F", str(self.config), "-o", "BatchMode=no"])
        base = ["rsync", "-r", "--checksum", "-e", ssh]
        self.assert_result(self.native(base + ["--dry-run", str(source)+"/", f"{self.name}:{remote}/"]))
        self.assert_result(self.run(f"test ! -e {shlex.quote(remote+'/alpha')}"))
        self.assert_result(self.native(base + [str(source)+"/", f"{self.name}:{remote}/"]))
        self.assert_result(self.run(f"cat {shlex.quote(remote+'/alpha')}"), b"initial\n")
        (source / "alpha").write_text("updated\n")
        self.assert_result(self.native(base + [str(source)+"/", f"{self.name}:{remote}/"]))
        self.assert_result(self.run(f"cat {shlex.quote(remote+'/alpha')}"), b"updated\n")

    def terminal(self):
        master, slave = pty.openpty()

        def setup():
            os.setsid()
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)

        # Force sh for this controlled scenario, avoiding writes to interactive
        # login-shell history. The user-facing interface remains native ssh.
        p = subprocess.Popen(["ssh", "-tt", "-F", str(self.config), self.name, "sh"],
                             stdin=slave, stdout=slave, stderr=slave, env=self.env, preexec_fn=setup)
        self.children.append(p)
        os.close(slave)
        received = bytearray()

        def until(marker):
            deadline = time.monotonic()+15
            while marker not in received:
                if time.monotonic() > deadline:
                    raise AssertionError("interactive terminal did not reach expected marker")
                if select.select([master], [], [], 0.1)[0]:
                    data = os.read(master, 8192)
                    if not data:
                        raise AssertionError("interactive terminal closed early")
                    received.extend(data)

        try:
            os.write(master, b"printf 'READY=%s\\n' yes\n")
            until(b"READY=yes\r\n")
            os.write(master, ("cd "+shlex.quote(self.remote)+"\npwd\n").encode())
            until((self.remote+"\r\n").encode())
            os.write(master, b"read -r answer; printf 'ANSWER=%s\\n' \"$answer\"\n")
            time.sleep(0.1)
            os.write(master, b"agent-reply\n")
            until(b"ANSWER=agent-reply\r\n")
            os.write(master, b"exit\n")
            deadline = time.monotonic()+8
            while p.poll() is None and time.monotonic()<deadline:
                if select.select([master], [], [], 0.1)[0]:
                    try:
                        if not os.read(master, 8192):
                            break
                    except OSError:
                        break
            if p.wait(timeout=2) != 0:
                raise AssertionError("interactive ssh failed")
        finally:
            if p.poll() is None:
                p.terminate()
                p.wait(timeout=3)
            os.close(master)

    def cleanup(self):
        for p in self.children:
            if p.poll() is None:
                p.terminate()
                p.communicate(timeout=5)
        if self.remote:
            remote, owner = shlex.quote(self.remote), shlex.quote(self.owner)
            cmd = f'test "$(cat {remote}/.sshm-owner)" = {owner} && rm -rf -- {remote}'
            self.assert_result(self.run(cmd))
            self.assert_result(self.run(f"test ! -e {remote}"))
            self.remote = None
        self.close_connections()
        if self.sockets():
            raise AssertionError("test control socket remains after cleanup")

    def suite(self, selected=None):
        if not self.case("password-auth-and-os", self.smoke):
            return
        try:
            if not self.case("temporary-workspace", self.create_workspace):
                return
            for name, fn in [
                ("raw-streams-and-exit-codes", self.raw_exit), ("unicode-and-shell-quoting", self.quoting),
                ("stdin-eof-pipe-file-binary", self.inputs), ("script-over-stdin", self.script),
                ("independent-shell-state", self.independent_shells), ("output-limits", self.limited_output),
                ("complete-output-to-file", self.save_output), ("concurrent-cold-calls", self.concurrent),
                ("20-repeated-calls", self.repeated), ("timeout-sibling-isolation", self.deadline),
                ("idle-connection-expiry", self.idle), ("delayed-text-output", self.streamed_text),
                ("native-scp-roundtrip", self.transfer), ("native-sftp-batch", self.sftp),
                ("native-rsync-dry-run-and-update", self.rsync), ("native-interactive-terminal", self.terminal),
            ]:
                if selected and name not in selected:
                    continue
                self.case(name, fn)
        finally:
            self.case("remote-and-local-cleanup", self.cleanup)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--toml", required=True, type=Path)
    parser.add_argument("--hosts", required=True, nargs="+")
    parser.add_argument("--binary", type=Path, default=ROOT / "bin/sshm")
    parser.add_argument("--report", type=Path)
    parser.add_argument("--cases", nargs="+", help="只重测指定场景；仍执行认证、临时目录和清理")
    parser.add_argument("--transfer-timeout", type=int, default=90, help="1 MiB 大输出场景的本地期限（秒）")
    args = parser.parse_args()
    if args.transfer_timeout <= 0:
        parser.error("transfer timeout must be positive")
    config = args.toml.expanduser().resolve()
    raw = config.read_bytes()
    servers = tomllib.loads(raw.decode())["ssh_servers"]
    known = Path.home()/".ssh/known_hosts"
    known_hash = hashlib.sha256(known.read_bytes()).hexdigest()
    names = list(dict.fromkeys(args.hosts))
    if any(not re.fullmatch(r"[A-Za-z0-9_.-]+", h) or h not in servers for h in names):
        parser.error("hosts must be explicit aliases present in the TOML")
    results = []
    with tempfile.TemporaryDirectory(prefix="sshm-live-", dir="/tmp") as tmp:
        for name in names:
            target = LiveTarget(name, servers[name], config, Path(tmp), args.binary.resolve(), results)
            target.transfer_timeout = args.transfer_timeout
            target.suite(args.cases)
    unchanged = raw == config.read_bytes() and known_hash == hashlib.sha256(known.read_bytes()).hexdigest()
    report = dict(results=results, original_config_and_known_hosts_unchanged=unchanged,
                  passed=sum(r["status"] == "passed" for r in results), failed=sum(r["status"] == "failed" for r in results))
    if args.report:
        args.report.parent.mkdir(parents=True, exist_ok=True)
        args.report.write_text(json.dumps(report, ensure_ascii=False, indent=2)+"\n")
    print(f"passed={report['passed']} failed={report['failed']} original_files_unchanged={unchanged}")
    return 0 if not report["failed"] and unchanged else 1


if __name__ == "__main__":
    raise SystemExit(main())
