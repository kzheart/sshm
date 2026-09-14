package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/crypto/ssh"
	"golang.org/x/term"
)

func shellCommand(ctx context.Context, args []string, stdin *os.File, stdout, stderr io.Writer) int {
	f := flags("shell", "用法：sshm shell 主机 [--command 命令] [-F TOML]；提供真实 PTY，宿主需保持进程并继续读写。", stderr)
	path := configFlag(f)
	command := f.String("command", "", "可选：启动指定远端程序，默认登录 shell")
	timeout := f.Duration("timeout", 0, "总期限；默认 0 不限时")
	cols := f.Int("cols", 80, "无本地终端时的列数")
	rows := f.Int("rows", 24, "无本地终端时的行数")
	if err := parse(f, args); err != nil {
		return parseError(err, stderr)
	}
	if f.NArg() != 1 || !validHost(f.Arg(0)) || *timeout < 0 || *cols < 1 || *rows < 1 || *cols > 10000 || *rows > 10000 {
		return parseError(errors.New("主机或终端参数无效"), stderr)
	}
	ctx, cancelLife := context.WithCancel(ctx)
	defer cancelLife()
	if *timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *timeout)
		defer cancel()
	}
	p, err := configPath(*path)
	if err != nil {
		return parseError(err, stderr)
	}
	cfg, err := loadConfig(p)
	if err != nil {
		return localFailure(ctx, stderr, err, false)
	}
	b, err := connect(ctx, cfg, f.Arg(0))
	if err != nil {
		return localFailure(ctx, stderr, err, false)
	}
	defer b.Close()
	stop := context.AfterFunc(ctx, func() { b.Close() })
	defer stop()
	s, err := b.client.NewSession()
	if err != nil {
		return localFailure(ctx, stderr, errors.New("无法建立终端通道"), false)
	}
	defer s.Close()
	out := &limitedWriter{out: contextOutput(ctx, stdout), abort: func() { b.Close() }}
	diag := &limitedWriter{out: contextOutput(ctx, stderr), abort: func() { b.Close() }}
	s.Stdout = out
	s.Stderr = diag
	localTTY := stdin != nil && term.IsTerminal(int(stdin.Fd()))
	if localTTY {
		if w, h, e := term.GetSize(int(stdin.Fd())); e == nil {
			*cols = w
			*rows = h
		}
	}
	if err = s.RequestPty("xterm-256color", *rows, *cols, ssh.TerminalModes{ssh.ECHO: 1, ssh.TTY_OP_ISPEED: 38400, ssh.TTY_OP_OSPEED: 38400}); err != nil {
		return localFailure(ctx, stderr, errors.New("远端拒绝 PTY"), false)
	}
	if localTTY {
		state, e := term.MakeRaw(int(stdin.Fd()))
		if e != nil {
			return localFailure(ctx, stderr, errors.New("无法切换本地终端模式"), false)
		}
		defer term.Restore(int(stdin.Fd()), state)
	}
	in, err := s.StdinPipe()
	if err != nil {
		return localFailure(ctx, stderr, err, false)
	}
	if *command == "" {
		err = s.Shell()
	} else {
		err = s.Start(*command)
	}
	if err != nil {
		return localFailure(ctx, stderr, errors.New("终端启动请求未获确认"), true)
	}
	inputCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer in.Close()
		if stdin != nil {
			_, _ = io.Copy(in, &contextReader{ctx: inputCtx, file: stdin})
		}
		// An interactive owner that closes stdin has ended the session. Keeping
		// only the remote PTY alive here could orphan a quiet foreground CLI.
		// Piped scripts belong to exec --stdin, which separately waits for output.
		if inputCtx.Err() == nil {
			cancelLife()
		}
	}()
	resized := make(chan os.Signal, 1)
	resizeDone := make(chan struct{})
	if localTTY {
		signal.Notify(resized, syscall.SIGWINCH)
	}
	go func() {
		defer close(resizeDone)
		for {
			select {
			case <-inputCtx.Done():
				return
			case <-resized:
				if w, h, e := term.GetSize(int(stdin.Fd())); e == nil {
					_ = s.WindowChange(h, w)
				}
			}
		}
	}()
	err = s.Wait()
	cancel()
	b.Close()
	signal.Stop(resized)
	<-done
	<-resizeDone
	if ctx.Err() != nil {
		return localFailure(ctx, stderr, ctx.Err(), true)
	}
	if out.err != nil || diag.err != nil {
		return localFailure(ctx, stderr, errors.New("本地终端输出写入失败"), true)
	}
	if err == nil {
		return 0
	}
	var exit *ssh.ExitError
	if errors.As(err, &exit) && exit.ExitStatus() >= 0 {
		return exit.ExitStatus()
	}
	return localFailure(ctx, stderr, errors.New("终端断开且未收到有效退出码"), true)
}
