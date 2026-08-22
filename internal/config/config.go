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

func LoadConfig() (*Config, error) {
	dataDir := getenv("DATA_DIR", "./data")

	cfg := &Config{
		DiscordToken:        os.Getenv("DISCORD_TOKEN"),
		SpotifyClientID:     os.Getenv("SPOTIFY_CLIENT_ID"),
		SpotifyClientSecret: os.Getenv("SPOTIFY_CLIENT_SECRET"),
		DataDir:             dataDir,
		BotStatus:           getenv("BOT_STATUS", "online"),
		BotActivity:         getenv("BOT_ACTIVITY", "music"),
		EnableSponsorBlock:  getenv("ENABLE_SPONSORBLOCK", "false") == "true",
		SponsorBlockTimeoutMin: func() int {
			i, _ := strconv.Atoi(getenv("SPONSORBLOCK_TIMEOUT", "5"))
			return i
		}(),
		RegisterCommandsOnBot: getenv("REGISTER_COMMANDS_ON_BOT", "false") == "true",
		YouTubePOToken:        getenv("YOUTUBE_PO_TOKEN", ""),
		YouTubeCookiesPath:    getenv("YOUTUBE_COOKIES_PATH", filepath.Join(dataDir, "cookies.txt")),
		HTTPUserAgent:         getenv("HTTP_USER_AGENT", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/131.0.0.0 Safari/537.36"),
	}

	if cfg.DiscordToken == "" {
		return nil, ErrConfig("DISCORD_TOKEN required")
	}
	_ = os.MkdirAll(cfg.DataDir, 0o755)
	return cfg, nil
}

type ErrConfig string

func (e ErrConfig) Error() string { return string(e) }
