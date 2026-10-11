// Command token-router serves the configured OpenAI API proxy.
package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"token-router/src/internal/app"
	"token-router/src/internal/cli"
)

func main() {
	os.Exit(execute())
}

func execute() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	command := cli.NewCommand(app.Run)
	if err := command.ExecuteContext(ctx); err != nil {
		slog.New(slog.NewJSONHandler(os.Stderr, nil)).Error("command failed", "error", err.Error())
		return 1
	}
	return 0
}
