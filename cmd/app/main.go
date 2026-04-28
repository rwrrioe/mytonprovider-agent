package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/rwrrioe/mytonprovider-agent/internal/app"
	"github.com/rwrrioe/mytonprovider-agent/internal/config"
)

func main() {
	os.Exit(run())
}

func run() int {
	cfg := config.MustLoad()

	logger := setupConfig(cfg)
	logger.Info("starting agent")

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	app, err := app.New(ctx, cfg, logger)
	if err != nil {
		logger.Error("init failed")
		return 1
	}
	defer app.Close()

	app.MustRun(ctx)
	return 0
}

func setupConfig(cfg *config.Config) *slog.Logger {
	logLevel := slog.LevelInfo
	if l, ok := config.LogLevels[cfg.System.LogLevel]; ok {
		logLevel = l
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: logLevel}))
	return logger
}
