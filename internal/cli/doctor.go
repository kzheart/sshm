package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func doctorCommand(ctx context.Context, args []string, stdout, stderr io.Writer, version string) int {
	f := flags("doctor", "用法：sshm doctor [-F 配置文件]\n只检查本地工具、配置发现和本工具的控制 socket。不连接服务器，不执行 Match exec。", stderr)
	cfg := configFlag(f)
	if err := parse(f, args); err != nil {
		return parseError(err, stderr)
	}
	if f.NArg() != 0 {
		return parseError(errors.New("doctor 不接受目标；远端排查请使用 exec --debug"), stderr)
	}
	fmt.Fprintf(stdout, "sshm %s\n", version)
	code := 0
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Fprintf(stdout, "OpenSSH: 不可用（%v）\n", err)
		code = 1
	} else {
		fmt.Fprintf(stdout, "OpenSSH: %s\n", ssh)
		checkCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		w := &limitedWriter{out: stdout, limit: 4096}
		if err := command(checkCtx, ssh, []string{"-V"}, nil, w, w).Run(); err != nil {
			fmt.Fprintf(stdout, "OpenSSH 版本检查失败：%v\n", err)
			code = 1
		}
		cancel()
	}
	p, err := configPath(*cfg)
	if err != nil {
		fmt.Fprintf(stdout, "配置: %v\n", err)
		code = 1
	} else if files, err := configFiles(p); err != nil {
		fmt.Fprintf(stdout, "配置: %v\n", err)
		code = 1
	} else {
		for _, file := range files {
			fmt.Fprintf(stdout, "配置来源: %s\n", file.path)
		}
		hosts, warnings, err := discover(files)
		if err != nil {
			fmt.Fprintf(stdout, "别名发现失败: %v\n", err)
			code = 1
		} else {
			fmt.Fprintf(stdout, "候选别名: %d（未测试认证或远端可达性）\n", len(hosts))
		}
		for _, warning := range warnings {
			fmt.Fprintf(stdout, "发现提示: %s\n", warning)
			code = 1
		}
	}
	dir, err := privateDir(false)
	if errors.Is(err, os.ErrNotExist) {
		fmt.Fprintln(stdout, "控制连接: 尚未创建运行目录；无本工具常驻服务")
	} else if err != nil {
		fmt.Fprintf(stdout, "控制连接: %v\n", err)
		code = 1
	} else {
		fmt.Fprintf(stdout, "运行目录: %s\n", dir)
		entries, err := os.ReadDir(dir)
		if err != nil {
			fmt.Fprintf(stdout, "控制连接: %v\n", err)
			return 1
		}
		count := 0
		for _, entry := range entries {
			if !strings.HasPrefix(entry.Name(), "c-") || entry.Type()&os.ModeSocket == 0 {
				continue
			}
			count++
			state := "失效或不可访问（未删除）"
			if ssh != "" && socketAlive(ctx, ssh, filepath.Join(dir, entry.Name())) {
				state = "活动，空闲后由 OpenSSH 回收"
			}
			fmt.Fprintf(stdout, "控制连接 %s: %s\n", entry.Name(), state)
		}
		fmt.Fprintf(stdout, "控制 socket 数: %d\n", count)
	}
	return code
}
