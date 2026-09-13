#!/usr/bin/env python3
"""Verify source/release installation in temporary prefixes, never in HOME."""
import hashlib
import platform
from pathlib import Path
import subprocess
import tarfile
import tempfile

ROOT = Path(__file__).resolve().parents[1]


def check_install(source, base):
    prefix, skills = base / "prefix with spaces", base / "agent skills"
    argv = [str(source / "install.sh"), "--prefix", str(prefix), "--skill-dir", str(skills)]
    for _ in range(2):  # repeated install/upgrade must replace atomically
        subprocess.run(argv, check=True, capture_output=True)
    result = subprocess.run([str(prefix / "bin/sshm"), "--version"], check=True,
                            capture_output=True, text=True)
    assert result.stdout.strip() == "sshm 0.1.0", result.stdout
    assert (skills / "sshm/SKILL.md").read_bytes() == (ROOT / "skills/sshm/SKILL.md").read_bytes()
    assert not list(prefix.rglob(".sshm-install.*"))
    assert not list(skills.rglob(".SKILL-install.*"))


with tempfile.TemporaryDirectory(prefix="sshm-install-") as temp:
    base = Path(temp)
    check_install(ROOT, base / "source")
    checksums = ROOT / "dist/SHA256SUMS-0.1.0"
    lines = checksums.read_text().splitlines()
    assert len(lines) == 4, lines
    for line in lines:
        digest, name = line.split()
        archive = ROOT / "dist" / name.lstrip("*")
        assert hashlib.sha256(archive.read_bytes()).hexdigest() == digest
        with tarfile.open(archive) as tar:
            names = [Path(m.name).name for m in tar.getmembers() if m.isfile()]
            assert sorted(names) == ["README.md", "SKILL.md", "install.sh", "sshm"], names
    target_os = "darwin" if platform.system() == "Darwin" else "linux"
    arch = "arm64" if platform.machine() in ("arm64", "aarch64") else "amd64"
    name = f"sshm_0.1.0_{target_os}_{arch}"
    with tarfile.open(ROOT / "dist" / f"{name}.tar.gz") as tar:
        tar.extractall(base, filter="data")
    check_install(base / name, base / "release")
print("安装验证通过：源码及发布包、含空格路径、重复安装、Skill 内容、四份发布包校验和。")
