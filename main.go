package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"sshm/internal/cli"
)

var version = "0.1.0"

func main() {
	// Let output errors reach our process cleanup instead of dying immediately
	// on a closed consumer pipe (for example `sshm ... | head`).
	signal.Ignore(syscall.SIGPIPE)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM, syscall.SIGHUP)
	code := cli.Run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version)
	stop()
	os.Exit(code)
}
