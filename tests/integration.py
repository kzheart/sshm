#!/usr/bin/env python3
"""Standalone client vs an isolated real OpenSSH server; no user credentials.
OpenSSH CLI is used only to create fixture keys and as a measured baseline.
"""
import concurrent.futures
import fcntl
import json
import os
from pathlib import Path
import pty
import select
import shutil
import signal
import socket
import struct
import subprocess
import tempfile
import termios
import time
import unittest
import uuid

ROOT = Path(__file__).resolve().parents[1]
BINARY = ROOT / 'bin/sshm'
IMAGE = os.environ.get('SSHM_TEST_IMAGE', 'sshm-test-server:local')


def checked(args, **kw):
    return subprocess.run(args, check=True, capture_output=True, timeout=60, **kw)


def capture(args, input=None, timeout=15, env=None):
    p = subprocess.Popen(args, stdin=subprocess.PIPE if input is not None else subprocess.DEVNULL,
                         stdout=subprocess.PIPE, stderr=subprocess.PIPE, env=env)
    try:
        out, err = p.communicate(input, timeout=timeout)
    except subprocess.TimeoutExpired:
        p.terminate()
        try:
            p.communicate(timeout=5)
        except subprocess.TimeoutExpired:
            p.kill(); p.communicate()
        raise
    return subprocess.CompletedProcess(args, p.returncode, out, err)


class Fixture:
    def __enter__(self):
        self.root = Path(tempfile.mkdtemp(prefix='sshm-v2-', dir='/tmp'))
        self.container = 'sshm-v2-' + uuid.uuid4().hex[:10]
        self.password = uuid.uuid4().hex
        try:
            for key in ('client', 'host'):
                checked(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(self.root/key)])
            arch = checked(['docker', 'image', 'inspect', '--format', '{{.Architecture}}', IMAGE], text=True).stdout.strip()
            self.linux = ROOT/'bin'/f'sshm-linux-{arch}'
            checked(['go', 'build', '-trimpath', '-o', str(self.linux), '.'], cwd=ROOT,
                    env=dict(os.environ, GOOS='linux', GOARCH=arch, CGO_ENABLED='0'))
            checked(['docker', 'run', '-d', '--name', self.container, '-p', '127.0.0.1::2222',
                     '-v', f'{self.root}:/fixture:ro', '-v', f'{self.linux}:/usr/local/bin/sshm:ro',
                     '-v', f'{self.root/"client.pub"}:/etc/ssh/sshm_test_authorized_keys:ro',
                     '-v', f'{self.root/"host"}:/etc/ssh/sshm_test_hostkey:ro', IMAGE])
            self.port = int(checked(['docker', 'port', self.container, '2222/tcp'], text=True).stdout.strip().rsplit(':', 1)[1])
            deadline = time.monotonic()+15
            while True:
                try:
                    with socket.create_connection(('127.0.0.1', self.port), timeout=1) as s:
                        if s.recv(100).startswith(b'SSH-'): break
                except OSError: pass
                if time.monotonic()>deadline: raise RuntimeError('fixture sshd did not start')
                time.sleep(.1)
            checked(['docker', 'exec', self.container, 'useradd', '-m', '-s', '/bin/sh', 'passwordtester'])
            checked(['docker', 'exec', '-i', self.container, 'chpasswd'], input=f'passwordtester:{self.password}\n'.encode())
            key = ' '.join((self.root/'host.pub').read_text().split()[:2])
            self.known = self.root/'known_hosts'
            self.known.write_text(f'[127.0.0.1]:{self.port} {key}\n[127.0.0.1]:2222 {key}\n')
            self.config = self.root/'servers.toml'
            self.config.write_text(f'''known_hosts = {json.dumps(str(self.known))}
[ssh_servers.fixture]
host = "127.0.0.1"
port = {self.port}
user = "tester"
key_path = {json.dumps(str(self.root/'client'))}
[ssh_servers.password]
host = "127.0.0.1"
port = {self.port}
user = "passwordtester"
password = "{self.password}"
[ssh_servers.jump]
host = "127.0.0.1"
port = 2222
user = "passwordtester"
password = "{self.password}"
proxy_jump = "fixture"
''')
            self.config.chmod(0o600)
            self.ssh_config = self.root/'openssh.conf'
            self.ssh_config.write_text(f'''Host fixture
 HostName 127.0.0.1
 Port {self.port}
 User tester
 IdentityFile "{self.root/'client'}"
 IdentitiesOnly yes
 UserKnownHostsFile "{self.known}"
 GlobalKnownHostsFile /dev/null
 StrictHostKeyChecking yes
''')
            self.linux_config = self.root/'linux.toml'
            self.linux_config.write_text(self.config.read_text().replace(str(self.root), '/tmp/sshm-fixture').replace(f'port = {self.port}', 'port = 2222'))
            self.linux_config.chmod(0o600)
            checked(['docker', 'exec', self.container, 'sh', '-c', 'mkdir -m 700 /tmp/sshm-fixture; cp /fixture/client /fixture/known_hosts /fixture/linux.toml /tmp/sshm-fixture/; chmod 600 /tmp/sshm-fixture/*'])
            return self
        except BaseException:
            self.__exit__(None,None,None); raise

    def __exit__(self, *args):
        subprocess.run(['docker', 'rm', '-f', self.container], capture_output=True, timeout=20)
        shutil.rmtree(self.root)

    def argv(self, cmd, host='fixture', *extra):
        return [str(BINARY), 'exec', host, '-F', str(self.config), '--command', cmd, *extra]

    def run(self, cmd, host='fixture', *extra, **kw):
        return capture(self.argv(cmd, host, *extra), **kw)

    def copy(self, *args):
        return capture([str(BINARY), 'copy', 'fixture', '-F', str(self.config), *args])


