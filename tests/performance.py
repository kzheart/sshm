#!/usr/bin/env python3
"""End-to-end measurements. External ssh is a baseline, never a client dependency."""
import argparse
import concurrent.futures
import json
import os
from pathlib import Path
import re
import statistics
import subprocess
import time
from integration import Fixture, BINARY, ROOT


def summary(samples):
    values=sorted(samples)
    return dict(samples=len(values),p50_ms=round(statistics.median(values)*1000,3),
                p95_ms=round(values[min(len(values)-1,int(len(values)*.95))]*1000,3),
                p99_ms=round(values[min(len(values)-1,int(len(values)*.99))]*1000,3))


def measure(argv, env=None, timeout=30):
    start=time.perf_counter()
    with open(os.devnull,'wb') as sink:
        p=subprocess.Popen(argv,stdout=sink,stderr=subprocess.PIPE,stdin=subprocess.DEVNULL,env=env)
        try:_,err=p.communicate(timeout=timeout)
        except subprocess.TimeoutExpired:
            p.terminate()
            try:p.communicate(timeout=3)
            except subprocess.TimeoutExpired:p.kill();p.communicate()
            raise RuntimeError('measurement timed out')
    return time.perf_counter()-start,p.returncode,err


def local(report):
    with Fixture() as f:
        runtime=f.root/'runtime';runtime.mkdir(mode=0o700,exist_ok=True)
        env=dict(os.environ,SSHM_RUNTIME_DIR=str(runtime))
        old=ROOT/'bin/sshm-openssh-0.1.1'
        methods={
          'go':lambda cmd:f.argv(cmd)+['--fresh'],
          'openssh_fresh':lambda cmd:['/usr/bin/ssh','-F',str(f.ssh_config),'-T','-o','BatchMode=yes','-o','ControlMaster=no','-o','ControlPath=none','fixture',cmd],
        }
        if old.exists():
            methods['old_cli_fresh']=lambda cmd:[str(old),'exec','fixture','-F',str(f.ssh_config),'--persist','0','--command',cmd,'--max-output','0']
            methods['old_cli_reused']=lambda cmd:[str(old),'exec','fixture','-F',str(f.ssh_config),'--persist','30s','--command',cmd,'--max-output','0']
        data={'context':'macOS client → isolated Linux sshd, loopback-published Docker port', 'latency':{},'throughput':{},'concurrency':{},'batching':{},'rss':{}}
        try:
            for fn in methods.values():
                _,code,_=measure(fn('true'),env);assert code==0
            samples={name:[] for name in methods}
            # Interleave methods to reduce order and warmup bias.
            for _ in range(30):
                for name,fn in methods.items():
                    elapsed,code,_=measure(fn('true'),env);assert code==0
                    samples[name].append(elapsed)
            for name,values in samples.items():data['latency'][name]=summary(values)
            print('latency:',json.dumps(data['latency']),flush=True)
            for size in (1<<20,32<<20):
                for name in ('go','openssh_fresh'):
                    samples=[]
                    for _ in range(3):
                        argv=methods[name](f'head -c {size} /dev/zero')
                        if name=='go':argv+=['--max-output','0']
                        elapsed,code,_=measure(argv,env);assert code==0;samples.append(elapsed)
                    record=summary(samples);record['end_to_end_mib_s']=round(size/(1<<20)/statistics.median(samples),2)
                    data['throughput'][f'{name}_{size}']=record
            for concurrency in (1,4,16,32):
                start=time.perf_counter()
                with concurrent.futures.ThreadPoolExecutor(max_workers=concurrency) as pool:
                    result=list(pool.map(lambda _:measure(methods['go']('true'),env),range(64)))
                elapsed=time.perf_counter()-start
                record=summary([v[0] for v in result]);record.update(calls=64,seconds=round(elapsed,3),calls_per_second=round(64/elapsed,2),failures=sum(v[1]!=0 for v in result))
                data['concurrency'][str(concurrency)]=record
                assert not record['failures']
            start=time.perf_counter()
            for _ in range(20):assert measure(methods['go']('true'),env)[1]==0
            separate=time.perf_counter()-start
            batched,code,_=measure(methods['go']('i=0; while [ "$i" -lt 20 ]; do true; i=$((i+1)); done'),env);assert code==0
            data['batching']={'20_separate_calls_ms':round(separate*1000,3),'20_steps_one_call_ms':round(batched*1000,3)}
            with concurrent.futures.ThreadPoolExecutor(max_workers=8) as pool:
                result=list(pool.map(lambda _:measure(f.argv('sleep 1','fixture','--timeout','100ms'),env),range(100)))
            data['timeout_stress']=summary([v[0] for v in result]);data['timeout_stress']['expected_124']=sum(v[1]==124 for v in result)
            assert data['timeout_stress']['expected_124']==100
            for name in ('go','openssh_fresh'):
                argv=methods[name]('head -c 67108864 /dev/zero')
                if name=='go':argv+=['--max-output','0']
                _,code,err=measure(['/usr/bin/time','-l',*argv],env);assert code==0
                match=re.search(rb'(\d+)\s+maximum resident set size',err)
                data['rss'][name]={'source':'macOS time -l maximum resident set size','bytes':int(match[1]) if match else None,'payload_bytes':67108864}
            report.update(data)
            print('throughput:',json.dumps(data['throughput']),flush=True)
            print('concurrency:',json.dumps(data['concurrency']),flush=True)
        finally:
            for p in runtime.glob('c-*'):
                if p.is_socket():subprocess.run(['/usr/bin/ssh','-F','/dev/null','-S',str(p),'-O','exit','fixture'],capture_output=True,timeout=3)


if __name__=='__main__':
    parser=argparse.ArgumentParser();parser.add_argument('--report',type=Path,default=ROOT/'reports/performance-local.json');args=parser.parse_args();args.report.parent.mkdir(parents=True,exist_ok=True)
    report={};local(report);args.report.write_text(json.dumps(report,indent=2)+'\n');print('Saved',args.report)
