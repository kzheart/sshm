package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
)

func doctorCommand(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	f := flags("doctor", "用法：sshm doctor [-F TOML]；仅本地检查，不连接服务器。", stderr)
	path := configFlag(f)
	if err := parse(f, args); err != nil {
		return parseError(err, stderr)
	}
	if f.NArg() != 0 {
		return parseError(errors.New("doctor 不接受目标"), stderr)
	}
	p, err := configPath(*path)
	if err != nil {
		return parseError(err, stderr)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		return localFailure(ctx, stderr, err, false)
	}
	fmt.Fprintf(stdout, "sshm %s\nSSH 实现：golang.org/x/crypto/ssh（内置，无外部 SSH 程序）\n配置：%s\n服务器：%d（未测试连接）\n生命周期：exec 自动复用；连接空闲 60 秒关闭，服务空闲自动退出；--fresh 使用独立连接\n", version, p, len(cfg.Servers))
	if _, err := readKnownHosts(cfg.KnownHosts); err != nil {
		fmt.Fprintln(stdout, "known_hosts：无法读取或格式无效")
		return 1
	}
	fmt.Fprintln(stdout, "known_hosts：检查通过；首次连接自动记录新主机公钥，文件不存在时自动创建")
	return 0
}
