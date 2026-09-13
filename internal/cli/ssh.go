package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

func sshOptions(o execOptions) []string {
	batch := "yes"
	if o.askpass != "" {
		batch = "no"
	}
	args := []string{
		"-T", "-o", "BatchMode=" + batch, "-o", "ConnectTimeout=10",
		"-o", "ClearAllForwardings=yes", "-o", "RemoteCommand=none",
		"-o", "SessionType=default", "-o", "ForkAfterAuthentication=no",
		"-o", "StdinNull=no", "-o", "PermitLocalCommand=no",
	}
	if o.askpass != "" {
		args = append(args, "-o", "NumberOfPasswordPrompts=1")
	}
	if o.config != "" {
		args = append(args, "-F", o.config)
	}
	return args
}

// Ask OpenSSH to interpret configuration rather than reimplementing Host,
// Match, ProxyJump, authentication and identity resolution. -G does not open an
// SSH connection, but trusted config's Match exec may run local commands.
func effectiveConfig(ctx context.Context, ssh string, o execOptions) ([]byte, error) {
	args := append(sshOptions(o), "-G", "-o", "ControlMaster=no", "-o", "ControlPath=none", "-o", "ControlPersist=no", "--", o.host, o.command)
	var out, diag bytes.Buffer
	w := &limitedWriter{out: &out, limit: 1024 * 1024}
	e := &limitedWriter{out: &diag, limit: 16384}
	c := command(ctx, ssh, args, nil, w, e)
	applyAskpass(c, o.askpass)
	err := c.Run()
	if err != nil {
		return nil, fmt.Errorf("OpenSSH 配置解析失败：%w\n%s", err, strings.TrimSpace(diag.String()))
	}
	if w.dropped > 0 {
		return nil, errors.New("OpenSSH 有效配置超过 1 MiB，无法生成可靠连接标识")
	}
	return out.Bytes(), nil
}

func controlPath(dir string, config []byte, persist time.Duration, askpass string) string {
	h := sha256.New()
	h.Write([]byte("sshm-control-v1\x00"))
	h.Write(config)
	// The same endpoint with a different agent must not silently share auth or
	// forwarded-agent state. Persist is part of the key so its TTL stays truthful.
	fmt.Fprintf(h, "\x00%s\x00%d\x00%s", os.Getenv("SSH_AUTH_SOCK"), persist, askpass)
	return filepath.Join(dir, "c-"+hex.EncodeToString(h.Sum(nil))[:32])
}

func socketAlive(ctx context.Context, ssh, path string) bool {
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	// -O check talks only to the named local socket. No user's Match/ProxyCommand
	// is evaluated and failure never falls back to opening a network connection.
	return command(ctx, ssh, []string{"-F", os.DevNull, "-S", path, "-O", "check", "sshm-local-check"}, nil, io.Discard, io.Discard).Run() == nil
}

