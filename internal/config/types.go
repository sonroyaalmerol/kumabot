package config

type Config struct {
	DiscordToken           string
	SpotifyClientID        string
	SpotifyClientSecret    string
	DataDir                string
	CacheDir               string
	CacheLimitBytes        int64
	BotStatus              string // online/dnd/idle
	BotActivity            string
	EnableSponsorBlock     bool
	SponsorBlockTimeoutMin int
	RegisterCommandsOnBot  bool
	YouTubePOToken 		   string
}
