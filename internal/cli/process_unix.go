//go:build darwin || linux

package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"
)

func configureProcess(c *exec.Cmd) {
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error {
		err := syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		if errors.Is(err, syscall.ESRCH) {
			return os.ErrProcessDone
		}
		return err
	}
}

func runtimePath() string {
	if p := os.Getenv("SSHM_RUNTIME_DIR"); p != "" {
		return p
	}
	// Keep ControlPath below Unix socket path limits, including macOS's 104 bytes.
	return filepath.Join("/tmp", fmt.Sprintf("sshm-%d", os.Getuid()))
}

func privateDir(create bool) (string, error) {
	p, err := filepath.Abs(runtimePath())
	if err != nil {
		return "", err
	}
	if create {
		if err := os.Mkdir(p, 0700); err != nil && !errors.Is(err, os.ErrExist) {
			return "", err
		}
	}
	info, err := os.Lstat(p)
	if err != nil {
		return "", err
	}
	st, ok := info.Sys().(*syscall.Stat_t)
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0077 != 0 || !ok || st.Uid != uint32(os.Getuid()) {
		return "", fmt.Errorf("运行目录必须由当前用户拥有、权限为 0700 且不是符号链接：%s", p)
	}
	if len(filepath.Join(p, "c-"+string(make([]byte, 32)))) >= 104 {
		return "", fmt.Errorf("控制 socket 路径过长；请将 SSHM_RUNTIME_DIR 设为更短的私有目录")
	}
	return p, nil
}

// Never unlink a lock file: a waiter may already hold its inode. The tiny files
// are stable per effective configuration, not per call or chat session.
func acquireLock(ctx context.Context, path string) (func(), error) {
	fd, err := syscall.Open(path, syscall.O_CREAT|syscall.O_RDWR|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	f := os.NewFile(uintptr(fd), path)
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("连接锁不是普通文件：%s", path)
	}
	for {
		if err := ctx.Err(); err != nil {
			f.Close()
			return nil, err
		}
		err = syscall.Flock(fd, syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() { _ = syscall.Flock(fd, syscall.LOCK_UN); _ = f.Close() }, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			f.Close()
			return nil, err
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, ctx.Err()
		case <-time.After(20 * time.Millisecond):
		}
	}
}
