#!/bin/sh
set -eu
root=$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)
cd "$root"
version=${1:-0.1.1}
case "$version" in
    ''|*[!0-9A-Za-z._-]*) echo '版本仅允许字母、数字、点、下划线和短横线。' >&2; exit 2 ;;
esac
mkdir -p dist
stage=$(mktemp -d)
trap 'rm -rf "$stage"' EXIT HUP INT TERM
for target in darwin/arm64 darwin/amd64 linux/arm64 linux/amd64; do
    target_os=${target%/*}
    target_arch=${target#*/}
    name=sshm_${version}_${target_os}_${target_arch}
    package=$stage/$name
    mkdir -p "$package/skills/sshm"
    CGO_ENABLED=0 GOOS=$target_os GOARCH=$target_arch go build -trimpath \
        -ldflags "-s -w -X main.version=$version" -o "$package/sshm" .
    cp README.md install.sh "$package/"
    cp skills/sshm/SKILL.md "$package/skills/sshm/"
    # macOS tar otherwise adds AppleDouble ._* files to distributable archives.
    COPYFILE_DISABLE=1 tar -czf "dist/$name.tar.gz" -C "$stage" "$name"
    echo "dist/$name.tar.gz"
done
cd dist
if command -v sha256sum >/dev/null 2>&1; then
    sha256sum "sshm_${version}_"*.tar.gz > "SHA256SUMS-${version}"
else
    shasum -a 256 "sshm_${version}_"*.tar.gz > "SHA256SUMS-${version}"
fi
