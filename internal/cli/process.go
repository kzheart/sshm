package cli

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"time"
)

// A stream limiter drains discarded bytes, so truncation neither buffers the
// rest in memory nor blocks the remote command. One instance per output stream.
type limitedWriter struct {
	out            io.Writer
	limit, written int64
	dropped        int64
	err            error
	abort          func()
}

func (w *limitedWriter) Write(p []byte) (int, error) {
	total := len(p)
	keep := total
	if w.limit > 0 && int64(keep) > w.limit-w.written {
		keep = int(w.limit - w.written)
	}
	n, err := w.out.Write(p[:keep])
	w.written += int64(n)
	if err != nil {
		w.err = err
		if w.abort != nil {
			w.abort()
		}
		return n, err
	}
	if n != keep {
		w.err = io.ErrShortWrite
		if w.abort != nil {
			w.abort()
		}
		return n, io.ErrShortWrite
	}
	w.dropped += int64(total - keep)
	return total, nil
}

func command(ctx context.Context, ssh string, args []string, stdin *os.File, stdout, stderr io.Writer) *exec.Cmd {
	c := exec.CommandContext(ctx, ssh, args...)
	if stdin != nil {
		c.Stdin = stdin
	}
	c.Stdout, c.Stderr = stdout, stderr
	c.WaitDelay = 500 * time.Millisecond
	configureProcess(c)
	return c
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var e *exec.ExitError
	if errors.As(err, &e) && e.ExitCode() >= 0 {
		return e.ExitCode()
	}
	return 125
}
