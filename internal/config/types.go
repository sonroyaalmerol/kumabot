package config

type Config struct {
	DiscordToken           string
	SpotifyClientID        string
	SpotifyClientSecret    string
	YoutubeDLPath          string // yt-dlp path (default: yt-dlp)
	FFmpegPath             string // ffmpeg path (default: ffmpeg)
	DataDir                string
	CacheDir               string
	CacheLimitBytes        int64
	BotStatus              string // online/dnd/idle
	BotActivity            string
	EnableSponsorBlock     bool
	SponsorBlockTimeoutMin int
	RegisterCommandsOnBot  bool
}
