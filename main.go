package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"sshm/internal/cli"
)

var version = "0.3.0"

func main() {
	// Let output errors reach our connection cleanup instead of dying immediately
	// on a closed consumer pipe (for example `sshm ... | head`).
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	code := cli.Main(ctx, os.Args[1:], version)
	stop()
	os.Exit(code)
}
