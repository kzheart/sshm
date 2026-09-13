// Package cli implements a small, text-first adapter around system OpenSSH.
package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode"
)

const help = `sshm — 面向 Agent 的精简 SSH 工具

用法：
  sshm hosts [筛选文本] [-F 配置文件]
  sshm exec 主机 --command '远端命令' [选项]
  sshm doctor [-F 配置文件]

命令：
  hosts   列出 SSH 配置中明确的候选别名，不连接服务器
  exec    非交互执行；保留原始 stdout、stderr 和远端退出码
  doctor  本地诊断；不建立远端连接或清理其他进程

使用 sshm <命令> --help 查看选项。--version 查看版本。
交互终端直接使用 ssh；传输使用 scp/sftp，目录同步按需使用 rsync。
`

// Run is also the integration seam: tests supply isolated config and runtime dirs.
func Run(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer, version string) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stdout, help)
		return 0
	}
	if args[0] == "--version" {
		fmt.Fprintf(stdout, "sshm %s\n", version)
		return 0
	}
	switch args[0] {
	case "hosts":
		return hostsCommand(args[1:], stdout, stderr)
	case "exec":
		return execCommand(ctx, args[1:], stdin, stdout, stderr)
	case "doctor":
		return doctorCommand(ctx, args[1:], stdout, stderr, version)
	default:
		fmt.Fprintf(stderr, "sshm: 参数错误：未知命令 %q；使用 sshm --help。\n", args[0])
		return 2
	}
}

func flags(name, usage string, out io.Writer) *flag.FlagSet {
	f := flag.NewFlagSet(name, flag.ContinueOnError)
	f.SetOutput(out)
	f.Usage = func() {
		fmt.Fprintln(out, usage)
		f.PrintDefaults()
	}
	return f
}

func configFlag(f *flag.FlagSet) *string {
	var config string
	f.StringVar(&config, "config", "", "SSH 配置文件（与 ssh -F 相同，默认沿用用户及系统配置）")
	f.StringVar(&config, "F", "", "--config 的简写")
	return &config
}

// Standard flag stops at a positional argument. Reorder only known options,
// preserving their values byte-for-byte, so `exec host --command '-x'` works.
func parse(f *flag.FlagSet, args []string) error {
	var opts, rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			rest = append(rest, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			rest = append(rest, a)
			continue
		}
		name, _, hasValue := strings.Cut(strings.TrimLeft(a, "-"), "=")
		if name == "help" || name == "h" {
			f.Usage()
			return flag.ErrHelp
		}
		option := f.Lookup(name)
		if option == nil {
			return fmt.Errorf("未知选项 %q", a)
		}
		opts = append(opts, a)
		b, isBool := option.Value.(interface{ IsBoolFlag() bool })
		if !hasValue && !(isBool && b.IsBoolFlag()) {
			i++
			if i == len(args) {
				return fmt.Errorf("选项 %s 需要参数", a)
			}
			opts = append(opts, args[i])
		}
	}
	return f.Parse(append(append(opts, "--"), rest...))
}

func parseError(err error, out io.Writer) int {
	if errors.Is(err, flag.ErrHelp) {
		return 0
	}
	fmt.Fprintf(out, "sshm: 参数错误：%v\n", err)
	return 2
}

func configPath(p string) (string, error) {
	if p == "" {
		return "", nil
	}
	if strings.HasPrefix(p, "~/") {
		h, err := os.UserHomeDir()
		if err != nil {
			return "", err
		}
		p = filepath.Join(h, p[2:])
	}
	p, err := filepath.Abs(p)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() && p != os.DevNull {
		return "", fmt.Errorf("配置不是普通文件：%s", p)
	}
	return p, nil
}

func validHost(host string) bool {
	return host != "" && !strings.HasPrefix(host, "-") && !strings.ContainsAny(host, "\x00*?!") &&
		strings.IndexFunc(host, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

type execOptions struct {
	config, host, command, input string
	timeout, persist             time.Duration
	maxOutput                    int64
	debug                        bool
}

func execCommand(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) int {
	f := flags("exec", "用法：sshm exec 主机 --command '远端命令' [选项]\n命令原样交给远端解释，不注入 shell、sudo 或 timeout。默认 stdin 为 EOF。", stderr)
	cfg := configFlag(f)
	o := execOptions{}
	f.StringVar(&o.command, "command", "", "必填：远端命令字符串；调用端应正确引用本地 shell 参数")
	f.StringVar(&o.input, "stdin", "", "读取指定文件作为 stdin；- 表示继承调用端 stdin")
	f.DurationVar(&o.timeout, "timeout", 2*time.Minute, "本地总等待期限，包含配置解析和连接；0 表示无限")
	f.DurationVar(&o.persist, "persist", time.Minute, "OpenSSH 控制连接空闲期限；0 禁用复用（不是永久保持）")
	f.Int64Var(&o.maxOutput, "max-output", 65536, "每个输出流最多显示的字节数；0 完整流式输出，可重定向到文件")
	f.BoolVar(&o.debug, "debug", false, "将 OpenSSH 调试信息写入 stderr")
	if err := parse(f, args); err != nil {
		return parseError(err, stderr)
	}
	if f.NArg() != 1 || !validHost(f.Arg(0)) || strings.TrimSpace(o.command) == "" || strings.ContainsRune(o.command, 0) {
		return parseError(errors.New("需要一个明确主机和非空 --command；使用 sshm exec --help"), stderr)
	}
	if o.timeout < 0 || o.persist < 0 || o.maxOutput < 0 {
		return parseError(errors.New("期限和输出上限不能为负数"), stderr)
	}
	var err error
	o.config, err = configPath(*cfg)
	if err != nil {
		return parseError(err, stderr)
	}
	o.host = f.Arg(0)
	ssh, err := exec.LookPath("ssh")
	if err != nil {
		fmt.Fprintf(stderr, "sshm: 本地错误：找不到 OpenSSH 客户端：%v\n", err)
		return 125
	}
	var input *os.File
	if o.input == "-" {
		input = stdin
	} else if o.input != "" {
		// A FIFO must not block during open, before we can reject it or start
		// the execution deadline. Regular files ignore O_NONBLOCK.
		input, err = os.OpenFile(o.input, os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			fmt.Fprintf(stderr, "sshm: 本地错误：无法打开 stdin 文件：%v\n", err)
			return 125
		}
		defer input.Close()
		info, statErr := input.Stat()
		if statErr != nil || !info.Mode().IsRegular() {
			return parseError(errors.New("--stdin 路径必须是普通文件；管道输入请使用 --stdin -"), stderr)
		}
	}
	if o.timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, o.timeout)
		defer cancel()
	}
	return execute(ctx, ssh, o, input, stdout, stderr)
}