def terminal(fixture, command='sh', host='fixture'):
    master, slave = pty.openpty()
    fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 31, 101, 0, 0))
    def setup():
        os.setsid(); fcntl.ioctl(0, termios.TIOCSCTTY, 0)
    p = subprocess.Popen([str(BINARY), 'shell', host, '-F', str(fixture.config), '--command', command, '--timeout', '10s'],
                         stdin=slave, stdout=slave, stderr=slave, preexec_fn=setup)
    os.close(slave); output = bytearray()
    def until(marker):
        deadline = time.monotonic()+8
        while marker not in output:
            if time.monotonic()>deadline: raise AssertionError('terminal marker deadline')
            if select.select([master], [], [], .1)[0]:
                try: data=os.read(master,65536)
                except OSError: break
                if not data: break
                output.extend(data)
        assert marker in output, 'missing terminal marker'
    try:
        os.write(master, b"printf 'READY_%s\\n' ok\n")
        until(b'READY_ok')
        os.write(master, b"cd /tmp; stty size; printf 'ASK_%s\\n' input; read answer; printf 'VALUE_%s\\n' \"$answer\"; pwd\n")
        until(b'ASK_input'); os.write(master, b'hello-agent\n'); until(b'VALUE_hello-agent')
        os.write(master, b'exit 17\n'); p.wait(timeout=5)
        assert p.returncode==17 and b'31 101' in output and b'/tmp' in output
        return bytes(output)
    finally:
        if p.poll() is None: p.terminate(); p.wait(timeout=5)
        os.close(master)


