#!/usr/bin/env python3
"""Cross-process reuse correctness, lifecycle, and interleaved fresh/reused timing."""
import argparse
import concurrent.futures
import hashlib
import json
import os
from pathlib import Path
import signal
import statistics
import subprocess
import tempfile
import time

from integration import BINARY, ROOT, Fixture, capture, checked
from performance import summary


def daemons(runtime):
    result=[]
    listing=checked(['ps','-axo','pid=,command='],text=True).stdout
    for line in listing.splitlines():
        fields=line.strip().split(None,1)
        if len(fields)==2 and fields[1]==f'{BINARY} _serve {runtime}':result.append(int(fields[0]))
    return result


def stop(runtime, sig=signal.SIGTERM):
    for pid in daemons(runtime):
        try:os.kill(pid,sig)
        except ProcessLookupError:pass
    deadline=time.monotonic()+5
    while daemons(runtime) and time.monotonic()<deadline:time.sleep(.05)
    assert not daemons(runtime),'service did not exit'


def timed(argv,env=None):
    start=time.perf_counter();p=capture(argv,env=env,timeout=20)
    assert p.returncode==0,p.stderr.decode(errors='replace')
    return time.perf_counter()-start,p


def comparisons(argv,env=None,n=30):
    samples={'fresh':[],'reused':[]}
    # Record the initial call separately; it may already be warm. Every warm sample confirms reuse.
    cold,p=timed(argv+['--debug'],env)
    for i in range(n):
        for mode in (('fresh','reused') if i%2==0 else ('reused','fresh')):
            elapsed,p=timed(argv+['--debug']+(['--fresh'] if mode=='fresh' else []),env)
            assert (b'reused=true' if mode=='reused' else b'reused=false') in p.stderr,p.stderr
            samples[mode].append(elapsed)
    result={mode:summary(values) for mode,values in samples.items()}
    result['initial_call_ms']=round(cold*1000,3)
    result['median_reduction_percent']=round((1-statistics.median(samples['reused'])/statistics.median(samples['fresh']))*100,1)
    return result


def resource_count(pid):
    # Counts real descriptor records, excluding cwd/text mappings.
    p=subprocess.run(['lsof','-nP','-p',str(pid),'-Ff'],capture_output=True,text=True,timeout=5)
    fds=[line[1:] for line in p.stdout.splitlines() if line.startswith('f') and line[1:].isdigit()]
    rss=int(checked(['ps','-o','rss=','-p',str(pid)],text=True).stdout.strip())*1024
    return dict(fd_count=len(fds),rss_bytes=rss)


