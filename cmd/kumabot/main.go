package main

import (
	"context"
	"log"
	"os"
	"os/signal"
	"syscall"

	_ "github.com/mattn/go-sqlite3"
	"github.com/sonroyaalmerol/kumabot/internal/cache"
	"github.com/sonroyaalmerol/kumabot/internal/config"
	"github.com/sonroyaalmerol/kumabot/internal/handlers"
	"github.com/sonroyaalmerol/kumabot/internal/repository"
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

	if err := bot.Run(ctx); err != nil {
		log.Fatal(err)
	}
}
