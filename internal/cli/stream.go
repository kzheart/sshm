package cli

import (
	"context"
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

type limitedWriter struct {
	out                     io.Writer
	limit, written, dropped int64
	err                     error
	abort                   func()
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	total := len(p)
	keep := total
	if w.limit > 0 && int64(keep) > w.limit-w.written {
		keep = int(w.limit - w.written)
	}
	if keep == 0 {
		w.dropped += int64(total)
		return total, nil
	}
	n, err := w.out.Write(p[:keep])
	w.written += int64(n)
	if err == nil && n != keep {
		err = io.ErrShortWrite
	}
	if err != nil {
		w.err = err
		if w.abort != nil {
			w.abort()
		}
		return n, err
	}
	w.dropped += int64(total - keep)
	return total, nil
}

// Poll avoids a goroutine permanently blocked on a caller-owned stdin pipe.
// Do not close the caller's fd or change its shared O_NONBLOCK state.
func waitFD(ctx context.Context, fd int, event int16) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		fds := []unix.PollFd{{Fd: int32(fd), Events: event}}
		n, err := unix.Poll(fds, 50)
		if errors.Is(err, unix.EINTR) {
			continue
		}
		if err != nil {
			return err
		}
		if n > 0 {
			return nil
		}
	}
}

type contextReader struct {
	ctx  context.Context
	file *os.File
}

func (r *contextReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	fd := int(r.file.Fd())
	if err := waitFD(r.ctx, fd, unix.POLLIN); err != nil {
		return 0, err
	}
	n, err := unix.Read(fd, p)
	if n == 0 && err == nil {
		return 0, io.EOF
	}
	if n < 0 {
		n = 0
	}
	return n, err
}

type contextFileWriter struct {
	ctx     context.Context
	file    *os.File
	regular bool
}

func (w *contextFileWriter) Write(p []byte) (int, error) {
	n := 0
	for len(p) > 0 {
		if err := w.ctx.Err(); err != nil {
			return n, err
		}
		size := len(p)
		if !w.regular {
			if err := waitFD(w.ctx, int(w.file.Fd()), unix.POLLOUT); err != nil {
				return n, err
			}
			if size > 512 {
				size = 512
			}
		}
		k, err := w.file.Write(p[:size])
		n += k
		p = p[k:]
		if err != nil {
			return n, err
		}
		if k == 0 {
			return n, io.ErrShortWrite
		}
	}
	return n, nil
}
func contextOutput(ctx context.Context, w io.Writer) io.Writer {
	if f, ok := w.(*os.File); ok {
		info, err := f.Stat()
		if err == nil {
			// /dev/null cannot backpressure. Treating it like a terminal generated
			// a poll + write syscall for every 512 bytes in throughput measurements.
			fast := info.Mode().IsRegular()
			if null, e := os.Stat(os.DevNull); e == nil && os.SameFile(info, null) {
				fast = true
			}
			return &contextFileWriter{ctx: ctx, file: f, regular: fast}
		}
	}
	return w
}
