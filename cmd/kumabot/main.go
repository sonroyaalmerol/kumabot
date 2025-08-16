package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/lrstanley/go-ytdlp"
	"github.com/sonroyaalmerol/kumabot/internal/cache"
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
	cache := cache.NewFileCache(cfg, repo)
	bot := handlers.NewBot(cfg, repo, cache)

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	ytdlp.MustInstall(ctx, nil)
	ytdlp.MustInstallFFprobe(ctx, nil)
	ytdlp.MustInstallFFmpeg(ctx, nil)

	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
