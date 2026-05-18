package config

type Config struct {
	DiscordToken           string
	SpotifyClientID        string
	SpotifyClientSecret    string
	DataDir                string
	BotStatus              string // online/dnd/idle
	BotActivity            string
	EnableSponsorBlock     bool
	SponsorBlockTimeoutMin int
	RegisterCommandsOnBot  bool
	YouTubePOToken         string
	YouTubeCookiesPath     string
}
