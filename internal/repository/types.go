package repository

import "database/sql"

type Repo struct {
	db *sql.DB
}

type Settings struct {
	GuildID               string
	PlaylistLimit         int
	SecondsWaitAfterEmpty int
	LeaveIfNoListeners    bool
	QAddEphemeral         bool
	AutoAnnounceNext      bool
	DefaultVolume         int
	DefaultQueuePageSize  int
	TurnDownWhenSpeaking  bool
	TurnDownTarget        int
}

type Favorite struct {
	ID      int64
	GuildID string
	Author  string
	Name    string
	Query   string
}

type PlayHistoryEntry struct {
	ID              int64
	GuildID         string
	UserID          string
	VideoID         string
	Title           string
	Artist          string
	Thumbnail       string
	DurationSeconds int
	PlayedAt        int64
}

type TopSong struct {
	Title     string
	Artist    string
	VideoID   string
	PlayCount int
}

type TopArtist struct {
	Artist    string
	PlayCount int
}

type TopUser struct {
	UserID    string
	PlayCount int
}

type HourlyCount struct {
	Hour  int
	Count int
}

type DailyCount struct {
	Day   string
	Count int
}

type WrappedStats struct {
	TotalPlays    int
	TotalDuration int
	TopSongs      []TopSong
	TopArtists    []TopArtist
	TopUsers      []TopUser
	PeakHour      int
	PeakDay       string
	HourlyCounts  []HourlyCount
	DailyCounts   []DailyCount
}

type GuildState struct {
	GuildID     string
	RadioMode   bool
	ShuffleMode bool
	LoopSong    bool
	LoopQueue   bool
	VoiceChanID string
	TextChanID  string
}

type QueuedSong struct {
	ID             int64
	GuildID        string
	Position       int
	VideoID        string
	Title          string
	Artist         string
	Thumbnail      string
	URL            string
	Source         int
	RequestedBy    string
	Length         int
	Offset         int
	IsLive         bool
	PlaylistTitle  string
	PlaylistSource string
}
