package main

import (
	"context"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/lrstanley/go-ytdlp"
	"github.com/sonroyaalmerol/kumabot/internal/config"
	"github.com/sonroyaalmerol/kumabot/internal/handlers"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
	_ "modernc.org/sqlite"
)

func main() {
	cfg, err := config.LoadConfig()
	if err != nil {
		log.Fatal(err)
	}
	db, err := repository.OpenDB(cfg)
	if err != nil {
		log.Fatal(err)
	}
	repo := repository.NewRepo(db)
	bot := handlers.NewBot(cfg, repo)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	slog.Info("installing dependencies", "ytdlp", true)

	ytdlp.MustInstall(ctx, nil)

	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
