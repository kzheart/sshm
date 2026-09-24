package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"golang.org/x/crypto/ssh"
	"golang.org/x/crypto/ssh/knownhosts"
	"golang.org/x/sys/unix"
)

// Missing trust files are normal before the first connection. Invalid or
// unreadable existing files must still fail closed.
func knownHostsContents(path string) ([]byte, error) {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	return privateRead(path, 8<<20, false)
}

func readKnownHosts(path string) (ssh.HostKeyCallback, error) {
	data, err := knownHostsContents(path)
	if err != nil {
		return nil, err
	}
	if data == nil {
		return func(string, net.Addr, ssh.PublicKey) error { return &knownhosts.KeyError{} }, nil
	}
	return knownhosts.New(path)
}

func unknownHostKey(err error) bool {
	var keyErr *knownhosts.KeyError
	return errors.As(err, &keyErr) && len(keyErr.Want) == 0
}

// Accept and persist previously unseen hosts, but never replace a known key.
// The algorithm-selection probe uses readKnownHosts directly, not this callback.
func verifyHostKey(ctx context.Context, path, host string, remote net.Addr, key ssh.PublicKey) error {
	cb, err := readKnownHosts(path)
	if err != nil {
		return errors.New("无法读取 known_hosts；检查文件权限与格式")
	}
	if err = cb(host, remote, key); err == nil {
		return nil
	}
	if !unknownHostKey(err) {
		return fmt.Errorf("主机公钥已变化、被撤销或不受信任（%s）；请核对后更新 known_hosts", ssh.FingerprintSHA256(key))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return errors.New("无法创建 known_hosts 目录")
	}
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND|unix.O_NONBLOCK|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return errors.New("无法保存首次连接的主机公钥；检查 known_hosts 写入权限")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return errors.New("known_hosts 必须是普通文件")
	}
	// Lock across both CLI processes and concurrent broker connections, and
	// recheck after acquiring the lock so conflicting first keys cannot both win.
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = unix.Flock(int(f.Fd()), unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, unix.EWOULDBLOCK) && !errors.Is(err, unix.EINTR) {
			return errors.New("无法锁定 known_hosts")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	defer unix.Flock(int(f.Fd()), unix.LOCK_UN)
	cb, err = readKnownHosts(path)
	if err != nil {
		return errors.New("无法读取 known_hosts；检查文件权限与格式")
	}
	if err = cb(host, remote, key); err == nil {
		return nil
	}
	if !unknownHostKey(err) {
		return fmt.Errorf("主机公钥已变化、被撤销或不受信任（%s）；请核对后更新 known_hosts", ssh.FingerprintSHA256(key))
	}
	// Leading newline preserves a preexisting final line without a newline.
	line := "\n" + knownhosts.Line([]string{host}, key) + "\n"
	if _, err := f.WriteString(line); err != nil {
		return errors.New("无法写入首次连接的主机公钥")
	}
	if err := f.Sync(); err != nil {
		return errors.New("无法保存首次连接的主机公钥")
	}
	return nil
}
