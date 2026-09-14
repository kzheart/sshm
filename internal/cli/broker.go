package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

type brokerEnabled struct{}

// Main enables cross-process reuse only for the executable. Run remains an
// in-process testing/embedding seam without spawning a copy of the test binary.
func Main(ctx context.Context, args []string, version string) int {
	if len(args) == 2 && args[0] == "_serve" {
		return serve(ctx, args[1])
	}
	return Run(context.WithValue(ctx, brokerEnabled{}, true), args, os.Stdin, os.Stdout, os.Stderr, version)
}

type brokerRequest struct {
	Config, Host, Command string
	MaxOutput             int64
	Deadline              time.Time
	Debug, Input          bool
}

func runtimePath() (string, error) {
	dir := os.Getenv("SSHM_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/tmp/sshm-%d", os.Getuid())
	}
	dir, err := filepath.Abs(dir)
	if err != nil {
		return "", err
	}
	if err := os.Mkdir(dir, 0700); err != nil && !os.IsExist(err) {
		return "", err
	}
	i, err := os.Lstat(dir)
	if err != nil {
		return "", err
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || !i.IsDir() || i.Mode().Perm() != 0700 || st.Uid != uint32(os.Getuid()) {
		return "", errors.New("连接服务目录必须属于当前用户、非符号链接且权限为 700")
	}
	return dir, nil
}

func dialBroker(ctx context.Context, dir string) (*net.UnixConn, error) {
	c, err := (&net.Dialer{}).DialContext(ctx, "unix", filepath.Join(dir, "control.sock"))
	if err != nil {
		return nil, err
	}
	return c.(*net.UnixConn), nil
}

func startBroker(ctx context.Context, dir string) (*net.UnixConn, error) {
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	lock, err := os.OpenFile(filepath.Join(dir, "service.lock"), os.O_CREATE|os.O_RDWR|unix.O_NOFOLLOW|unix.O_NONBLOCK, 0600)
	if err != nil {
		return nil, err
	}
	defer lock.Close()
	i, err := lock.Stat()
	if err != nil {
		return nil, err
	}
	st, ok := i.Sys().(*syscall.Stat_t)
	if !ok || !i.Mode().IsRegular() || i.Mode().Perm() != 0600 || st.Uid != uint32(os.Getuid()) {
		return nil, errors.New("无效的连接服务锁文件")
	}
	for {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if c, err := dialBroker(ctx, dir); err == nil {
			return c, nil
		}
		err = unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) {
			return nil, err
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	// Keep the SAME lock description alive in the child for its entire lifetime.
	// Concurrent agents cannot spawn duplicate services, even on stale sockets.
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	defer r.Close()
	defer w.Close()
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer null.Close()
	proc, err := os.StartProcess(exe, []string{exe, "_serve", dir}, &os.ProcAttr{
		Dir: "/", Env: os.Environ(), Files: []*os.File{null, null, null, lock, w},
		Sys: &syscall.SysProcAttr{Setsid: true},
	})
	if err != nil {
		return nil, err
	}
	w.Close()
	// Wait reaps a failed startup while the calling CLI is alive. A successful
	// service outlives this client and is adopted/reaped by the OS on client exit.
	go proc.Wait()
	var ready [1]byte
	_, err = io.ReadFull(&contextReader{ctx: ctx, file: r}, ready[:])
	if err != nil || ready[0] != 1 {
		proc.Kill()
		return nil, errors.New("连接服务启动失败")
	}
	return dialBroker(ctx, dir)
}

func brokerExecute(ctx context.Context, o execOptions, input, stdout, stderr *os.File) int {
	dir, err := runtimePath()
	var c *net.UnixConn
	if err == nil {
		c, err = startBroker(ctx, dir)
	}
	if err != nil {
		// Safe fallback: no request was sent, so nothing can have run remotely.
		if o.debug {
			fmt.Fprintln(stderr, "sshm: 连接服务不可用，使用独立连接")
		}
		return execute(ctx, o, input, stdout, stderr)
	}
	defer c.Close()
	stop := context.AfterFunc(ctx, func() { c.Close() })
	defer stop()
	fdInput := input
	if fdInput == nil {
		fdInput, err = os.Open(os.DevNull)
		if err != nil {
			return localFailure(ctx, stderr, err, false)
		}
		defer fdInput.Close()
	}
	// SCM_RIGHTS preserves binary streams/backpressure without a second stream
	// framing implementation. The service closes all received descriptors per call.
	_, _, err = c.WriteMsgUnix([]byte{1}, unix.UnixRights(int(fdInput.Fd()), int(stdout.Fd()), int(stderr.Fd())), nil)
	if err != nil {
		return localFailure(ctx, stderr, errors.New("无法提交连接服务请求"), false)
	}
	req := brokerRequest{Config: o.config, Host: o.host, Command: o.command, MaxOutput: o.maxOutput, Debug: o.debug, Input: input != nil}
	req.Deadline, _ = ctx.Deadline()
	if err := json.NewEncoder(c).Encode(req); err != nil {
		return localFailure(ctx, stderr, errors.New("连接服务请求未获确认"), true)
	}
	var code int
	if err := json.NewDecoder(c).Decode(&code); err != nil {
		return localFailure(ctx, stderr, errors.New("连接服务中断，未取得退出状态"), true)
	}
	return code
}

func serve(ctx context.Context, dir string) int {
	lock := os.NewFile(3, "service-lock")
	ready := os.NewFile(4, "service-ready")
	defer lock.Close()
	defer ready.Close()
	// Only the launcher supplies these inherited files; no public service command.
	if _, err := lock.Stat(); err != nil {
		return 125
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return 125
	}
	path := filepath.Join(dir, "control.sock")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return 125
	}
	l, err := net.ListenUnix("unix", &net.UnixAddr{Name: path, Net: "unix"})
	if err != nil {
		return 125
	}
	defer l.Close()
	if os.Chmod(path, 0600) != nil {
		return 125
	}
	pool := newPool(poolIdle)
	defer pool.close()
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	stop := context.AfterFunc(ctx, func() { l.Close(); pool.close() })
	defer stop()
	ready.Write([]byte{1})
	ready.Close()
	var wg sync.WaitGroup
	defer func() { cancel(); pool.close(); wg.Wait() }()
	slots := make(chan struct{}, 64)
	last := time.Now()
	for {
		l.SetDeadline(time.Now().Add(time.Second))
		c, err := l.AcceptUnix()
		if err != nil {
			if ctx.Err() != nil {
				return 0
			}
			if e, ok := err.(net.Error); !ok || !e.Timeout() {
				return 125
			}
			pool.sweep(time.Now())
			if len(slots) == 0 && time.Since(last) >= poolIdle {
				return 0
			}
			if len(slots) > 0 {
				last = time.Now()
			}
			continue
		}
		last = time.Now()
		select {
		case slots <- struct{}{}:
			wg.Add(1)
			go func() { defer wg.Done(); defer func() { <-slots }(); handleBroker(ctx, c, pool) }()
		default:
			c.Close()
		}
	}
}