class Integration(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.context = Fixture(); cls.f = cls.context.__enter__()
        cls.addClassCleanup(cls.context.__exit__, None, None, None)

    def assert_ok(self, p, out=None, code=0):
        self.assertEqual(p.returncode,code,p.stderr.decode(errors='replace'))
        if out is not None:self.assertEqual(p.stdout,out)
        self.assertEqual(p.stderr,b'')

    def test_01_password_key_and_jump(self):
        for host in ('fixture','password','jump'):
            self.assert_ok(self.f.run("printf ok",host),b'ok')

    def test_02_raw_streams_and_exit_status(self):
        for code in (0,1,2,17,124,125,130,255):
            p=self.f.run(f"printf out; printf err >&2; exit {code}")
            self.assertEqual((p.returncode,p.stdout,p.stderr),(code,b'out',b'err'))

    def test_03_binary_stdin_and_default_eof(self):
        data=bytes(range(256))*4096
        self.assert_ok(self.f.run('cat','fixture','--stdin','-','--max-output','0',input=data),data)
        self.assert_ok(self.f.run('cat'),b'')

    def test_04_unicode_script_and_state(self):
        self.assert_ok(self.f.run('cd /tmp; pwd'), b'/tmp\n')
        # Use single quotes for literals; the CLI must not reinterpret the script.
        data="printf '%s\\n' '中文 $HOME `literal`'\n".encode()
        self.assert_ok(self.f.run('sh -s','fixture','--stdin','-',input=data),'中文 $HOME `literal`\n'.encode())
        self.assert_ok(self.f.run('pwd'),b'/home/tester\n')

    def test_05_large_output_drains(self):
        p=self.f.run('head -c 20971520 /dev/zero; printf diag >&2','fixture','--max-output','1024')
        self.assertEqual(p.returncode,0);self.assertEqual(len(p.stdout),1024);self.assertIn(b'20970496',p.stderr)
        p=self.f.run('head -c 20971520 /dev/zero','fixture','--max-output','0')
        self.assert_ok(p,b'\0'*20971520)

    def test_06_timeout(self):
        start=time.monotonic();p=self.f.run('sleep 2','fixture','--timeout','150ms')
        self.assertEqual(p.returncode,124);self.assertLess(time.monotonic()-start,1.5)

    def test_07_cancel_open_stdin(self):
        p=subprocess.Popen(self.f.argv('sleep 2','fixture','--stdin','-'),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        time.sleep(.3);p.terminate();p.wait(timeout=3);self.assertEqual(p.returncode,130)
        p.stdin.close();p.stdout.close();p.stderr.close()

    def test_08_normal_exit_with_unclosed_stdin(self):
        p=subprocess.Popen(self.f.argv('printf done','fixture','--stdin','-'),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        p.wait(timeout=3);self.assertEqual(p.returncode,0);self.assertEqual(p.stdout.read(),b'done')
        p.stdin.close();p.stdout.close();p.stderr.close()

    def test_09_concurrent_calls(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=16) as pool:
            results=list(pool.map(lambda i:self.f.run(f'printf {i}'),range(32)))
        for i,p in enumerate(results):self.assert_ok(p,str(i).encode())

    def test_10_sibling_survives_timeout(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            a=pool.submit(self.f.run,'sleep 2','fixture','--timeout','150ms')
            b=pool.submit(self.f.run,'sleep .3; printf survived')
            self.assertEqual(a.result().returncode,124);self.assert_ok(b.result(),b'survived')

    def test_11_no_openssh_in_path(self):
        env=dict(os.environ,PATH='/nonexistent')
        self.assert_ok(self.f.run('printf standalone',env=env),b'standalone')

    def test_12_copy_roundtrip_and_overwrite(self):
        source=self.f.root/'中文 space.bin';dest=self.f.root/'download.bin';data=os.urandom(2<<20);source.write_bytes(data)
        remote='/tmp/sshm-copy-'+uuid.uuid4().hex
        try:
            p=self.f.copy('--upload',str(source),'--remote',remote);self.assertEqual(p.returncode,0,p.stderr)
            self.assertNotEqual(self.f.copy('--upload',str(source),'--remote',remote).returncode,0)
            p=self.f.copy('--download',str(dest),'--remote',remote);self.assertEqual(p.returncode,0,p.stderr);self.assertEqual(dest.read_bytes(),data)
            self.assertNotEqual(self.f.copy('--download',str(dest),'--remote',remote).returncode,0)
            source.write_bytes(b'new');self.assertEqual(self.f.copy('--upload',str(source),'--remote',remote,'--overwrite').returncode,0)
            self.assertEqual(self.f.copy('--download',str(dest),'--remote',remote,'--overwrite').returncode,0);self.assertEqual(dest.read_bytes(),b'new')
        finally:self.f.run('rm -f -- '+remote)

    def test_13_interactive_terminal(self):terminal(self.f)

    def test_14_linux_binary_password_jump_and_stdin(self):
        for host in ('fixture','password','jump'):
            p=capture(['docker','exec','-i',self.f.container,'/usr/local/bin/sshm','exec',host,'-F','/tmp/sshm-fixture/linux.toml','--command','cat','--stdin','-'],input=b'linux\x00\xff')
            self.assert_ok(p,b'linux\x00\xff')

    def test_15_invalid_auth_config_and_host_key(self):
        cfg=self.f.root/'invalid.toml';cfg.write_text(self.f.config.read_text().replace(self.f.password,'wrong-password'));cfg.chmod(0o600)
        p=capture([str(BINARY),'exec','password','-F',str(cfg),'--command','true'])
        self.assertEqual(p.returncode,125);self.assertNotIn(b'wrong-password',p.stderr)
        cfg.write_text(self.f.config.read_text().replace(str(self.f.known),str(self.f.root/'empty-known')));(self.f.root/'empty-known').write_text('')
        p=capture([str(BINARY),'exec','fixture','-F',str(cfg),'--command','true'])
        self.assertEqual(p.returncode,125);self.assertIn(b'SHA256:',p.stderr)

    def test_16_closed_output_consumer(self):
        p=subprocess.Popen(self.f.argv('head -c 20971520 /dev/zero','fixture','--max-output','0'),stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        p.stdout.close();p.wait(timeout=5);self.assertEqual(p.returncode,125);p.stderr.close()

    def test_17_blocked_output_consumer(self):
        p=subprocess.Popen(self.f.argv('head -c 20971520 /dev/zero','fixture','--max-output','0','--timeout','300ms'),stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        p.wait(timeout=3);self.assertEqual(p.returncode,124);p.stdout.close();p.stderr.close()

    def test_18_encrypted_private_key(self):
        key=self.f.root/'encrypted';shutil.copyfile(self.f.root/'client',key);key.chmod(0o600)
        # Synthetic fixture passphrase, no real credential enters command arguments.
        checked(['ssh-keygen','-p','-P','','-N','fixture-only-passphrase','-f',str(key)])
        cfg=self.f.root/'encrypted.toml';cfg.write_text(self.f.config.read_text().replace(str(self.f.root/'client'),str(key)).replace('[ssh_servers.password]','passphrase = "fixture-only-passphrase"\n[ssh_servers.password]'));cfg.chmod(0o600)
        self.assert_ok(capture([str(BINARY),'exec','fixture','-F',str(cfg),'--command','printf encrypted']),b'encrypted')

    def test_19_jump_timeout(self):
        p=self.f.run('sleep 2','jump','--timeout','200ms');self.assertEqual(p.returncode,124)

    def test_20_shell_cancel_restores_local_terminal(self):
        master,slave=pty.openpty();before=termios.tcgetattr(slave)
        p=subprocess.Popen([str(BINARY),'shell','fixture','-F',str(self.f.config),'--command','sleep 2'],stdin=slave,stdout=slave,stderr=slave)
        try:
            time.sleep(.3);p.terminate();p.wait(timeout=3)
            self.assertEqual(p.returncode,130)
            after=termios.tcgetattr(slave)
            # Darwin may set PENDIN when tcsetattr reprocesses queued input;
            # it is a transient kernel state, not a raw/canonical mode change.
            before[3] &= ~getattr(termios,'PENDIN',0)
            after[3] &= ~getattr(termios,'PENDIN',0)
            self.assertEqual(after,before)
        finally:
            if p.poll() is None:p.kill();p.wait()
            os.close(master);os.close(slave)

    def test_21_cancel_sftp_mid_transfer(self):
        source=self.f.root/'sparse-upload.bin'
        with source.open('wb') as f:f.truncate(2<<30)
        remote='/tmp/sshm-cancel-'+uuid.uuid4().hex
        try:
            p=capture([str(BINARY),'copy','fixture','-F',str(self.f.config),'--upload',str(source),'--remote',remote,'--timeout','1s'],timeout=5)
            self.assertEqual(p.returncode,124,p.stderr)
            size=int(checked(['docker','exec',self.f.container,'stat','-c','%s',remote]).stdout)
            self.assertGreater(size,0);self.assertLess(size,2<<30)
        finally:
            checked(['docker','exec',self.f.container,'rm','-f','--',remote]);source.unlink()

    def test_22_shell_owner_closes_input(self):
        p=subprocess.Popen([str(BINARY),'shell','fixture','-F',str(self.f.config),'--command','sleep 5'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        try:
            time.sleep(.3);p.stdin.close();p.wait(timeout=3)
            self.assertEqual(p.returncode,130)
        finally:
            if p.poll() is None:p.kill();p.wait()
            p.stdout.close();p.stderr.close()

    def test_23_shell_output_consumer_closes(self):
        p=subprocess.Popen([str(BINARY),'shell','fixture','-F',str(self.f.config),'--command','printf output; sleep 5'],stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        try:
            p.stdout.close();p.wait(timeout=3)
            self.assertEqual(p.returncode,125)
        finally:
            if p.poll() is None:p.kill();p.wait()
            p.stdin.close();p.stderr.close()


if __name__=='__main__':unittest.main(verbosity=2)
