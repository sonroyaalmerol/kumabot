package config

import (
	"os"
	"path/filepath"
	"strconv"
)

func getenv(key, def string) string {
	val := os.Getenv(key)
	if val == "" {
		return def
	}
	return val
}

func mustAtoi64(s string) int64 {
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return v
}

func LoadConfig() (*Config, error) {
	dataDir := getenv("DATA_DIR", "./data")
	cacheDir := filepath.Join(dataDir, "cache")

	// CACHE_LIMIT supports simple suffix-less bytes number; you can extend to parse 2GB, etc.
	cacheLimit := getenv("CACHE_LIMIT", "2147483648") // default 2GB
	cfg := &Config{
		DiscordToken:        os.Getenv("DISCORD_TOKEN"),
		SpotifyClientID:     os.Getenv("SPOTIFY_CLIENT_ID"),
		SpotifyClientSecret: os.Getenv("SPOTIFY_CLIENT_SECRET"),
		DataDir:             dataDir,
		CacheDir:            cacheDir,
		CacheLimitBytes:     mustAtoi64(cacheLimit),
		BotStatus:           getenv("BOT_STATUS", "online"),
		BotActivity:         getenv("BOT_ACTIVITY", "music"),
		EnableSponsorBlock:  getenv("ENABLE_SPONSORBLOCK", "false") == "true",
		SponsorBlockTimeoutMin: func() int {
			i, _ := strconv.Atoi(getenv("SPONSORBLOCK_TIMEOUT", "5"))
			return i
		}(),
		RegisterCommandsOnBot: getenv("REGISTER_COMMANDS_ON_BOT", "false") == "true",
	}

	if cfg.DiscordToken == "" {
		return nil, ErrConfig("DISCORD_TOKEN required")
	}
	_ = os.MkdirAll(cfg.DataDir, 0o755)
	_ = os.MkdirAll(cfg.CacheDir, 0o755)
	_ = os.MkdirAll(filepath.Join(cfg.CacheDir, "tmp"), 0o755)
	return cfg, nil
}

type ErrConfig string

func (e ErrConfig) Error() string { return string(e) }
