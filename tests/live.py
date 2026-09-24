#!/usr/bin/env python3
"""Explicit real-server validation for standalone sshm. Only selected hosts.
Writes exclusively to marked, random remote temporary directories, then cleans.
Includes interleaved fresh-connection OpenSSH baseline; never prints credentials.
"""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import re
import shlex
import subprocess
import tempfile
import time
import tomllib
import uuid
from integration import BINARY, ROOT, capture, terminal
from performance import measure, summary


class Target:
    def __init__(self,alias,config,servers,local):
        self.alias=alias;self.config=config;self.server=servers[alias];self.local=local/alias;self.local.mkdir(mode=0o700)
        self.remote=None;self.owner=uuid.uuid4().hex;self.results=[];self.perf={}
        q=lambda v:'"'+str(v).replace('\\','\\\\').replace('"','\\"')+'"'
        self.native_config=self.local/'openssh.conf'
        s=self.server
        assert not s.get('proxy_jump'),'This live baseline supports direct targets only'
        lines=[f'Host {alias}',f' HostName {q(s["host"])}',f' User {q(s["user"])}',f' Port {int(s.get("port",22))}',
               ' StrictHostKeyChecking yes',f' UserKnownHostsFile {q(Path.home()/".ssh/known_hosts")}',
               ' IdentitiesOnly yes',' NumberOfPasswordPrompts 1',' ConnectTimeout 10']
        if s.get('key_path'):lines.append(' IdentityFile '+q(Path(s['key_path']).expanduser()))
        else:lines+=[' IdentityFile none',' PreferredAuthentications password']
        self.native_config.write_text('\n'.join(lines)+'\n');self.native_config.chmod(0o600)
        self.env=dict(os.environ,SSH_ASKPASS=str(ROOT/'tests/toml_askpass.py'),SSH_ASKPASS_REQUIRE='force',SSHM_TEST_TOML=str(config),SSHM_TEST_TARGET=alias)

    def sanitize(self,text):
        for key in ('host','user','password','passphrase'):
            v=self.server.get(key)
            if v:text=str(text).replace(v,'[redacted]')
        return str(text)

    def argv(self,cmd,*options):
        return [str(BINARY),'exec',self.alias,'-F',str(self.config),'--command',cmd,'--timeout','90s',*options]

    def run(self,cmd,*options,input=None):
        return capture(self.argv(cmd,*options),input=input,timeout=100)

    def require(self,p,out=None,code=0):
        if p.returncode!=code:raise AssertionError(self.sanitize(f'exit={p.returncode}, diagnostic={p.stderr.decode(errors="replace")[:600]}'))
        if out is not None and p.stdout!=out:raise AssertionError('output byte mismatch')
        return p

    def case(self,name,fn):
        start=time.monotonic()
        try:
            note=fn();record={'host':self.alias,'case':name,'status':'passed'}
            if note is not None:record['note']=note
        except Exception as e:record={'host':self.alias,'case':name,'status':'failed','error':self.sanitize(e)}
        record['seconds']=round(time.monotonic()-start,3);self.results.append(record)
        print(f'{self.alias}: {name}: {record["status"]} ({record["seconds"]}s)',flush=True)
        if record['status']=='failed':print(record['error'],flush=True)
        return record['status']=='passed'

    def auth(self):
        p=self.require(self.run('uname -s'));assert p.stdout in (b'Darwin\n',b'Linux\n');return p.stdout.decode().strip()

    def workspace(self):
        value=self.require(self.run('mktemp -d /tmp/sshm-v2-XXXXXXXX')).stdout.decode().strip()
        assert re.fullmatch(r'/tmp/sshm-v2-[A-Za-z0-9]+',value)
        self.remote=value
        self.require(self.run(f'printf %s {self.owner} > {shlex.quote(value+"/.owner")}'))

    def streams(self):
        for code in (0,1,17,124,125,130,255):
            p=self.require(self.run(f'printf out; printf err >&2; exit {code}'),b'out',code)
            assert p.stderr==b'err'

    def stdin(self):
        data=bytes(range(256))*256
        self.require(self.run('cat','--stdin','-','--max-output','0',input=data),data)
        self.require(self.run('cat'),b'')

    def scripts(self):
        script="cd /tmp\nprintf '%s\\n' '中文 $HOME `literal`'\npwd\n".encode()
        self.require(self.run('sh -s','--stdin','-',input=script),'中文 $HOME `literal`\n/tmp\n'.encode())

    def limits(self):
        p=self.require(self.run('head -c 1048576 /dev/zero; printf diag >&2','--max-output','1024'))
        assert len(p.stdout)==1024 and b'1047552' in p.stderr

    def concurrency(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
            results=list(pool.map(lambda i:self.run(f'printf {i}'),range(4)))
        for i,p in enumerate(results):self.require(p,str(i).encode())

    def timeout(self):
        with concurrent.futures.ThreadPoolExecutor(max_workers=2) as pool:
            a=pool.submit(self.run,"printf started; sleep 8",'--timeout','5s')
            b=pool.submit(self.run,"printf sibling")
            self.require(a.result(),b'started',124);self.require(b.result(),b'sibling')

    def copy(self):
        source=self.local/'中文 source.bin';dest=self.local/'download.bin';data=os.urandom(65536);source.write_bytes(data)
        remote=self.remote+'/中文 destination.bin'
        base=[str(BINARY),'copy',self.alias,'-F',str(self.config),'--remote',remote,'--timeout','90s']
        self.require(capture([*base,'--upload',str(source)],timeout=100))
        self.require(capture([*base,'--download',str(dest)],timeout=100));assert dest.read_bytes()==data
        assert capture([*base,'--upload',str(source)],timeout=100).returncode!=0

    def pty(self):terminal(self,host=self.alias)

    def performance(self):
        methods={
         'go':lambda cmd:self.argv(cmd,'--max-output','0','--fresh'),
         'openssh_fresh':lambda cmd:['/usr/bin/ssh','-F',str(self.native_config),'-T','-o','BatchMode=no','-o','ControlMaster=no','-o','ControlPath=none',self.alias,cmd],
        }
        samples={k:[] for k in methods}
        for _ in range(10):
            for name,fn in methods.items():
                elapsed,code,_=measure(fn('true'),self.env,timeout=90);assert code==0;samples[name].append(elapsed)
        self.perf['latency']={name:summary(v) for name,v in samples.items()}
        self.perf['throughput']={}
        for size in (65536,1048576):
            values={name:[] for name in methods}
            for sample in range(5):
                order=list(methods.items())
                if sample%2:order.reverse()
                for name,fn in order:
                    elapsed,code,_=measure(fn(f'head -c {size} /dev/zero'),self.env,timeout=100);assert code==0
                    values[name].append(elapsed)
            for name,durations in values.items():
                record=summary(durations)
                record['end_to_end_mib_s']=round(size/(1<<20)/(record['p50_ms']/1000),4)
                self.perf['throughput'][f'{name}_{size}']=record
                print(f'{self.alias}: {name} {size} bytes: median {record["p50_ms"]:.3f}ms',flush=True)
        return self.perf

    def cleanup(self):
        if self.remote:
            q=shlex.quote(self.remote)
            self.require(self.run(f'[ "$(cat {q}/.owner)" = {self.owner} ] && rm -rf -- {q} && [ ! -e {q} ]'))
            self.remote=None


def main():
    parser=argparse.ArgumentParser();parser.add_argument('--toml',type=Path,default=Path.home()/'.ssh/sshm.toml');parser.add_argument('--hosts',nargs='+',required=True);parser.add_argument('--report',type=Path,default=ROOT/'reports/live-v2-results.json');parser.add_argument('--performance-only',action='store_true');args=parser.parse_args();args.report.parent.mkdir(parents=True,exist_ok=True)
    cfg=args.toml.expanduser().resolve();raw=cfg.read_bytes();known=Path.home()/'.ssh/known_hosts';known_before=known.read_bytes()
    servers=tomllib.loads(raw.decode())['ssh_servers'];results=[]
    with tempfile.TemporaryDirectory(prefix='sshm-v2-live-',dir='/tmp') as temp:
        for alias in args.hosts:
            target=Target(alias,cfg,servers,Path(temp))
            try:
                if args.performance_only:
                    target.case('performance-baseline',target.performance)
                elif target.case('authentication',target.auth) and target.case('temporary-workspace',target.workspace):
                    for name,fn in [('streams-and-exits',target.streams),('binary-stdin-and-eof',target.stdin),('script-and-unicode',target.scripts),('one-mib-drain-limit',target.limits),('four-concurrent-calls',target.concurrency),('timeout-and-sibling',target.timeout),('sftp-roundtrip-and-no-overwrite',target.copy),('interactive-pty',target.pty),('performance-baseline',target.performance)]:target.case(name,fn)
            finally:
                target.case('cleanup',target.cleanup);results.extend(target.results)
                args.report.write_text(json.dumps({'results':results},ensure_ascii=False,indent=2)+'\n')
    unchanged=cfg.read_bytes()==raw and known.read_bytes()==known_before
    report={'results':results,'original_config_and_known_hosts_unchanged':unchanged,'passed':sum(v['status']=='passed' for v in results),'failed':sum(v['status']=='failed' for v in results)}
    args.report.write_text(json.dumps(report,ensure_ascii=False,indent=2)+'\n')
    print(f'RESULT: {report["passed"]} passed, {report["failed"]} failed; original files unchanged={unchanged}')
    if report['failed'] or not unchanged:raise SystemExit(1)


if __name__=='__main__':main()
