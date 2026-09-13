#!/usr/bin/env python3
"""Compare native SSH and sshm over the same explicitly selected live target."""
import argparse
import json
from pathlib import Path
import subprocess
import tempfile
import time
import tomllib

from live import LiveTarget, ROOT


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--toml", type=Path, required=True)
    parser.add_argument("--host", required=True)
    args = parser.parse_args()
    config = args.toml.expanduser().resolve()
    server = tomllib.loads(config.read_text())["ssh_servers"][args.host]
    with tempfile.TemporaryDirectory(prefix="sshm-probe-", dir="/tmp") as tmp:
        target = LiveTarget(args.host, server, config, Path(tmp), ROOT/"bin/sshm", [])
        for size in (65536, 262144):
            for backend in ("native", "sshm"):
                cmd = f"head -c {size} /dev/zero"
                argv = (["ssh", "-T", "-F", str(target.config), args.host, cmd] if backend == "native" else
                        target.argv(cmd, "--persist", "0", "--max-output", "0"))
                start = time.monotonic()
                p = subprocess.Popen(argv, stdin=subprocess.DEVNULL, stdout=subprocess.PIPE,
                                     stderr=subprocess.PIPE, env=target.env)
                timed_out = False
                try:
                    out, err = p.communicate(timeout=20)
                except subprocess.TimeoutExpired:
                    timed_out = True
                    p.terminate()
                    out, err = p.communicate(timeout=5)
                seconds = time.monotonic()-start
                print(json.dumps(dict(backend=backend, requested_bytes=size, received_bytes=len(out),
                                      seconds=round(seconds, 3), timeout=timed_out, code=p.returncode,
                                      stderr=target.sanitize(err.decode(errors="replace")[:500])), ensure_ascii=False), flush=True)
        target.close_connections()


if __name__ == "__main__":
    main()
