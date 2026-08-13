package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"doppels.so/cli/internal/command"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	app := command.New()
	app.Context = ctx
	os.Exit(app.Run(os.Args[1:]))
}