def local(report):
    with Fixture() as f:
        runtime=f.runtime
        def batch(n,workers,command='true',extra=()):
            with concurrent.futures.ThreadPoolExecutor(max_workers=workers) as pool:
                return list(pool.map(lambda _:capture(f.argv(command,'fixture',*extra),timeout=20),range(n)))
        result=batch(32,32)
        assert all(p.returncode==0 for p in result)
        assert len(daemons(runtime))==1
        pid=daemons(runtime)[0]
        report['simultaneous_start']={'clients':32,'services':1,'failures':0}
        report['latency']=comparisons(f.argv('true'))
        print('local latency',json.dumps(report['latency']),flush=True)
        before=resource_count(pid)
        start=time.perf_counter();result=batch(1000,16)
        assert all(p.returncode==0 and p.stdout==b'' and p.stderr==b'' for p in result)
        time.sleep(.3);after=resource_count(pid)
        report['process_soak']={'calls':1000,'workers':16,'failures':0,'seconds':round(time.perf_counter()-start,3),'before':before,'after':after}
        assert after['fd_count']<=before['fd_count']+2,report['process_soak']
        # A caller killed without cleanup must release daemon pipe refs promptly.
        sibling=subprocess.Popen(f.argv('printf ready; sleep 1; printf sibling'),stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        assert sibling.stdout.read(5)==b'ready'
        victim=subprocess.Popen(f.argv('printf ready; sleep 120','fixture','--stdin','-'),stdin=subprocess.PIPE,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        assert victim.stdout.read(5)==b'ready'
        start=time.monotonic();victim.kill();out,err=victim.communicate(timeout=2)
        assert time.monotonic()-start<.7
        out,err=sibling.communicate(timeout=3);assert sibling.returncode==0 and out==b'sibling'
        report['sigkill_caller']={'pipes_released':True,'sibling_survived':True}
        # A long remote sleep must not leave a local worker after deadline expiry.
        result=batch(100,8,'sleep 120',('--timeout','80ms'))
        assert all(p.returncode==124 for p in result)
        time.sleep(.5)
        after_cancel=resource_count(pid)
        assert after_cancel['fd_count']<=before['fd_count']+2,after_cancel
        report['cancellation_soak']={'calls':100,'workers':8,'expected_124':100,'after':after_cancel}
        # Configuration and trusted-key changes may never reuse stale credentials.
        content=f.config.read_text()
        bad=content.replace(f'password = "{f.password}"','password = "wrong-synthetic-password"')
        assert f.run('true','password').returncode==0
        f.config.write_text(bad)
        assert f.run('true','password').returncode==125
        f.config.write_text(content)
        trust=f.known.read_bytes();f.known.write_bytes(b'')
        assert f.run('true').returncode==125
        f.known.write_bytes(trust)
        assert f.run('true').returncode==0
        key=f.root/'client';key.chmod(0o644)
        assert f.run('true').returncode==125
        key.chmod(0o600)
        report['config_trust_key_revalidation']=True
        # No replay on service death after a command has demonstrably executed.
        remote='/tmp/sshm-reuse-counter'
        p=subprocess.Popen(f.argv(f'printf x >> {remote}; printf ready; sleep 120'),stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        assert p.stdout.read(5)==b'ready'
        stop(runtime,signal.SIGKILL)
        out,err=p.communicate(timeout=3);assert p.returncode==125
        assert f.run(f'wc -c < {remote}').stdout.strip()==b'1'
        assert len(daemons(runtime))==1
        report['service_crash']={'uncertain_exit_125':True,'executions':1,'stale_socket_restart':True}
        # Silent network loss with an already warm transport. The liveness probe
        # bounds abandoned channels, and the next request reconnects after recovery.
        checked(['docker','pause',f.container])
        try:
            p=f.run('true','fixture','--timeout','200ms')
            assert p.returncode==124
        finally:checked(['docker','unpause',f.container])
        assert f.run('true').returncode==0
        report['network_pause_recovery']=True
        report['large_output']={}
        for size in (1<<20,32<<20):
            values=[]
            for _ in range(3):
                start=time.perf_counter()
                with open(os.devnull,'wb') as sink:
                    p=subprocess.run(f.argv(f'head -c {size} /dev/zero','fixture','--max-output','0'),stdout=sink,stderr=subprocess.PIPE,timeout=10)
                assert p.returncode==0;values.append(time.perf_counter()-start)
            report['large_output'][str(size)]={'mib_s':round(size/(1<<20)/statistics.median(values),2),**summary(values)}
        # Actual production timeout, not a shortened test-only setting.
        assert f.run('true').returncode==0
        start=time.monotonic()
        print('waiting for the actual 60-second idle exit',flush=True)
        while daemons(runtime) and time.monotonic()-start<65:time.sleep(.5)
        assert not daemons(runtime),'idle service leaked'
        assert not (runtime/'control.sock').exists()
        report['idle_exit']={'seconds':round(time.monotonic()-start,2),'services':0,'socket_removed':True}
        # An unusable runtime directory falls back before submitting any command.
        env=dict(os.environ,SSHM_RUNTIME_DIR=str(f.root))
        f.root.chmod(0o755)
        try:
            p=capture(f.argv('printf fallback','fixture','--debug'),env=env)
            assert p.returncode==0 and p.stdout==b'fallback' and b'reused=false' in p.stderr
        finally:f.root.chmod(0o700)
        report['fresh_fallback']=True


def live(report,hosts):
    config=Path.home()/'.ssh/sshm.toml';known=Path.home()/'.ssh/known_hosts'
    before=[hashlib.sha256(p.read_bytes()).digest() for p in (config,known)]
    with tempfile.TemporaryDirectory(prefix='sshm-reuse-live-',dir='/tmp') as directory:
        runtime=Path(directory);env=dict(os.environ,SSHM_RUNTIME_DIR=str(runtime))
        try:
            for host in hosts:
                argv=[str(BINARY),'exec',host,'--command','true']
                report[host]=comparisons(argv,env,n=15)
                # Warm concurrency and cancellation isolation without remote files.
                with concurrent.futures.ThreadPoolExecutor(max_workers=4) as pool:
                    result=list(pool.map(lambda _:capture(argv,env=env),range(16)))
                assert all(p.returncode==0 for p in result)
                report[host]['concurrent_calls']=16
                p=capture([str(BINARY),'exec',host,'--command','sleep 3','--timeout','200ms'],env=env)
                assert p.returncode==124
                assert capture(argv,env=env).returncode==0
                report[host]['timeout_and_reconnect']=True
                print('live',host,json.dumps(report[host]),flush=True)
        finally:stop(runtime)
    assert before==[hashlib.sha256(p.read_bytes()).digest() for p in (config,known)]


if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('--live',nargs='*',choices=['macmini','tencent']);parser.add_argument('--report',type=Path,default=ROOT/'reports/reuse-results.json');args=parser.parse_args();args.report.parent.mkdir(parents=True,exist_ok=True)
    report={}
    try:
        if args.live is not None:live(report,args.live)
        else:local(report)
    finally:args.report.write_text(json.dumps(report,indent=2)+'\n')
    print('Saved',args.report,flush=True)
