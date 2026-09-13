#!/bin/sh
set -eu

root=$(CDPATH= cd -- "$(dirname -- "$0")" && pwd)
prefix=${HOME}/.local
skill_dir=

while [ "$#" -gt 0 ]; do
    case "$1" in
        --prefix|--skill-dir)
            if [ "$#" -lt 2 ] || [ -z "$2" ]; then
                echo "安装参数缺少路径：$1" >&2
                exit 2
            fi
            case "$1" in
                --prefix) prefix=$2 ;;
                --skill-dir) skill_dir=$2 ;;
            esac
            shift 2
            ;;
        -h|--help)
            echo '用法：./install.sh [--prefix "$HOME/.local"] [--skill-dir /所用Agent的skills目录]'
            echo '从本地构建产物或解压的发布包安装，不下载任何内容，不修改 shell 或 Agent 配置。'
            exit 0
            ;;
        *) echo "未知安装参数：$1" >&2; exit 2 ;;
    esac
done

binary=$root/sshm
if [ ! -f "$binary" ]; then
    binary=$root/bin/sshm
fi
if [ ! -f "$binary" ]; then
    echo '缺少二进制；源码目录先运行 make build，或解压对应平台发布包。' >&2
    exit 1
fi

mkdir -p "$prefix/bin"
staged=$(mktemp "$prefix/bin/.sshm-install.XXXXXX")
trap 'rm -f "$staged"' EXIT HUP INT TERM
cp "$binary" "$staged"
chmod 755 "$staged"
mv -f "$staged" "$prefix/bin/sshm"
echo "已安装：$prefix/bin/sshm"
if [ -n "$skill_dir" ]; then
    mkdir -p "$skill_dir/sshm"
    staged=$(mktemp "$skill_dir/sshm/.SKILL-install.XXXXXX")
    cp "$root/skills/sshm/SKILL.md" "$staged"
    chmod 644 "$staged"
    mv -f "$staged" "$skill_dir/sshm/SKILL.md"
    echo "已安装 Skill：$skill_dir/sshm/SKILL.md"
fi
case ":$PATH:" in
    *":$prefix/bin:"*) ;;
    *) echo "请确保 $prefix/bin 位于 Agent 和终端使用的 PATH 中，或使用绝对路径。" ;;
esac
