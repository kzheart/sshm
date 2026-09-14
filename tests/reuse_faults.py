#!/usr/bin/env python3
"""Bounded probes, blocked stderr, and native Linux broker checks."""
import json
import os
import subprocess
import time

from integration import ROOT, Fixture, checked, capture
from reuse import daemons, resource_count


report={}
with Fixture() as f:
    # Linux must actually reuse, rather than silently pass on fresh fallback.
    argv=['docker','exec',f.container,'/usr/local/bin/sshm','exec','fixture','-F','/tmp/sshm-fixture/linux.toml','--command','true','--debug']
    checked(argv);p=checked(argv)
    assert b'reused=true' in p.stderr
    report['linux_broker_reused']=True
    assert f.run('true').returncode==0
    pid=daemons(f.runtime)[0]
    before=resource_count(pid)
    # Both fresh and pooled calls must return even if nobody reads stderr.
    for fresh in (False,True):
        argv=f.argv('head -c 20971520 /dev/zero >&2','fixture','--timeout','200ms','--max-output','0')+(['--fresh'] if fresh else [])
        p=subprocess.Popen(argv,stdout=subprocess.PIPE,stderr=subprocess.PIPE)
        start=time.monotonic();p.wait(timeout=3)
        assert p.returncode==124 and time.monotonic()-start<1
        p.stdout.close();p.stderr.close()
    report['blocked_stderr_both_modes']=True
    assert f.run('true').returncode==0
    # No CLI deadline: a completely silent broken transport must still be reaped
    # by the transport liveness watchdog (10-second interval + 5-second wait).
    checked(['docker','pause',f.container])
    start=time.monotonic()
    try:
        p=capture(f.argv('true','fixture','--timeout','0'),timeout=20)
        assert p.returncode==125,p.returncode
    finally:checked(['docker','unpause',f.container])
    report['silent_network_loss']={'exit':125,'seconds':round(time.monotonic()-start,3)}
    assert f.run('true').returncode==0
    time.sleep(.2)
    after=resource_count(pid)
    assert after['fd_count']<=before['fd_count']+2
    report['resources']={'before':before,'after':after}
    # Repeated malformed local requests cannot accumulate descriptor references.
    import array
    import socket
    for _ in range(100):
        with socket.socket(socket.AF_UNIX,socket.SOCK_STREAM) as s,open(os.devnull,'rb') as null:
            s.connect(str(f.runtime/'control.sock'))
            s.sendmsg([b'X'],[(socket.SOL_SOCKET,socket.SCM_RIGHTS,array.array('i',[null.fileno()]*3))])
    time.sleep(.2)
    after=resource_count(pid)
    assert after['fd_count']<=before['fd_count']+2
    report['malformed_requests']={'calls':100,'fd_after':after['fd_count']}
(ROOT/'docs/reuse-fault-results.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps(report,indent=2))
