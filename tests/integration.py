#!/usr/bin/env python3
"""Real OpenSSH integration tests against an ephemeral, loopback-only container.

Requires Docker, Go, Python 3, ssh and ssh-keygen. No developer SSH config,
credentials or remote servers are used. All test connections use pinned keys.
"""
import concurrent.futures
import fcntl
import os
from pathlib import Path
import pty
import re
import select
import shlex
import shutil
import socket
import subprocess
import tempfile
import termios
import time
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / "bin/sshm"
IMAGE = os.environ.get("SSHM_TEST_IMAGE", "sshm-test-server:local")


def checked(args, **kwargs):
    return subprocess.run(args, check=True, capture_output=True, **kwargs)


def ssh_quote(value):
    return '"' + str(value).replace('\\', '\\\\').replace('"', '\\"') + '"'


class SSHIntegration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        checked(["go", "build", "-trimpath", "-o", str(BINARY), "."], cwd=ROOT)
        checked(["docker", "image", "inspect", IMAGE])
        architecture = checked(["docker", "image", "inspect", "--format", "{{.Architecture}}", IMAGE], text=True).stdout.strip()
        cls.linux_binary = ROOT / "bin" / f"sshm-linux-{architecture}"
        checked(["go", "build", "-trimpath", "-o", str(cls.linux_binary), "."], cwd=ROOT,
                env=dict(os.environ, GOOS="linux", GOARCH=architecture, CGO_ENABLED="0"))
        cls.fixture = Path(tempfile.mkdtemp(prefix="sshm-fixture-", dir="/tmp"))
        cls.container = "sshm-test-" + uuid.uuid4().hex[:10]
        cls.addClassCleanup(shutil.rmtree, cls.fixture, True)
        cls.addClassCleanup(lambda: subprocess.run(
            ["docker", "rm", "-f", cls.container], capture_output=True, timeout=20))
        for key in ("client", "host"):
            checked(["ssh-keygen", "-q", "-t", "ed25519", "-N", "", "-f", str(cls.fixture / key)])
        checked([
            "docker", "run", "-d", "--name", cls.container,
            "-p", "127.0.0.1::2222",
            "-v", f"{cls.linux_binary}:/usr/local/bin/sshm:ro",
            "-v", f"{cls.fixture}:/fixture:ro",
            "-v", f"{cls.fixture / 'client.pub'}:/etc/ssh/sshm_test_authorized_keys:ro",
            "-v", f"{cls.fixture / 'host'}:/etc/ssh/sshm_test_hostkey:ro", IMAGE,
        ])
        binding = checked(["docker", "port", cls.container, "2222/tcp"], text=True).stdout.strip()
        cls.port = int(binding.rsplit(":", 1)[1])
        deadline = time.monotonic() + 15
        while True:
            try:
                with socket.create_connection(("127.0.0.1", cls.port), timeout=1) as s:
                    if s.recv(100).startswith(b"SSH-"):
                        break
            except OSError:
                pass
            if time.monotonic() > deadline:
                logs = checked(["docker", "logs", cls.container], text=True)
                raise RuntimeError(logs.stdout + logs.stderr)
            time.sleep(0.1)
        public = (cls.fixture / "host.pub").read_text().split()
        known = cls.fixture / "known_hosts"
        known.write_text(f"[127.0.0.1]:{cls.port} {public[0]} {public[1]}\n")
        cls.config = cls.fixture / "ssh config"
        cls.config.write_text(
            "Host fixture\n"
            " HostName 127.0.0.1\n"
            f" Port {cls.port}\n"
            " User tester\n"
            f" IdentityFile {ssh_quote(cls.fixture / 'client')}\n"
            " IdentitiesOnly yes\n"
            f" UserKnownHostsFile {ssh_quote(known)}\n"
            " GlobalKnownHostsFile /dev/null\n"
            " StrictHostKeyChecking yes\n"
        )
        (cls.fixture / "linux-known-hosts").write_text(f"[127.0.0.1]:2222 {public[0]} {public[1]}\n")
        (cls.fixture / "linux-config").write_text(
            "Host fixture\n HostName 127.0.0.1\n Port 2222\n User tester\n"
            " IdentityFile /fixture/client\n IdentitiesOnly yes\n"
            " UserKnownHostsFile /fixture/linux-known-hosts\n"
            " GlobalKnownHostsFile /dev/null\n StrictHostKeyChecking yes\n")

    def setUp(self):
        self.runtime = Path(tempfile.mkdtemp(prefix="sshm-it-", dir="/tmp"))
        self.env = dict(os.environ, SSHM_RUNTIME_DIR=str(self.runtime))
        self.children = []

    def tearDown(self):
        for child in self.children:
            if child.poll() is None:
                child.terminate()
                try:
                    child.communicate(timeout=3)
                except subprocess.TimeoutExpired:
                    child.kill()
                    child.communicate(timeout=3)
        # Only close masters belonging to this test's private directory.
        for path in self.sockets():
            subprocess.run(["ssh", "-F", "/dev/null", "-S", str(path), "-O", "exit", "unused"],
                           capture_output=True, timeout=3)
        shutil.rmtree(self.runtime)

    def sockets(self):
        return [p for p in self.runtime.glob("c-*") if p.is_socket()]

    def argv(self, command, *options):
        return [str(BINARY), "exec", "fixture", "-F", str(self.config), "--command", command, *options]

    def run_ssh(self, command, *options, input=None):
        return subprocess.run(self.argv(command, *options), input=input, capture_output=True,
                              env=self.env, timeout=15)

    def start(self, command, *options):
        p = subprocess.Popen(self.argv(command, *options), stdin=subprocess.DEVNULL,
                             stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=self.env)
        self.children.append(p)
        return p

    def wait_master(self):
        deadline = time.monotonic() + 5
        while time.monotonic() < deadline:
            paths = self.sockets()
            if paths:
                return paths[0]
            time.sleep(0.02)
        self.fail("no control socket published")

    def master_pid(self, path):
        p = checked(["ssh", "-F", "/dev/null", "-S", str(path), "-O", "check", "unused"])
        return int(re.search(rb"pid=(\d+)", p.stderr).group(1))

    def accepted_connections(self):
        logs = checked(["docker", "logs", self.container])
        return (logs.stdout + logs.stderr).count(b"Accepted publickey for tester")

    def test_01_raw_output_exit_codes_and_no_local_shell(self):
        literal = "引号 ' \" $HOME `literal` $(touch /tmp/sshm-should-not-exist)"
        p = self.run_ssh("printf '%s' " + shlex.quote(literal) + "; printf 'err' >&2; exit 17")
        self.assertEqual(p.returncode, 17, p.stderr)
        self.assertEqual(p.stdout.decode(), literal)
        self.assertEqual(p.stderr, b"err")
        self.assertEqual(self.run_ssh("exit 125").stderr, b"")
        p = self.run_ssh("exit 255")
        self.assertEqual(p.returncode, 255)
        self.assertIn("无法确认", p.stderr.decode())

    def test_02_stdin_eof_pipe_file_and_binary(self):
        self.assertEqual(self.run_ssh("cat").stdout, b"")
        content = "中文 stdin\n'\"$`".encode() + b"\x00\xff"
        p = self.run_ssh("cat", "--stdin", "-", "--max-output", "0", input=content)
        self.assertEqual((p.returncode, p.stdout, p.stderr), (0, content, b""))
        path = self.runtime / "input with spaces"
        path.write_bytes(content)
        self.assertEqual(self.run_ssh("cat", "--stdin", str(path)).stdout, content)

    def test_03_large_output_is_drained_not_buffered(self):
        p = self.run_ssh("head -c 20000000 /dev/zero; printf 'finished' >&2", "--max-output", "4096")
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual(len(p.stdout), 4096)
        self.assertIn(b"finished", p.stderr)
        self.assertIn("19995904".encode(), p.stderr)
        with (self.runtime / "full-output").open("wb") as output:
            p = subprocess.run(self.argv("head -c 2000000 /dev/zero", "--max-output", "0"),
                               stdout=output, stderr=subprocess.PIPE, env=self.env, timeout=10)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertEqual((self.runtime / "full-output").stat().st_size, 2000000)

    def test_04_concurrent_cold_calls_share_one_transport(self):
        before = self.accepted_connections()
        started = time.monotonic()
        with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
            results = list(pool.map(lambda _: self.run_ssh("sleep 0.4; printf done"), range(8)))
        duration = time.monotonic() - started
        for p in results:
            self.assertEqual((p.returncode, p.stdout, p.stderr), (0, b"done", b""))
        self.assertEqual(len(self.sockets()), 1)
        self.assertEqual(self.accepted_connections() - before, 1)
        self.assertLess(duration, 3.5, "commands were serialized")
        print(f"\n  8 cold concurrent calls: {duration:.3f}s, one SSH transport", flush=True)

    def test_05_reuse_and_idle_expiry(self):
        p = self.run_ssh("printf first", "--persist", "2s")
        self.assertEqual(p.returncode, 0, p.stderr)
        path = self.wait_master()
        pid = self.master_pid(path)
        p = self.run_ssh("printf second", "--persist", "2s")
        self.assertEqual(p.stdout, b"second")
        self.assertEqual(self.master_pid(path), pid)
        deadline = time.monotonic() + 8
        while path.exists() and time.monotonic() < deadline:
            time.sleep(0.1)
        self.assertFalse(path.exists(), "idle master/socket did not expire")

    def test_06_active_command_outlives_idle_ttl(self):
        p = self.run_ssh("sleep 2; printf still-running", "--persist", "1s")
        self.assertEqual((p.returncode, p.stdout), (0, b"still-running"), p.stderr)

    def test_07_timeout_is_explicit_and_does_not_break_sibling(self):
        sibling = self.start("sleep 1.5; printf sibling-done")
        self.wait_master()
        p = self.run_ssh("sleep 10", "--timeout", "250ms")
        self.assertEqual(p.returncode, 124, p.stderr)
        self.assertIn("远端执行结果未知", p.stderr.decode())
        out, err = sibling.communicate(timeout=5)
        self.assertEqual((sibling.returncode, out, err), (0, b"sibling-done", b""))

    def test_08_sigterm_reaps_foreground_ssh(self):
        p = self.start("sleep 10", "--persist", "0")
        time.sleep(0.35)
        children = checked(["ps", "-axo", "pid,ppid"], text=True).stdout.splitlines()[1:]
        ssh_children = [int(x.split()[0]) for x in children if x.split()[1] == str(p.pid)]
        self.assertTrue(ssh_children, "did not observe running SSH child")
        p.terminate()
        _, err = p.communicate(timeout=3)
        self.assertEqual(p.returncode, 130, err)
        for child in ssh_children:
            with self.assertRaises(ProcessLookupError):
                os.kill(child, 0)

    def test_09_no_reuse_and_configured_side_effects_disabled(self):
        config = self.runtime / "overrides"
        config.write_text(self.config.read_text() +
                          " RemoteCommand sleep 20\n RequestTTY force\n SessionType none\n"
                          " ForkAfterAuthentication yes\n LocalCommand touch /tmp/sshm-unwanted-local\n"
                          " PermitLocalCommand yes\n LocalForward 127.0.0.1:1 localhost:1\n")
        p = self.run_ssh("printf explicit", "-F", str(config), "--persist", "0")
        self.assertEqual((p.returncode, p.stdout, p.stderr), (0, b"explicit", b""))
        self.assertEqual(self.sockets(), [])

    def test_10_doctor_is_local_and_reports_own_master(self):
        self.assertEqual(self.run_ssh("true").returncode, 0)
        before = self.accepted_connections()
        p = subprocess.run([str(BINARY), "doctor", "-F", str(self.config)], capture_output=True,
                           env=self.env, timeout=5)
        self.assertEqual(p.returncode, 0, p.stderr)
        self.assertIn("控制 socket 数: 1", p.stdout.decode())
        self.assertEqual(self.accepted_connections(), before)

    def test_11_native_terminal_supports_multi_turn_input(self):
        master, slave = pty.openpty()

        def controlling_terminal():
            os.setsid()
            fcntl.ioctl(0, termios.TIOCSCTTY, 0)

        p = subprocess.Popen(["ssh", "-tt", "-F", str(self.config), "fixture"],
                             stdin=slave, stdout=slave, stderr=slave, preexec_fn=controlling_terminal)
        self.children.append(p)
        os.close(slave)
        self.addCleanup(os.close, master)
        received = bytearray()

        def read_until(marker, timeout=5):
            deadline = time.monotonic() + timeout
            while marker not in received:
                if time.monotonic() > deadline:
                    self.fail(f"terminal did not produce {marker!r}: {received[-2000:]!r}")
                if select.select([master], [], [], 0.1)[0]:
                    data = os.read(master, 8192)
                    if not data:
                        self.fail("terminal closed early")
                    received.extend(data)

        os.write(master, b"printf 'READY=%s\\n' yes\n")
        read_until(b"READY=yes\r\n")
        os.write(master, b"cd /tmp\npwd\n")
        read_until(b"/tmp\r\n")
        os.write(master, b"read -r answer; printf 'ANSWER=%s\\n' \"$answer\"\n")
        time.sleep(0.1)
        os.write(master, b"agent-reply\n")
        read_until(b"ANSWER=agent-reply\r\n")
        os.write(master, b"exit\n")
        deadline = time.monotonic() + 5
        while p.poll() is None and time.monotonic() < deadline:
            if select.select([master], [], [], 0.1)[0]:
                try:
                    received.extend(os.read(master, 8192))
                except OSError:
                    break
        self.assertEqual(p.wait(timeout=1), 0, received[-2000:])

    def test_12_host_key_mismatch_fails_without_prompt(self):
        wrong = self.runtime / "wrong-known-hosts"
        key = (self.fixture / "client.pub").read_text().split()
        wrong.write_text(f"[127.0.0.1]:{self.port} {key[0]} {key[1]}\n")
        config = self.runtime / "wrong-config"
        config.write_text(self.config.read_text().replace(
            ssh_quote(self.fixture / "known_hosts"), ssh_quote(wrong)))
        p = self.run_ssh("printf must-not-execute", "-F", str(config))
        self.assertEqual(p.returncode, 255, p.stderr)
        self.assertEqual(p.stdout, b"")
        self.assertEqual(self.sockets(), [])

    def test_13_proxyjump_uses_native_ssh_config(self):
        public = (self.fixture / "host.pub").read_text().split()
        known = self.runtime / "jump-known-hosts"
        known.write_text(f"internal-fixture {public[0]} {public[1]}\n")
        config = self.runtime / "jump-config"
        config.write_text(self.config.read_text() +
                          "Host via-jump\n HostName 127.0.0.1\n Port 2222\n User tester\n"
                          f" IdentityFile {ssh_quote(self.fixture / 'client')}\n"
                          " IdentitiesOnly yes\n ProxyJump fixture\n HostKeyAlias internal-fixture\n"
                          f" UserKnownHostsFile {ssh_quote(known)}\n StrictHostKeyChecking yes\n")
        p = subprocess.run([str(BINARY), "exec", "via-jump", "-F", str(config),
                            "--command", "printf jumped"], capture_output=True, env=self.env, timeout=10)
        self.assertEqual((p.returncode, p.stdout, p.stderr), (0, b"jumped", b""))

    def test_14_closed_output_pipe_cancels_foreground(self):
        p = self.start("yes", "--persist", "0", "--max-output", "0")
        self.assertTrue(p.stdout.read(10))
        children = checked(["ps", "-axo", "pid,ppid"], text=True).stdout.splitlines()[1:]
        ssh_children = [int(x.split()[0]) for x in children if x.split()[1] == str(p.pid)]
        p.stdout.close()
        p.stdout = None
        _, err = p.communicate(timeout=3)
        self.assertEqual(p.returncode, 125, err)
        self.assertIn("输出写入失败", err.decode())
        for child in ssh_children:
            with self.assertRaises(ProcessLookupError):
                os.kill(child, 0)

    def test_15_large_stream_has_bounded_cli_memory(self):
        p = self.start("i=0; while [ $i -lt 20 ]; do head -c 1000000 /dev/zero; sleep 0.05; i=$((i+1)); done",
                       "--max-output", "1024")
        peak_kib = 0
        while p.poll() is None:
            snapshot = subprocess.run(["ps", "-o", "rss=", "-p", str(p.pid)], capture_output=True, text=True)
            if snapshot.stdout.strip():
                peak_kib = max(peak_kib, int(snapshot.stdout.strip()))
            time.sleep(0.02)
        out, err = p.communicate(timeout=3)
        self.assertEqual(p.returncode, 0, err)
        self.assertEqual(len(out), 1024)
        self.assertLess(peak_kib, 64 * 1024)
        print(f"\n  CLI peak RSS while draining 20 MB: {peak_kib / 1024:.1f} MiB", flush=True)

    def test_16_linux_binary_executes_and_reuses_connection(self):
        base = ["docker", "exec", self.container, "sshm"]
        for _ in range(2):
            p = checked(base + ["exec", "fixture", "-F", "/fixture/linux-config", "--command",
                                "printf linux-ok", "--persist", "2s"])
            self.assertEqual((p.stdout, p.stderr), (b"linux-ok", b""))
        p = checked(base + ["doctor", "-F", "/fixture/linux-config"])
        self.assertIn("控制 socket 数: 1", p.stdout.decode())


if __name__ == "__main__":
    unittest.main(verbosity=2)