func execute(ctx context.Context, ssh string, o execOptions, stdin *os.File, stdout, stderr io.Writer) int {
	args := sshOptions(o)
	var unlock func()
	var socket string
	if o.persist > 0 {
		cfg, err := effectiveConfig(ctx, ssh, o)
		if err != nil {
			return localFailure(ctx, stderr, err, false)
		}
		dir, err := privateDir(true)
		if err != nil {
			return localFailure(ctx, stderr, err, false)
		}
		socket = controlPath(dir, cfg, o.persist, o.askpass)
		unlock, err = acquireLock(ctx, socket+".lock")
		if err != nil {
			return localFailure(ctx, stderr, err, false)
		}
		if socketAlive(ctx, ssh, socket) {
			unlock()
			unlock = nil
		} else if info, err := os.Lstat(socket); err == nil {
			if info.Mode()&os.ModeSocket == 0 {
				unlock()
				return localFailure(ctx, stderr, fmt.Errorf("控制路径被非 socket 文件占用：%s", socket), false)
			}
			if err := os.Remove(socket); err != nil {
				unlock()
				return localFailure(ctx, stderr, err, false)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			unlock()
			return localFailure(ctx, stderr, err, false)
		}
		args = append(args, "-o", "ControlMaster=auto", "-o", fmt.Sprintf("ControlPersist=%.0f", math.Ceil(o.persist.Seconds())), "-S", socket)
	} else {
		args = append(args, "-o", "ControlMaster=no", "-o", "ControlPersist=no", "-S", "none")
	}
	if o.debug {
		args = append(args, "-v")
	}
	args = append(args, "--", o.host, o.command)
	runCtx, abort := context.WithCancel(ctx)
	defer abort()
	out := &limitedWriter{out: stdout, limit: o.maxOutput, abort: abort}
	diag := &limitedWriter{out: stderr, limit: o.maxOutput, abort: abort}
	c := command(runCtx, ssh, args, stdin, out, diag)
	applyAskpass(c, o.askpass)
	if err := c.Start(); err != nil {
		if unlock != nil {
			unlock()
		}
		return localFailure(ctx, stderr, err, false)
	}
	// Serialize only cold connection establishment, never the remote command.
	// Once OpenSSH publishes its listening socket, other invocations can attach.
	done, released := make(chan struct{}), make(chan struct{})
	if unlock != nil {
		go func() {
			defer close(released)
			defer unlock()
			tick := time.NewTicker(15 * time.Millisecond)
			defer tick.Stop()
			for {
				select {
				case <-done:
					return
				case <-tick.C:
					if info, err := os.Lstat(socket); err == nil && info.Mode()&os.ModeSocket != 0 {
						return
					}
				}
			}
		}()
	} else {
		close(released)
	}
	err := c.Wait()
	close(done)
	<-released
	for _, item := range []struct {
		name string
		w    *limitedWriter
	}{{"stdout", out}, {"stderr", diag}} {
		if item.w.dropped > 0 {
			fmt.Fprintf(stderr, "\nsshm: 输出已截断：%s 显示 %d 字节，省略 %d 字节；使用 --max-output 0 保存完整输出。\n", item.name, item.w.written, item.w.dropped)
		}
	}
	if ctx.Err() != nil {
		return localFailure(ctx, stderr, ctx.Err(), true)
	}
	if out.err != nil || diag.err != nil {
		return localFailure(ctx, stderr, fmt.Errorf("输出写入失败：%w", errors.Join(out.err, diag.err)), true)
	}
	code := exitCode(err)
	if code == 255 {
		fmt.Fprintln(stderr, "\nsshm: SSH 返回 255：可能是连接/认证错误，也可能是远端退出码 255；仅凭此状态无法确认执行结果，请勿自动重试。")
	} else if err != nil {
		// A genuine remote 125 should stay unadorned; local I/O errors and
		// inherited-pipe timeouts must still get a diagnostic even after exit 0.
		var remote *exec.ExitError
		if !errors.As(err, &remote) || remote.ExitCode() < 0 {
			return localFailure(ctx, stderr, err, true)
		}
	}
	return code
}

// Reuse OpenSSH's credential-helper mechanism. The CLI never receives or
// stores the returned credential and stdin stays dedicated to the remote job.
func applyAskpass(c *exec.Cmd, program string) {
	if program == "" {
		return
	}
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "SSH_ASKPASS=") && !strings.HasPrefix(v, "SSH_ASKPASS_REQUIRE=") {
			c.Env = append(c.Env, v)
		}
	}
	c.Env = append(c.Env, "SSH_ASKPASS="+program, "SSH_ASKPASS_REQUIRE=force")
}

func localFailure(ctx context.Context, out io.Writer, err error, started bool) int {
	state := "本次远端命令尚未启动。"
	if started {
		state = "远端执行结果未知，不能确认远端任务已停止；请勿自动重试。"
	}
	if errors.Is(ctx.Err(), context.DeadlineExceeded) {
		fmt.Fprintf(out, "\nsshm: 本地等待超时。%s\n", state)
		return 124
	}
	if ctx.Err() != nil {
		fmt.Fprintf(out, "\nsshm: 本地调用已取消。%s\n", state)
		return 130
	}
	fmt.Fprintf(out, "\nsshm: 本地错误：%v。%s\n", err, state)
	return 125
}
