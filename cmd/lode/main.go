package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/redthing1/lode/internal/cli"
)

var version = "dev"

func main() {
	os.Exit(run())
}

func run() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return cli.RunContext(ctx, os.Args[1:], os.Stdout, os.Stderr, version)
}
