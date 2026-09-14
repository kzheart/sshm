package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"syscall"
	"time"

	"github.com/pkg/sftp"
)

func copyCommand(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	f := flags("copy", "用法：sshm copy 主机 --upload 本地文件 --remote 远端路径\n      sshm copy 主机 --download 本地文件 --remote 远端路径\n单文件 SFTP 传输；默认不覆盖。失败可能留下部分目标文件。", stderr)
	path := configFlag(f)
	upload := f.String("upload", "", "要上传的本地普通文件")
	download := f.String("download", "", "下载保存到本地路径")
	remote := f.String("remote", "", "远端完整文件路径，原样传递，不展开 ~ 或 shell 变量")
	overwrite := f.Bool("overwrite", false, "允许覆盖目标文件（开始写入时截断）")
	timeout := f.Duration("timeout", 2*time.Minute, "连接和传输的总期限，0 不限时")
	if err := parse(f, args); err != nil {
		return parseError(err, stderr)
	}
	if f.NArg() != 1 || !validHost(f.Arg(0)) || (*upload == "") == (*download == "") || *remote == "" || strings.ContainsRune(*remote, 0) || *timeout < 0 {
		return parseError(errors.New("需要一个主机、--remote 和 upload/download 中的一项"), stderr)
	}
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
	var source *os.File
	if *upload != "" {
		source, err = os.OpenFile(expand(*upload), os.O_RDONLY|syscall.O_NONBLOCK, 0)
		if err != nil {
			return localFailure(ctx, stderr, errors.New("无法打开上传文件"), false)
		}
		defer source.Close()
		info, e := source.Stat()
		if e != nil || !info.Mode().IsRegular() {
			return parseError(errors.New("上传源必须是普通文件"), stderr)
		}
	}
	b, err := connect(ctx, cfg, f.Arg(0))
	if err != nil {
		return localFailure(ctx, stderr, err, false)
	}
	defer b.Close()
	stop := context.AfterFunc(ctx, func() { b.Close() })
	defer stop()
	c, err := sftp.NewClient(b.client, sftp.MaxConcurrentRequestsPerFile(16), sftp.UseConcurrentWrites(true))
	if err != nil {
		return localFailure(ctx, stderr, errors.New("无法启动 SFTP；远端必须提供该子系统"), false)
	}
	defer c.Close()
	var n int64
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if *overwrite {
		flags = os.O_WRONLY | os.O_CREATE | os.O_TRUNC
	}
	if source != nil {
		// Reject a known symlink before explicit overwrite; the remote filesystem
		// can still race this check, so paths must be within the authorized scope.
		if *overwrite {
			if info, e := c.Lstat(*remote); e == nil && info.Mode()&os.ModeSymlink != 0 {
				return localFailure(ctx, stderr, errors.New("目标是符号链接，拒绝覆盖"), false)
			}
		}
		dst, e := c.OpenFile(*remote, flags)
		if e != nil {
			return localFailure(ctx, stderr, errors.New("无法创建远端目标；检查路径、权限或目标是否已存在"), true)
		}
		n, err = io.Copy(dst, source)
		closeErr := dst.Close()
		if err == nil {
			err = closeErr
		}
	} else {
		src, e := c.Open(*remote)
		if e != nil {
			return localFailure(ctx, stderr, errors.New("无法打开远端文件"), false)
		}
		defer src.Close()
		info, e := src.Stat()
		if e != nil || !info.Mode().IsRegular() {
			return localFailure(ctx, stderr, errors.New("远端源必须是普通文件"), false)
		}
		dst, e := os.OpenFile(expand(*download), flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK, 0600)
		if e != nil {
			return localFailure(ctx, stderr, errors.New("无法创建本地目标；检查路径、权限或目标是否已存在"), false)
		}
		info, e = dst.Stat()
		if e != nil || !info.Mode().IsRegular() {
			dst.Close()
			return localFailure(ctx, stderr, errors.New("本地目标必须是普通文件"), false)
		}
		n, err = io.Copy(dst, src)
		closeErr := dst.Close()
		if err == nil {
			err = closeErr
		}
	}
	if ctx.Err() != nil {
		return localFailure(ctx, stderr, ctx.Err(), true)
	}
	if err != nil {
		return localFailure(ctx, stderr, errors.New("文件传输未完成，目标可能包含部分内容"), true)
	}
	fmt.Fprintf(stdout, "已传输 %d 字节\n", n)
	return 0
}