func handleBroker(parent context.Context, c *net.UnixConn, pool *connectionPool) {
	defer c.Close()
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	marker := make([]byte, 1)
	oob := make([]byte, unix.CmsgSpace(3*4))
	n, oobn, flags, _, err := c.ReadMsgUnix(marker, oob)
	var files []*os.File
	defer func() {
		for _, f := range files {
			f.Close()
		}
	}()
	msgs, parseErr := unix.ParseSocketControlMessage(oob[:oobn])
	for _, m := range msgs {
		fds, e := unix.ParseUnixRights(&m)
		if e != nil {
			parseErr = e
			continue
		}
		for _, fd := range fds {
			unix.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "client-stream"))
		}
	}
	if err != nil || parseErr != nil || flags&unix.MSG_CTRUNC != 0 || n != 1 || marker[0] != 1 || len(files) != 3 {
		return
	}
	var req brokerRequest
	if json.NewDecoder(io.LimitReader(c, 4<<20)).Decode(&req) != nil {
		return
	}
	if !filepath.IsAbs(req.Config) || !validHost(req.Host) || req.Command == "" || req.MaxOutput < 0 {
		return
	}
	c.SetReadDeadline(time.Time{})
	ctx, cancel := context.WithCancel(parent)
	defer cancel()
	if !req.Deadline.IsZero() {
		var stop context.CancelFunc
		ctx, stop = context.WithDeadline(ctx, req.Deadline)
		defer stop()
	}
	// EOF also covers SIGKILL or an Agent disappearing without graceful cleanup.
	monitor := make(chan struct{})
	go func() { defer close(monitor); var b [1]byte; c.Read(b[:]); cancel() }()
	defer func() { c.Close(); <-monitor }()
	// Release output pipe references promptly on cancellation, even while a
	// peer delays channel-close acknowledgement and a sibling is still running.
	stopOutput := context.AfterFunc(ctx, func() { files[1].Close(); files[2].Close() })
	defer stopOutput()
	var input *os.File
	if req.Input {
		input = files[0]
	}
	o := execOptions{config: req.Config, host: req.Host, command: req.Command, maxOutput: req.MaxOutput, debug: req.Debug}
	code := executeWithPool(ctx, o, input, files[1], contextOutput(ctx, files[2]), pool)
	json.NewEncoder(c).Encode(code)
}
