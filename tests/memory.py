#!/usr/bin/env python3
"""Peak RSS and CPU vs payload size; discard output without Python buffering."""
import json,re
from integration import Fixture,ROOT
from performance import measure
report={}
with Fixture() as f:
    for size in (1048576,67108864,268435456):
        samples=[]
        for _ in range(3):
            seconds,code,err=measure(['/usr/bin/time','-l',*f.argv(f'head -c {size} /dev/zero','fixture','--max-output','0')],timeout=30)
            assert code==0
            rss=re.search(rb'(\d+)\s+maximum resident set size',err)
            cpu=re.search(rb'([0-9.]+)\s+real\s+([0-9.]+)\s+user\s+([0-9.]+)\s+sys',err)
            assert rss and cpu
            samples.append({'seconds':round(seconds,4),'peak_rss_bytes':int(rss[1]),'user_seconds':float(cpu[2]),'sys_seconds':float(cpu[3])})
        report[str(size)]=samples
(ROOT/'reports').mkdir(exist_ok=True)
(ROOT/'reports/performance-memory.json').write_text(json.dumps(report,indent=2)+'\n')
print(json.dumps(report))
